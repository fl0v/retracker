package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	btresponse "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/config"
)

func makeIntegrationCfg() *config.Config {
	return &config.Config{
		AnnounceInterval:        1800,
		ForwardTimeout:          1,
		ForwarderWorkers:        1,
		MaxForwarderWorkers:     1,
		ForwarderQueueSize:      10,
		ForwarderFailThreshold:  3,
		ForwarderRetryAttempts:  1,
		ForwarderRetryBaseMs:    5,
		ForwarderSuspendSeconds: 60,
	}
}

func newIntegrationHarness(t *testing.T, cfg *config.Config, forwarder corecommon.Forward, startManager bool) (*ReceiverAnnounce, *ForwarderManager, *ForwarderStorage, func()) {
	t.Helper()

	cfg.Forwards = []corecommon.Forward{forwarder}
	forwarderStorage := NewForwarderStorage()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, forwarderStorage, mainStorage, nil, nil)
	activateForwarderForTest(fm, forwarder)

	announceReceiver := &ReceiverAnnounce{
		Config:           cfg,
		Storage:          mainStorage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: fm,
	}

	if startManager {
		fm.Start()
	}

	cleanup := func() {
		if startManager {
			fm.Stop()
		}
	}

	return announceReceiver, fm, forwarderStorage, cleanup
}

// activateForwarderForTest restores a forwarder into active state after manager
// initialization. This is required for localhost-backed httptest servers because
// production DNS hardening removes localhost/private forwarders at startup.
func activateForwarderForTest(fm *ForwarderManager, forwarder corecommon.Forward) {
	name := forwarder.GetName()

	fm.pendingMu.Lock()
	delete(fm.disabledForwarders, name)
	delete(fm.failCounts, name)
	fm.pendingMu.Unlock()

	fm.forwardersMu.Lock()
	found := false
	for _, existing := range fm.Forwarders {
		if existing.GetName() == name {
			found = true
			break
		}
	}
	if !found {
		fm.Forwarders = append(fm.Forwarders, forwarder)
	}
	fm.forwardersMu.Unlock()

	fm.statsMu.Lock()
	if _, ok := fm.stats[name]; !ok {
		fm.stats[name] = &ForwarderStats{SampleCount: 0}
	}
	fm.statsMu.Unlock()
}

func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for condition: %s", description)
}

