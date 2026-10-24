// forwarder_regression_test.go - Protocol, retry, lifecycle, and metric regressions.
package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	btresponse "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/observability"
)

func regressionRequest(t *testing.T) tracker.Request {
	t.Helper()
	req, err := tracker.MakeRequest("203.0.113.10", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "100", "", "200", "started", "review", "1", "0", nil)
	if err != nil {
		t.Fatal(err)
	}
	return *req
}
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestForwarderPreservesExistingQuery(t *testing.T) {
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	req := regressionRequest(t)
	req.InfoHash = btcommon.InfoHash("\x00\xff" + strings.Repeat("h", 18))
	built := fm.buildAnnounceURI(corecommon.Forward{Uri: "https://tracker.example/announce?passkey=secret%2B%26%3D&info_hash=old&event=stopped"}, req, "started")
	u, err := url.Parse(built)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("info_hash") != string(req.InfoHash) || u.Query().Get("passkey") != "secret+&=" || u.Query().Get("event") != "started" {
		t.Fatalf("forwarder query corrupted: %s", built)
	}
}

func TestZeroRateLimitThresholdDisablesLimiting(t *testing.T) {
	cfg := makeCfg()
	cfg.QueueRateLimitThreshold = 0
	fm := NewForwarderManager(cfg, NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	for range 8 {
		fm.jobQueue <- AnnounceJob{}
	}
	if fm.shouldRateLimitInitial() {
		t.Fatal("disabled rate limiting activated")
	}
}

func TestInvalidPeerPortsRejected(t *testing.T) {
	for _, port := range []string{"-1", "0", "65536"} {
		req, err := tracker.MakeRequest("203.0.113.10", strings.Repeat("h", 20), strings.Repeat("p", 20), port, "0", "0", "100", "", "1", "started", "review", "1", "0", nil)
		if err == nil || req != nil {
			t.Fatalf("invalid port %s accepted", port)
		}
	}
}

func TestPeerExpiryClearsForwarderCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req := regressionRequest(t)
		cfg := makeCfg()
		cfg.Age = 1
		cfg.Forwards = []corecommon.Forward{{Name: "upstream", Uri: "udp://8.8.8.8:6969/announce"}}
		core := NewCore(cfg, nil, discardLogger())
		defer core.Close()
		cache := core.ForwarderStorage
		core.Storage.Update(req)
		cache.UpdatePeers(req.InfoHash, "upstream", []btcommon.Peer{{IP: "8.8.8.8", Port: 6881}}, 60)
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if len(core.Storage.GetPeers(req.InfoHash)) != 0 {
			t.Fatal("local peer did not expire")
		}
		if cache.HasInfoHash(req.InfoHash) {
			t.Fatal("expired local hash retained its upstream cache")
		}
	})
}

type regressionRoundTripper func(*http.Request) (*http.Response, error)

func (f regressionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLongRetryDefersAnnounce(t *testing.T) {
	req := regressionRequest(t)
	cache := NewForwarderStorage()
	response := btresponse.NewFailure("busy")
	response.RetryIn = 60
	wire, err := response.Bencode(false)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: regressionRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}, nil
	})}
	fm := NewForwarderManager(makeCfg(), cache, nil, nil, nil, WithHTTPClient(client), WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	fwd := corecommon.Forward{Name: "upstream", Uri: "https://tracker.example/announce"}
	fm.executeAnnounce(AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, Request: req, ForwarderName: "upstream", Forwarder: fwd})
	if cache.ShouldAnnounceNow(req.InfoHash, "upstream", time.Now().Add(59*time.Minute)) {
		t.Fatal("upstream cooldown was shortened")
	}
	if len(fm.scheduledJobs) != 1 {
		t.Fatal("long retry was not scheduled")
	}
}

