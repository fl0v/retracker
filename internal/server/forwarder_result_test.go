// forwarder_result_test.go - Result publication stays isolated from slow logging.
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	btresponse "github.com/fl0v/retracker/bittorrent/response"
	corecommon "github.com/fl0v/retracker/common"
)

type blockingLogWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestForwarderLogsDoNotHoldPeerLock(t *testing.T) {
	for _, cancelMode := range []string{"stopped", "expired"} {
		for _, resultKind := range []string{"filtered success", "retryable failure", "permanent failure", "suspended"} {
			t.Run(cancelMode+"/"+resultKind, func(t *testing.T) {
				sink := &blockingLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(sink.release) }) }
				cfg := makeCfg()
				cfg.Age = -1
				core := NewCore(cfg, nil, slog.New(slog.NewTextHandler(sink, nil)))
				t.Cleanup(func() { _ = core.Close() })
				cache := NewForwarderStorage()
				fm := NewForwarderManager(cfg, cache, core.Storage, nil, nil, WithForwarderLogger(core.Log()))
				t.Cleanup(fm.Stop)
				// Keep unrelated incoming requests local; share their storage with this worker.
				core.Receiver.Announce.ForwarderStorage = cache
				req := regressionRequest(t)
				session := core.Storage.Update(req)
				job := AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, Forwarder: corecommon.Forward{Name: "upstream", Uri: "http://tracker.example/announce"}, state: &announceJobState{peerContext: session}}
				result := AnnounceResult{Error: errors.New("upstream failure")}
				switch resultKind {
				case "filtered success":
					result = AnnounceResult{Success: true, Interval: 1800, Peers: []btcommon.Peer{{IP: "127.0.0.1", Port: 6881}, {IP: "8.8.8.8", Port: 6881}}}
				case "permanent failure":
					result.NonRetryable = true
				case "suspended":
					result.Suspend = true
				}
				done := make(chan struct{})
				go func() { fm.handleAnnounceResult(job, result); close(done) }()
				t.Cleanup(func() { release(); waitResult(t, done) })
				waitResult(t, sink.entered)
				incoming := make(chan struct{})
				go func() {
					defer close(incoming)
					query := url.Values{"info_hash": {strings.Repeat("x", 20)}, "peer_id": {strings.Repeat("q", 20)}, "port": {"6881"}, "uploaded": {"0"}, "downloaded": {"0"}, "left": {"100"}}
					w := httptest.NewRecorder()
					core.Receiver.Announce.HTTPHandler(w, httptest.NewRequest(http.MethodGet, "/announce?"+query.Encode(), nil))
					if w.Code != http.StatusOK {
						t.Errorf("HTTP announce returned %d", w.Code)
					}
					_, failure := core.Receiver.UDP.ProcessAnnounce("203.0.113.10", strings.Repeat("y", 20), strings.Repeat("q", 20), "6881", "0", "0", "100", "", "50", "started", "", "", "")
					if failure != "" {
						t.Error(failure)
					}
				}()
				t.Cleanup(func() { release(); waitResult(t, incoming) })
				waitResult(t, incoming)
				canceled := make(chan struct{})
				go func() {
					defer close(canceled)
					if cancelMode == "expired" {
						core.Storage.purgeExpired()
					} else {
						core.Receiver.Announce.handleStoppedEvent(&req, &btresponse.Response{})
					}
				}()
				t.Cleanup(func() { release(); waitResult(t, canceled) })
				waitResult(t, canceled)
				if session.Err() == nil {
					t.Fatal("peer session not canceled")
				}
				core.Storage.Update(req) // A restart must not revive the old result.
				cache.UpdatePeers(req.InfoHash, "upstream", []btcommon.Peer{{IP: "1.1.1.1", Port: 6881}}, 1800)
				release()
				waitResult(t, done)
				peers := cache.GetAllPeers(req.InfoHash)
				if len(peers) != 1 || peers[0].IP != "1.1.1.1" {
					t.Fatalf("old result replaced restarted peer cache: %v", peers)
				}
			})
		}
	}
}

func waitResult(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("operation blocked behind result logging")
	}
}

func TestStoppedJobLoggingReleasesPeerAndPendingLocks(t *testing.T) {
	sink := &blockingLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(sink.release) }) }
	cfg := makeCfg()
	cfg.Debug = true
	core := NewCore(cfg, nil, slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { _ = core.Close() })
	cache := NewForwarderStorage()
	fm := NewForwarderManager(cfg, cache, core.Storage, nil, nil, WithForwarderLogger(core.Log()))
	t.Cleanup(fm.Stop)
	req := regressionRequest(t)
	ctx := core.Storage.Update(req)
	fm.pendingJobs[fm.jobKey(req.InfoHash, "upstream", req.PeerID)] = &announceJobState{peerContext: ctx}
	core.Receiver.Announce.ForwarderManager = fm
	done := make(chan struct{})
	go func() { core.Receiver.Announce.handleStoppedEvent(&req, &btresponse.Response{}); close(done) }()
	t.Cleanup(func() { release(); waitResult(t, done) })
	waitResult(t, sink.entered)
	updated := make(chan struct{})
	go func() {
		core.Storage.Update(req)
		fm.isJobPending(req.InfoHash, "upstream", req.PeerID)
		close(updated)
	}()
	t.Cleanup(func() { release(); waitResult(t, updated) })
	waitResult(t, updated)
	if ctx.Err() == nil {
		t.Fatal("stopped session was not canceled before logging")
	}
}