func canUseLocalTCP() bool {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func TestForwarderIntegrationCycle(t *testing.T) {
	if !canUseLocalTCP() {
		t.Skip("local TCP listeners are not permitted in this environment")
	}

	const (
		infoHash = "12345678901234567890"
		peerID   = "ABCDEFGHIJKLMNOPQRST"
	)

	t.Run("happy path", func(t *testing.T) {
		successServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/announce" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			payload, err := (&btresponse.Response{
				Interval: 1800,
				Peers: []btcommon.Peer{
					{
						IP:     btcommon.Address("203.0.113.99"),
						Port:   6881,
						PeerID: btcommon.PeerID("ZYXWVUTSRQPONMLKJIHG"),
					},
				},
			}).Bencode(false)
			if err != nil {
				t.Fatalf("bencode response: %v", err)
			}
			_, _ = w.Write([]byte(payload))
		}))
		defer successServer.Close()

		cfg := makeIntegrationCfg()
		forwarder := corecommon.Forward{
			Name: "happy-forwarder",
			Uri:  fmt.Sprintf("%s/announce", successServer.URL),
		}
		receiver, fm, fwdStorage, cleanup := newIntegrationHarness(t, cfg, forwarder, true)
		defer cleanup()

		resp, failure := receiver.ProcessAnnounce("198.51.100.10", infoHash, peerID, "51413", "0", "0", "100", "", "50", "started", "integration-test", "", "")
		if failure != "" {
			t.Fatalf("unexpected announce failure: %s", failure)
		}
		if resp == nil {
			t.Fatalf("expected non-nil announce response")
		}

		waitForCondition(t, 2*time.Second, "forwarder success to be stored", func() bool {
			fwdStorage.mu.RLock()
			defer fwdStorage.mu.RUnlock()

			entries, ok := fwdStorage.Entries[btcommon.InfoHash(infoHash)]
			if !ok {
				return false
			}
			entry, ok := entries[forwarder.GetName()]
			if !ok {
				return false
			}
			return len(entry.Peers) == 1
		})

		if fm.isDisabled(forwarder.GetName()) {
			t.Fatalf("forwarder should not be disabled in happy path")
		}
		if fm.isSuspended(forwarder.GetName()) {
			t.Fatalf("forwarder should not be suspended in happy path")
		}
	})

	t.Run("suspend error", func(t *testing.T) {
		tooManyReqServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("too many requests"))
		}))
		defer tooManyReqServer.Close()

		cfg := makeIntegrationCfg()
		forwarder := corecommon.Forward{
			Name: "suspend-forwarder",
			Uri:  fmt.Sprintf("%s/announce", tooManyReqServer.URL),
		}
		receiver, fm, _, cleanup := newIntegrationHarness(t, cfg, forwarder, true)
		defer cleanup()

		resp, failure := receiver.ProcessAnnounce("198.51.100.11", infoHash, peerID, "51413", "0", "0", "100", "", "50", "started", "integration-test", "", "")
		if failure != "" {
			t.Fatalf("unexpected announce failure: %s", failure)
		}
		if resp == nil {
			t.Fatalf("expected non-nil announce response")
		}

		waitForCondition(t, 2*time.Second, "forwarder to become suspended", func() bool {
			return fm.isSuspended(forwarder.GetName())
		})

		if fm.isDisabled(forwarder.GetName()) {
			t.Fatalf("forwarder should not be disabled for 429 suspension path")
		}
	})

	t.Run("permanent error", func(t *testing.T) {
		cfg := makeIntegrationCfg()
		forwarder := corecommon.Forward{
			Name: "permanent-forwarder",
			Uri:  "http://no-such-forwarder-host.invalid:8080/announce",
		}
		receiver, fm, _, cleanup := newIntegrationHarness(t, cfg, forwarder, true)
		defer cleanup()

		resp, failure := receiver.ProcessAnnounce("198.51.100.12", infoHash, peerID, "51413", "0", "0", "100", "", "50", "started", "integration-test", "", "")
		if failure != "" {
			t.Fatalf("unexpected announce failure: %s", failure)
		}
		if resp == nil {
			t.Fatalf("expected non-nil announce response")
		}

		waitForCondition(t, 3*time.Second, "forwarder to become disabled", func() bool {
			return fm.isDisabled(forwarder.GetName())
		})

		if fm.isSuspended(forwarder.GetName()) {
			t.Fatalf("forwarder should not be suspended in permanent error path")
		}

		fm.pendingMu.Lock()
		disabled := fm.disabledForwarders[forwarder.GetName()]
		fm.pendingMu.Unlock()
		if disabled == nil {
			t.Fatalf("disabled forwarder details should be present")
		}
		reasonLower := strings.ToLower(disabled.Reason)
		if !strings.Contains(reasonLower, "lookup no-such-forwarder") {
			t.Fatalf("expected disable reason to include lookup host prefix, got: %q", disabled.Reason)
		}
	})

	t.Run("queue full gives retry error", func(t *testing.T) {
		cfg := makeIntegrationCfg()
		cfg.ForwarderQueueSize = 1

		forwarder := corecommon.Forward{
			Name: "queue-full-forwarder",
			Uri:  "http://example.com/announce",
		}
		receiver, fm, _, cleanup := newIntegrationHarness(t, cfg, forwarder, false)
		defer cleanup()

		// Disable rate-limit gate for this scenario, so we specifically test queue-full handling.
		fm.queueRateLimitThresh = 0

		// Fill queue to capacity before announce.
		fm.jobQueue <- AnnounceJob{}

		resp, failure := receiver.ProcessAnnounce("198.51.100.13", infoHash, peerID, "51413", "0", "0", "100", "", "50", "started", "integration-test", "", "")
		if failure != "" {
			t.Fatalf("unexpected announce failure: %s", failure)
		}
		if resp == nil {
			t.Fatalf("expected non-nil announce response")
		}

		if resp.FailureReason == "" {
			t.Fatalf("expected queue full failure reason")
		}
		if !strings.Contains(resp.FailureReason, "tracker queue full") {
			t.Fatalf("unexpected failure reason: %q", resp.FailureReason)
		}
		if resp.Interval != cfg.AnnounceInterval {
			t.Fatalf("expected retry interval %d, got %d", cfg.AnnounceInterval, resp.Interval)
		}
		if got := len(fm.jobQueue); got != cap(fm.jobQueue) {
			t.Fatalf("expected queue to remain full, got %d/%d", got, cap(fm.jobQueue))
		}
	})
}