func newRegressionUDPTracker(t *testing.T, wrongError, invalidCookie bool) (corecommon.Forward, <-chan int32) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	numwant := make(chan int32, 1)
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		cookieSent := false
		for {
			n, addr, readErr := conn.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			if n < 16 {
				continue
			}
			action := binary.BigEndian.Uint32(buf[8:12])
			tx := binary.BigEndian.Uint32(buf[12:16])
			if action == 0 {
				out := make([]byte, 16)
				binary.BigEndian.PutUint32(out[4:8], tx)
				binary.BigEndian.PutUint64(out[8:16], 12345)
				_, _ = conn.WriteToUDP(out, addr)
				continue
			}
			if action == 1 && n >= 98 {
				select {
				case numwant <- int32(binary.BigEndian.Uint32(buf[92:96])):
				default:
				}
				if invalidCookie && !cookieSent {
					cookieSent = true
					out := make([]byte, 8+len("invalid connection id"))
					binary.BigEndian.PutUint32(out[0:4], 3)
					binary.BigEndian.PutUint32(out[4:8], tx)
					copy(out[8:], "invalid connection id")
					_, _ = conn.WriteToUDP(out, addr)
					continue
				}
				if wrongError {
					out := make([]byte, 12)
					binary.BigEndian.PutUint32(out[0:4], 3)
					binary.BigEndian.PutUint32(out[4:8], tx+1)
					copy(out[8:], "busy")
					_, _ = conn.WriteToUDP(out, addr)
				}
				out := make([]byte, 20)
				binary.BigEndian.PutUint32(out[0:4], 1)
				binary.BigEndian.PutUint32(out[4:8], tx)
				binary.BigEndian.PutUint32(out[8:12], 1800)
				_, _ = conn.WriteToUDP(out, addr)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	return corecommon.Forward{Uri: fmt.Sprintf("udp://%s/announce", conn.LocalAddr())}, numwant
}

func TestUDPAnnounceIgnoresUnmatchedErrors(t *testing.T) {
	fwd, _ := newRegressionUDPTracker(t, true, false)
	uf := NewUDPForwarder(false, 1, 1, 1, discardLogger())
	defer uf.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := uf.AnnounceContext(ctx, fwd, regressionRequest(t))
	if err != nil {
		t.Fatalf("unmatched error aborted announce: %v", err)
	}
}

func TestUDPForwarderPreservesMaxNumWant(t *testing.T) {
	fwd, got := newRegressionUDPTracker(t, false, false)
	uf := NewUDPForwarder(false, 1, 1, 1, discardLogger())
	defer uf.Stop()
	if _, _, err := uf.Announce(fwd, regressionRequest(t)); err != nil {
		t.Fatal(err)
	}
	if n := <-got; n != 200 {
		t.Fatalf("numwant changed: %d", n)
	}
}

func TestHTTPResponseMetricCountsOnce(t *testing.T) {
	req := regressionRequest(t)
	payload, err := (&btresponse.Response{Interval: 1800}).Bencode(true)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: regressionRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	registry := prometheus.NewRegistry()
	metrics, err := observability.NewPrometheus(registry)
	if err != nil {
		t.Fatal(err)
	}
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, metrics, nil, WithHTTPClient(client), WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	fwd := corecommon.Forward{Name: "upstream", Uri: "https://tracker.example/announce"}
	fm.executeAnnounce(AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, Request: req, ForwarderName: "upstream", Forwarder: fwd})
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "forwarder_status" {
			if got := family.Metric[0].Counter.GetValue(); got != 1 {
				t.Fatalf("HTTP response counted %v times", got)
			}
			return
		}
	}
	t.Fatal("forwarder status metric missing")
}

func TestUDPLimitsAfterFilteringAddressFamily(t *testing.T) {
	cfg := makeCfg()
	core := NewCore(cfg, nil, discardLogger())
	defer core.Close()
	cache := NewForwarderStorage()
	req := regressionRequest(t)
	peers := make([]btcommon.Peer, 0, 100)
	for i := range 50 {
		peers = append(peers, btcommon.Peer{IP: btcommon.Address(fmt.Sprintf("2001:4860::%x", i+1)), Port: 6881})
	}
	for i := range 50 {
		peers = append(peers, btcommon.Peer{IP: btcommon.Address(fmt.Sprintf("8.8.4.%d", i+1)), Port: 6881})
	}
	cache.UpdatePeers(req.InfoHash, "upstream", peers, 1800)
	receiver := NewReceiverUDP(cfg, core.Storage, cache, nil, discardLogger())
	defer receiver.Close()
	response, failure := receiver.ProcessAnnounce("203.0.113.10", string(req.InfoHash), string(req.PeerID), "6881", "0", "0", "100", "", "50", "started", "", "", "")
	if failure != "" {
		t.Fatal(failure)
	}
	if len(response.Peers) != 50 {
		t.Fatalf("expected 50 prefiltered peers, got %d", len(response.Peers))
	}
	for _, peer := range response.Peers {
		if _, err := peer.IP.IPv4(); err != nil {
			t.Fatal("IPv4 response contains an incompatible peer")
		}
	}
	if err := receiver.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	receiver.sendAnnounceResponse(client.LocalAddr().(*net.UDPAddr), 1, response)
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 20+50*6 {
		t.Fatalf("expected 50 IPv4 peers, got %d bytes", n)
	}
}

func TestIPv6UDPSelectsNativeIPv6Peers(t *testing.T) {
	cfg := makeCfg()
	core := NewCore(cfg, nil, discardLogger())
	defer core.Close()
	cache := NewForwarderStorage()
	req := regressionRequest(t)
	peers := make([]btcommon.Peer, 0, 100)
	for i := range 50 {
		peers = append(peers, btcommon.Peer{IP: btcommon.Address(fmt.Sprintf("8.8.4.%d", i+1)), Port: 6881})
	}
	for i := range 50 {
		peers = append(peers, btcommon.Peer{IP: btcommon.Address(fmt.Sprintf("2001:4860::%x", i+1)), Port: 6881})
	}
	cache.UpdatePeers(req.InfoHash, "upstream", peers, 1800)
	receiver := NewReceiverUDP(cfg, core.Storage, cache, nil, discardLogger())
	defer receiver.Close()
	response, failure := receiver.ProcessAnnounce("2001:4860::9999", string(req.InfoHash), string(req.PeerID), "6881", "0", "0", "100", "", "50", "started", "", "", "")
	if failure != "" || len(response.Peers) != 50 {
		t.Fatalf("bad IPv6 response: %v, failure=%s", response, failure)
	}
	for _, peer := range response.Peers {
		if ip := net.ParseIP(string(peer.IP)); ip == nil || ip.To4() != nil {
			t.Fatal("IPv6 reply selected an IPv4 or invalid endpoint")
		}
	}
}

func TestUDPCookieRefreshDoesNotConsumeOnlyAttempt(t *testing.T) {
	forwarder, _ := newRegressionUDPTracker(t, false, true)
	client := NewUDPForwarder(false, 1, 1, 1, discardLogger())
	defer client.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, _, err := client.AnnounceContext(ctx, forwarder, regressionRequest(t))
	if err != nil || response.Interval != 1800 {
		t.Fatalf("cookie refresh lost announce attempt: response=%v, err=%v", response, err)
	}
}
