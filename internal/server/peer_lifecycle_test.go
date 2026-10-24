package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
)

func announceLifecycleEvent(t *testing.T, ra *ReceiverAnnounce, event string) {
	t.Helper()
	_, failure := ra.ProcessAnnounce("192.0.2.1", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1", "", "50", event, "", "", "")
	if failure != "" {
		t.Fatal(failure)
	}
}

func TestStoppedPeerInvalidatesQueuedAndScheduledJobs(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("d8:intervali1800e5:peers0:e"))
	}))
	defer upstream.Close()
	ra, fm, fs, cleanup := newIntegrationHarness(t, makeIntegrationCfg(), corecommon.Forward{Uri: upstream.URL}, false)
	defer cleanup()
	defer fm.udpForwarder.Stop()
	announceLifecycleEvent(t, ra, "started")
	oldJob := <-fm.jobQueue
	retry := oldJob
	retry.ScheduledTime = time.Now().Add(time.Minute)
	if !fm.scheduleJob(retry) {
		t.Fatal("retry was not scheduled")
	}
	announceLifecycleEvent(t, ra, "stopped")
	if fm.isJobPending(oldJob.InfoHash, oldJob.ForwarderName, oldJob.PeerID) || len(fm.scheduledJobs) != 0 {
		t.Fatal("stopped peer left pending or scheduled jobs")
	}
	fm.executeAnnounce(oldJob)
	fm.processScheduledReadyJob(retry, time.Now().Add(2*time.Minute))
	if requests.Load() != 0 || len(fm.jobQueue) != 0 {
		t.Fatal("stopped peer was forwarded")
	}
	// Restarting the same peer must not reactivate the old job or lose its new marker.
	announceLifecycleEvent(t, ra, "started")
	newJob := <-fm.jobQueue
	// A handler paused between updating storage and queueing retains its old session.
	fm.queueEligibleAnnounces(oldJob.InfoHash, oldJob.Request, oldJob.context())
	if len(fm.jobQueue) != 0 {
		t.Fatal("old request attached to the restarted peer's session")
	}
	fm.finishJob(oldJob)
	if !fm.isJobPending(newJob.InfoHash, newJob.ForwarderName, newJob.PeerID) {
		t.Fatal("old job cleared the restarted peer's pending marker")
	}
	fm.executeAnnounce(oldJob)
	fm.handleAnnounceResult(oldJob, AnnounceResult{Success: true, Interval: 1800})
	if requests.Load() != 0 || fs.HasInfoHash(oldJob.InfoHash) {
		t.Fatal("old job revived after peer restart")
	}
	fm.executeAnnounce(newJob)
	fm.finishJob(newJob)
	if requests.Load() != 1 || !fs.HasInfoHash(newJob.InfoHash) {
		t.Fatal("restarted peer did not announce successfully")
	}
}

func TestScheduledRetryKeepsItsOwnPendingMarker(t *testing.T) {
	ra, fm, _, cleanup := newIntegrationHarness(t, makeIntegrationCfg(), corecommon.Forward{Uri: "http://127.0.0.1:1/announce"}, false)
	defer cleanup()
	defer fm.udpForwarder.Stop()
	announceLifecycleEvent(t, ra, "started")
	job := <-fm.jobQueue
	if !fm.handleRetryError(job, "busy", 1, job.Forwarder.Uri) {
		t.Fatal("retry was not handled")
	}
	fm.finishJob(job)
	if !fm.isJobPending(job.InfoHash, job.ForwarderName, job.PeerID) || len(fm.scheduledJobs) != 1 {
		t.Fatal("finishing the original job cleared the scheduled retry")
	}
	// A duplicate retry must preserve the existing scheduled job's ownership.
	fm.handleRetryError(job, "busy", 1, job.Forwarder.Uri)
	fm.finishJob(job)
	if !fm.isJobPending(job.InfoHash, job.ForwarderName, job.PeerID) {
		t.Fatal("duplicate retry cleared the existing pending marker")
	}
	announceLifecycleEvent(t, ra, "stopped")
	if fm.isJobPending(job.InfoHash, job.ForwarderName, job.PeerID) || len(fm.scheduledJobs) != 0 {
		t.Fatal("stopped peer left its retry scheduled")
	}
}

func TestStoppedPeerCancelsInFlightHTTP(t *testing.T) {
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
	ra, fm, fs, cleanup := newIntegrationHarness(t, makeIntegrationCfg(), corecommon.Forward{Uri: upstream.URL}, false)
	defer cleanup()
	defer fm.udpForwarder.Stop()
	announceLifecycleEvent(t, ra, "started")
	job := <-fm.jobQueue
	done := make(chan struct{})
	go func() {
		fm.executeAnnounce(job)
		fm.finishJob(job)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("forwarded request did not start")
	}
	announceLifecycleEvent(t, ra, "stopped")
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("stopped peer did not cancel the HTTP request")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled job did not finish")
	}
	if fs.HasInfoHash(job.InfoHash) {
		t.Fatal("late result repopulated the stopped peer's cache")
	}
}

func TestPeerSessionSurvivesUpdatesAndEndsOnDelete(t *testing.T) {
	s := &Storage{Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request)}
	req := tracker.Request{InfoHash: common.InfoHash(strings.Repeat("h", 20)), PeerID: common.PeerID(strings.Repeat("p", 20))}
	s.Update(req)
	first := s.peerContext(req.InfoHash, req.PeerID)
	s.Update(req)
	if s.peerContext(req.InfoHash, req.PeerID) != first || first.Err() != nil {
		t.Fatal("periodic update changed the peer session")
	}
	s.Delete(req)
	if first.Err() == nil || s.peerContext(req.InfoHash, req.PeerID) != nil {
		t.Fatal("deletion did not end the peer session")
	}
	s.Update(req)
	if next := s.peerContext(req.InfoHash, req.PeerID); next == first || next.Err() != nil {
		t.Fatal("restart did not create an active peer session")
	}
}
