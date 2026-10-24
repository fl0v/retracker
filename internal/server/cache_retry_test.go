// cache_retry_test.go - Cache aging and upstream cooldown lifecycle coverage.
package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	corecommon "github.com/fl0v/retracker/common"
)

func TestCachePrunePreservesCooldownAndFreshPeers(t *testing.T) {
	cache := NewForwarderStorage()
	cache.maxPeerAge = time.Minute
	now := time.Now()
	hash := btcommon.InfoHash(strings.Repeat("h", 20))
	cache.UpdatePeers(hash, "old", []btcommon.Peer{{IP: "8.8.8.8", Port: 6881}}, 30)
	cache.UpdatePeers(hash, "deferred", []btcommon.Peer{{IP: "1.1.1.1", Port: 6881}}, 30)
	cache.UpdatePeers(hash, "fresh", []btcommon.Peer{{IP: "9.9.9.9", Port: 6881}}, 30)
	for _, name := range []string{"old", "deferred"} {
		entry := cache.Entries[hash][name]
		entry.LastUpdate = now.Add(-2 * time.Minute)
		entry.NextAnnounce = entry.LastUpdate.Add(30 * time.Second)
		cache.Entries[hash][name] = entry
	}
	until := now.Add(time.Hour)
	cache.DeferAnnounce(hash, "deferred", until)
	cache.DeferAnnounce(hash, "deferred", now.Add(time.Minute))
	if peers := cache.GetAllPeers(hash); len(peers) != 1 || peers[0].IP != "9.9.9.9" {
		t.Fatalf("stale peers returned: %v", peers)
	}
	cache.Prune(now)
	if _, ok := cache.Entries[hash]["old"]; ok {
		t.Fatal("expired entry retained")
	}
	if entry := cache.Entries[hash]["deferred"]; len(entry.Peers) != 0 || !entry.NextAnnounce.Equal(until) {
		t.Fatalf("cooldown lost: %+v", entry)
	}
	cache.Prune(now.Add(2 * time.Hour))
	if cache.HasInfoHash(hash) {
		t.Fatal("expired hash retained after cooldown")
	}
}

func TestRetryOutcomesPreserveSessionAndPendingJob(t *testing.T) {
	for _, value := range []any{1, int64(5), "60"} {
		t.Run("valid", func(t *testing.T) {
			req := regressionRequest(t)
			cfg := makeCfg()
			core := NewCore(cfg, nil, discardLogger())
			defer core.Close()
			cache := NewForwarderStorage()
			fm := NewForwarderManager(cfg, cache, core.Storage, nil, nil, WithForwarderLogger(discardLogger()))
			defer fm.Stop()
			ctx := core.Storage.Update(req)
			fwd := corecommon.Forward{Name: "upstream", Uri: "http://tracker.example/announce"}
			job := AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, ForwarderName: "upstream", Forwarder: fwd, Request: req, state: &announceJobState{peerContext: ctx}}
			if !fm.handleRetryError(job, "busy", value, fwd.Uri) {
				t.Fatal("retry not handled")
			}
			fm.handleAnnounceResult(job, AnnounceResult{Deferred: true})
			if cache.ShouldAnnounceNow(req.InfoHash, "upstream", time.Now()) {
				t.Fatal("deferred upstream is eligible")
			}
			fm.finishJob(job)
			if !fm.isJobPending(job.InfoHash, "upstream", req.PeerID) {
				t.Fatal("retry marker lost")
			}
			core.Storage.Delete(req)
			if cache.HasInfoHash(req.InfoHash) {
				t.Fatal("peer removal retained cooldown state")
			}
			fm.handleRetryError(job, "busy", value, fwd.Uri)
			if cache.HasInfoHash(req.InfoHash) {
				t.Fatal("canceled retry restored cache")
			}
		})
	}
	for _, value := range []any{nil, 0, -1, "invalid", float64(2), int64(1 << 62)} {
		t.Run("invalid", func(t *testing.T) {
			fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
			defer fm.Stop()
			if fm.handleRetryError(AnnounceJob{}, "busy", value, "") {
				t.Fatalf("invalid retry value accepted: %v", value)
			}
		})
	}
}

func TestPermanentRetryDoesNotRecreateCache(t *testing.T) {
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	fwd := corecommon.Forward{Name: "upstream", Uri: "http://tracker.example/announce"}
	fm.Forwarders = []corecommon.Forward{fwd}
	job := AnnounceJob{InfoHash: btcommon.InfoHash(strings.Repeat("h", 20)), Forwarder: fwd, ForwarderName: fwd.GetName()}
	if !fm.handleRetryError(job, "not a tracker", "never", fwd.Uri) {
		t.Fatal("permanent retry not handled")
	}
	fm.handleAnnounceResult(job, AnnounceResult{Deferred: true})
	if !fm.isDisabled(fwd.GetName()) || fm.Storage.HasInfoHash(job.InfoHash) {
		t.Fatal("disabled tracker cache recreated")
	}
}

func TestSessionCancellationDoesNotRemoveRestartedJobs(t *testing.T) {
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
	defer fm.Stop()
	req := regressionRequest(t)
	old, cancel := context.WithCancel(context.Background())
	cancel()
	current, stop := context.WithCancel(context.Background())
	defer stop()
	next := AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, ForwarderName: "upstream", state: &announceJobState{peerContext: current}, ScheduledTime: time.Now().Add(time.Minute)}
	fm.pendingJobs[fm.jobKey(req.InfoHash, "upstream", req.PeerID)] = next.state
	fm.scheduleJob(next)
	fm.cancelPendingJobs(req.InfoHash, req.PeerID, old)
	if !fm.isJobPending(req.InfoHash, "upstream", req.PeerID) || len(fm.scheduledJobs) != 1 {
		t.Fatal("old stop removed restarted jobs")
	}
}

func TestUDPParserRetainsInjectedLogger(t *testing.T) {
	var output logBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	core := NewCore(makeCfg(), nil, logger)
	defer core.Close()
	req := regressionRequest(t)
	_, failure := core.Receiver.UDP.ProcessAnnounce("203.0.113.10", string(req.InfoHash), string(req.PeerID), "6881", "0", "0", "100", "", "1", "unknown-event", "", "", "")
	if failure != "" {
		t.Fatal(failure)
	}
	if !strings.Contains(output.String(), "unknown-event") {
		t.Fatal("UDP parser bypassed injected logger")
	}
}

func TestSchedulerPrunesCanceledLongRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := makeCfg()
		fm := NewForwarderManager(cfg, NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(discardLogger()))
		defer fm.Stop()
		ctx, cancel := context.WithCancel(context.Background())
		req := regressionRequest(t)
		job := AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, ForwarderName: "upstream", state: &announceJobState{peerContext: ctx}, ScheduledTime: time.Now().Add(time.Hour)}
		fm.pendingJobs[fm.jobKey(job.InfoHash, job.ForwarderName, job.PeerID)] = job.state
		fm.scheduleJob(job)
		fm.Start()
		cancel()
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if fm.isJobPending(job.InfoHash, job.ForwarderName, job.PeerID) || len(fm.scheduledJobs) != 0 {
			t.Fatal("canceled long retry retained until its deadline")
		}
	})
}
