package server

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/config"
)

func waitForShutdown(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish promptly")
	}
}

func TestBackgroundServicesCloseAndCancelPeerSessions(t *testing.T) {
	cfg := &config.Config{StatsInterval: 1}
	storage := NewStorage(cfg)
	defer storage.Close()
	peerContext := storage.Update(tracker.Request{InfoHash: "hash", PeerID: "peer"})
	temp, err := NewTempStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer temp.Close()
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() { defer wg.Done(); _ = storage.Close(); _ = temp.Close() }()
		}
		wg.Wait()
		close(done)
	}()
	waitForShutdown(t, done)
	if peerContext.Err() == nil {
		t.Fatal("storage close left an active peer session")
	}
}

func TestTempStorageCleanupPreservesRecentFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	recent := filepath.Join(dir, "recent")
	for _, name := range []string{old, recent} {
		if err := os.WriteFile(name, []byte("response"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "directory")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := &TempStorage{path: dir}
	ts.cleanExpired()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old response was not removed: %v", err)
	}
	for _, name := range []string{recent, child} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForwarderStopCancelsHTTPAndJoinsWorkers(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	cfg := makeIntegrationCfg()
	cfg.ForwardTimeout = 60
	cfg.ForwarderRetryAttempts = 5
	ra, fm, _, cleanup := newIntegrationHarness(t, cfg, corecommon.Forward{Uri: upstream.URL}, true)
	defer cleanup()
	announceLifecycleEvent(t, ra, "started")
	waitForShutdown(t, started)
	done := make(chan struct{})
	go func() { fm.Stop(); fm.Stop(); close(done) }()
	waitForShutdown(t, done)
	waitForShutdown(t, canceled)
	if fm.workerCount != 0 || len(fm.pendingJobs) != 0 || len(fm.scheduledJobs) != 0 {
		t.Fatal("stopped manager retained workers or pending jobs")
	}
	if _, err := fm.AddRuntimeForwarder("https://new.example/announce"); err == nil {
		t.Fatal("stopped manager accepted a new forwarder")
	}
	fm.Start()
}

func TestForwarderStopWhileQueueing(t *testing.T) {
	cfg := makeIntegrationCfg()
	storage := &Storage{Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request)}
	fm := NewForwarderManager(cfg, NewForwarderStorage(), storage, nil, nil)
	defer fm.Stop()
	fm.Forwarders = []corecommon.Forward{{Name: "test", Uri: "http://127.0.0.1:1/announce"}}
	req := tracker.Request{InfoHash: "hash", PeerID: "peer"}
	storage.Update(req)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				fm.QueueEligibleAnnounces(req.InfoHash, req)
				fm.processScheduledReadyJob(AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID}, time.Now())
			}
		}()
	}
	close(start)
	fm.Stop()
	wg.Wait()
	if len(fm.jobQueue) != 0 || len(fm.scheduledJobs) != 0 || len(fm.pendingJobs) != 0 {
		t.Fatal("queueing continued after shutdown")
	}
}

func TestUDPForwarderCancellationInterruptsReads(t *testing.T) {
	for _, stage := range []string{"connect", "announce"} {
		for _, stopClient := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/peer", true: "/client"}[stopClient], func(t *testing.T) {
				listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				ready := make(chan struct{})
				serverDone := make(chan struct{})
				go func() {
					defer close(serverDone)
					buffer := make([]byte, 128)
					for {
						n, addr, readErr := listener.ReadFromUDP(buffer)
						if readErr != nil {
							return
						}
						if n < 16 {
							continue
						}
						if stage == "announce" && binary.BigEndian.Uint32(buffer[8:12]) == udpActionConnect {
							response := make([]byte, 16)
							copy(response[4:8], buffer[12:16])
							binary.BigEndian.PutUint64(response[8:], 42)
							_, _ = listener.WriteToUDP(response, addr)
							continue
						}
						close(ready)
						return
					}
				}()
				uf := NewUDPForwarder(false, 60, 3, 30000)
				defer uf.Stop()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				var announceErr error
				go func() {
					_, _, announceErr = uf.AnnounceContext(ctx, corecommon.Forward{Uri: "udp://" + listener.LocalAddr().String()}, tracker.Request{InfoHash: common.InfoHash(strings.Repeat("h", 20)), PeerID: common.PeerID(strings.Repeat("p", 20))})
					close(done)
				}()
				waitForShutdown(t, ready)
				if stopClient {
					uf.Stop()
				} else {
					cancel()
				}
				waitForShutdown(t, done)
				waitForShutdown(t, serverDone)
				if announceErr == nil {
					t.Fatal("canceled UDP announce succeeded")
				}
			})
		}
	}
}

func TestUDPForwarderBackoffIsCancelable(t *testing.T) {
	uf := NewUDPForwarder(false, 60, 3, 30000)
	defer uf.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := uf.waitRetry(ctx, 1); err == nil {
		t.Fatal("canceled retry delay succeeded")
	}
	uf.Stop()
	if _, _, err := uf.Announce(corecommon.Forward{Uri: "udp://127.0.0.1:1"}, tracker.Request{}); err == nil {
		t.Fatal("stopped UDP client accepted another announce")
	}
}

func TestCoreStartsConfiguredForwardersExplicitly(t *testing.T) {
	requested := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		_, _ = w.Write([]byte("d8:intervali1800e5:peers0:e"))
	}))
	defer upstream.Close()
	cfg := makeIntegrationCfg()
	forwarder := corecommon.Forward{Uri: upstream.URL}
	cfg.Forwards = []corecommon.Forward{forwarder}
	core := NewCore(cfg, nil)
	defer core.Close()
	activateForwarderForTest(core.ForwarderManager, forwarder)
	announceLifecycleEvent(t, core.Receiver.Announce, "started")
	if len(core.ForwarderManager.jobQueue) != 1 {
		t.Fatal("configured forwarding started before Core.Start")
	}
	core.Start()
	core.Start()
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("Core.Start did not execute configured forwarding")
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core.Start()
	if core.ForwarderManager.workerCount != 0 {
		t.Fatal("closed core restarted workers")
	}
}

func TestCoreClosePreventsRuntimeInitialization(t *testing.T) {
	core := NewCore(makeIntegrationCfg(), nil)
	defer core.Close()
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core.Start()
	w := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(w, httptest.NewRequest(http.MethodPost, "/add-forwarder?urls=https://tracker.example/announce", nil))
	if w.Code != http.StatusServiceUnavailable || core.ForwarderManager != nil {
		t.Fatal("closed core restarted forwarding")
	}
}
