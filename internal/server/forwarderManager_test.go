package server

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/config"
)

func makeCfg() *config.Config {
	return &config.Config{
		ForwarderQueueSize:     10,
		ForwarderFailThreshold: 2,
		ForwarderRetryAttempts: 1,
		ForwarderRetryBaseMs:   10,
		ForwardTimeout:         1,
	}
}

func TestDisableAfterFailures(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Inject a single forwarder
	fwd := corecommon.Forward{Name: "failer", Uri: "http://invalid"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Simulate failures
	fm.registerFailure(fwd.GetName())
	if fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder disabled too early")
	}
	fm.registerFailure(fwd.GetName())
	if !fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should be disabled after reaching threshold")
	}
}

func TestResetFailureOnSuccess(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "flaky", Uri: "http://invalid"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	fm.registerFailure(fwd.GetName())
	fm.resetFailure(fwd.GetName())
	if fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should not be disabled after reset")
	}
}

func TestIsNonRetryableError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "DNS error",
			err:      &net.DNSError{Err: "no such host", Name: "invalid.example.com"},
			expected: true,
		},
		{
			name:     "Connection refused",
			err:      &net.OpError{Err: syscall.ECONNREFUSED},
			expected: true,
		},
		{
			name:     "No such host in error message",
			err:      errors.New("dial tcp: lookup invalid.example.com: no such host"),
			expected: true,
		},
		{
			name:     "Unknown host",
			err:      errors.New("unknown host"),
			expected: true,
		},
		{
			name:     "Connection refused in error message",
			err:      errors.New("connection refused"),
			expected: true,
		},
		{
			name:     "No route to host",
			err:      errors.New("no route to host"),
			expected: true,
		},
		{
			name:     "Network unreachable",
			err:      errors.New("network is unreachable"),
			expected: true,
		},
		{
			name:     "Timeout error (retryable)",
			err:      &net.OpError{Err: errors.New("timeout")},
			expected: false,
		},
		{
			name:     "Generic error (retryable)",
			err:      errors.New("some generic error"),
			expected: false,
		},
		{
			name:     "Nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isNonRetryableError(tt.err)
			if result != tt.expected {
				t.Errorf("isNonRetryableError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestIsTrackerRejection(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		expected   bool
	}{
		{"400 Bad Request", 400, true},
		{"403 Forbidden", 403, true},
		{"404 Not Found", 404, true},
		{"200 OK", 200, false},
		{"500 Internal Server Error", 500, false},
		{"502 Bad Gateway", 502, false},
		{"503 Service Unavailable", 503, false},
		{"429 Too Many Requests", 429, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isTrackerRejection(tt.statusCode)
			if result != tt.expected {
				t.Errorf("isTrackerRejection(%d) = %v, want %v", tt.statusCode, result, tt.expected)
			}
		})
	}
}

func TestRetryableErrorUsesRegisterFailure(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 3 // Need 3 failures to disable
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "retryable", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Simulate retryable error (should use registerFailure, not immediate disable)
	// This simulates what happens when a retryable error occurs after all retries fail
	fm.registerFailure(fwd.GetName())
	if fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should not be disabled after 1 failure")
	}

	fm.registerFailure(fwd.GetName())
	if fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should not be disabled after 2 failures")
	}

	fm.registerFailure(fwd.GetName())
	if !fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should be disabled after 3 failures (threshold reached)")
	}
}

func TestNonRetryableErrorDisablesImmediately(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 10 // High threshold, but should disable immediately
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "nonretryable", Uri: "http://invalid-host-that-does-not-exist.example"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Simulate non-retryable error (should disable immediately)
	dnsErr := &net.DNSError{Err: "no such host", Name: "invalid-host-that-does-not-exist.example"}
	fm.disableForwarder(fwd.GetName(), "DNS error: "+dnsErr.Error())

	if !fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should be disabled immediately for non-retryable error")
	}

	// Verify it's in disabledForwarders with reason
	fm.pendingMu.Lock()
	disabled, exists := fm.disabledForwarders[fwd.GetName()]
	fm.pendingMu.Unlock()

	if !exists {
		t.Fatalf("forwarder should be in disabledForwarders map")
	}

	if disabled == nil {
		t.Fatalf("disabled forwarder entry should not be nil")
	}

	if disabled.Reason == "" {
		t.Fatalf("disabled forwarder should have a reason")
	}

	// Verify the disabled forwarder has the expected data
	if disabled.Reason == "" {
		t.Fatalf("disabled forwarder should have a reason")
	}
	if disabled.DisabledAt == 0 {
		t.Fatalf("disabled forwarder should have a timestamp")
	}
}

func TestTrackerRejectionDisablesImmediately(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 10 // High threshold, but should disable immediately
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "rejected", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Simulate tracker rejection (HTTP 404)
	fm.disableForwarder(fwd.GetName(), "HTTP status 404 (tracker rejection)")

	if !fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should be disabled immediately for tracker rejection")
	}

	// Verify it's in disabledForwarders
	fm.pendingMu.Lock()
	disabled, exists := fm.disabledForwarders[fwd.GetName()]
	fm.pendingMu.Unlock()

	if !exists {
		t.Fatalf("forwarder should be in disabledForwarders map")
	}

	if disabled.Reason != "HTTP status 404 (tracker rejection)" {
		t.Fatalf("disabled forwarder should have correct reason, got: %s", disabled.Reason)
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 2
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "flaky", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Register one failure
	fm.registerFailure(fwd.GetName())

	// Reset on success
	fm.resetFailure(fwd.GetName())

	// Register another failure (should be first failure, not second)
	fm.registerFailure(fwd.GetName())
	if fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should not be disabled - failure count should have been reset")
	}

	// One more failure should disable
	fm.registerFailure(fwd.GetName())
	if !fm.isDisabled(fwd.GetName()) {
		t.Fatalf("forwarder should be disabled after 2 failures")
	}
}

func TestDisabledForwarderRemovedFromActiveLists(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd1 := corecommon.Forward{Name: "active1", Uri: "http://example.com"}
	fwd2 := corecommon.Forward{Name: "disabled", Uri: "http://invalid"}
	fwd3 := corecommon.Forward{Name: "active2", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd1, fwd2, fwd3}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Verify all forwarders are in the list
	fm.forwardersMu.RLock()
	initialCount := len(fm.Forwarders)
	fm.forwardersMu.RUnlock()

	if initialCount != 3 {
		t.Fatalf("expected 3 forwarders, got %d", initialCount)
	}

	// Disable one forwarder
	fm.disableForwarder(fwd2.GetName(), "test disable")

	// Verify it's disabled
	if !fm.isDisabled(fwd2.GetName()) {
		t.Fatalf("forwarder should be disabled")
	}

	// Verify it's removed from Forwarders slice
	fm.forwardersMu.RLock()
	remainingCount := len(fm.Forwarders)
	remainingNames := make([]string, len(fm.Forwarders))
	for i, f := range fm.Forwarders {
		remainingNames[i] = f.GetName()
	}
	fm.forwardersMu.RUnlock()

	if remainingCount != 2 {
		t.Fatalf("expected 2 forwarders after disable, got %d", remainingCount)
	}

	// Verify the disabled one is not in the list
	for _, name := range remainingNames {
		if name == fwd2.GetName() {
			t.Fatalf("disabled forwarder should not be in Forwarders slice")
		}
	}

	// Verify it's removed from stats
	fm.statsMu.RLock()
	_, existsInStats := fm.stats[fwd2.GetName()]
	fm.statsMu.RUnlock()

	if existsInStats {
		t.Fatalf("disabled forwarder should be removed from stats map")
	}

	// Verify other forwarders are still active
	if fm.isDisabled(fwd1.GetName()) {
		t.Fatalf("other forwarders should still be active")
	}
	if fm.isDisabled(fwd3.GetName()) {
		t.Fatalf("other forwarders should still be active")
	}
}

func TestRateLimitThresholdZeroDisablesRateLimit(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.queueRateLimitThresh = 0 // disable
	fm.jobQueue = make(chan AnnounceJob, 10)
	for i := 0; i < 8; i++ { // fill to 80%
		fm.jobQueue <- AnnounceJob{}
	}

	if fm.shouldRateLimitInitial() {
		t.Fatalf("rate limiting should be disabled when threshold is 0")
	}
}

func TestRateLimitThresholdActive(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderQueueSize = 10
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.queueRateLimitThresh = 50 // 50%
	fm.jobQueue = make(chan AnnounceJob, 10)
	for i := 0; i < 6; i++ { // 60%
		fm.jobQueue <- AnnounceJob{}
	}

	if !fm.shouldRateLimitInitial() {
		t.Fatalf("rate limiting should be active when queue exceeds threshold")
	}
}

func TestQueueEligibleAnnouncesRespectsMaxLimit(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderQueueSize = 100
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.maxForwardersPerAnnounce = 2 // Limit to 2
	fm.queueThrottleThresh = 100    // Disable throttling

	// Prepare 4 forwarders
	f1 := corecommon.Forward{Name: "f1", Uri: "http://f1"}
	f2 := corecommon.Forward{Name: "f2", Uri: "http://f2"}
	f3 := corecommon.Forward{Name: "f3", Uri: "http://f3"}
	f4 := corecommon.Forward{Name: "f4", Uri: "http://f4"}
	fm.Forwarders = []corecommon.Forward{f1, f2, f3, f4}

	ih := btcommon.InfoHash("max-limit-test-hash1") // 20 bytes
	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000000")}

	fm.QueueEligibleAnnounces(ih, req)

	// Should be limited to 2 jobs
	if got := len(fm.jobQueue); got != 2 {
		t.Fatalf("expected 2 jobs queued (max limit); got %d", got)
	}
}

func TestQueueEligibleAnnouncesThrottleLimitsCount(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderQueueSize = 10
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.queueThrottleThresh = 50
	fm.queueThrottleTopN = 1
	fm.maxForwardersPerAnnounce = 100
	fm.jobQueue = make(chan AnnounceJob, 10)
	for i := 0; i < 6; i++ { // 60% fill to trigger throttling
		fm.jobQueue <- AnnounceJob{}
	}

	// Prepare forwarders
	f1 := corecommon.Forward{Name: "f1", Uri: "http://f1"}
	f2 := corecommon.Forward{Name: "f2", Uri: "http://f2"}
	fm.Forwarders = []corecommon.Forward{f1, f2}

	ih := btcommon.InfoHash("throttle-test-hash01") // 20 bytes
	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000001")}

	fm.QueueEligibleAnnounces(ih, req)

	// With throttling (queueThrottleTopN = 1), only 1 job should be added (total 7)
	if got := len(fm.jobQueue); got != 7 {
		t.Fatalf("expected 7 jobs in queue (6 + 1 throttled); got %d", got)
	}
}

func TestQueueEligibleAnnouncesSkipsRecentlyAnnounced(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderQueueSize = 100
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.maxForwardersPerAnnounce = 100
	fm.queueThrottleThresh = 100 // Disable throttling

	// Prepare 3 forwarders
	f1 := corecommon.Forward{Name: "ready", Uri: "http://ready"}
	f2 := corecommon.Forward{Name: "notdue", Uri: "http://notdue"}
	f3 := corecommon.Forward{Name: "alsoready", Uri: "http://alsoready"}
	fm.Forwarders = []corecommon.Forward{f1, f2, f3}

	ih := btcommon.InfoHash("skip-recent-test-001") // 20 bytes
	now := time.Now()

	// Set one forwarder as not due (NextAnnounce in future)
	fs.mu.Lock()
	fs.Entries[ih] = map[string]ForwarderPeerEntry{
		"ready": {
			NextAnnounce: now.Add(-1 * time.Minute), // Due
		},
		"notdue": {
			NextAnnounce: now.Add(5 * time.Minute), // Not due (future)
		},
		// "alsoready" not in storage = never announced = due
	}
	fs.mu.Unlock()

	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000002")}
	fm.QueueEligibleAnnounces(ih, req)

	// Should queue 2 jobs (ready + alsoready), skip notdue
	if got := len(fm.jobQueue); got != 2 {
		t.Fatalf("expected 2 jobs queued; got %d", got)
	}
}

func TestSuspendForwarderExpires(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.suspendForwarder("s1", 20*time.Millisecond)
	if !fm.isSuspended("s1") {
		t.Fatalf("forwarder should be suspended immediately after suspension")
	}
	time.Sleep(30 * time.Millisecond)
	if fm.isSuspended("s1") {
		t.Fatalf("forwarder suspension should expire after duration")
	}
}

func TestQueueEligibleAnnouncesQueuesWhenDue(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "ready", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Set NextAnnounce in the past so it is due
	ih := btcommon.InfoHash("announce-hash-00001") // 20 bytes
	fs.mu.Lock()
	fs.Entries[ih] = map[string]ForwarderPeerEntry{
		fwd.GetName(): {
			Interval:     30,
			NextAnnounce: time.Now().Add(-1 * time.Minute),
		},
	}
	fs.mu.Unlock()

	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000000")}

	fm.QueueEligibleAnnounces(ih, req)

	if got := len(fm.jobQueue); got != 1 {
		t.Fatalf("expected 1 job queued, got %d", got)
	}

	job := <-fm.jobQueue
	if job.ForwarderName != fwd.GetName() {
		t.Fatalf("queued job forwarder mismatch: got %s want %s", job.ForwarderName, fwd.GetName())
	}
}

func TestQueueEligibleAnnouncesSkipsWhenNotDue(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fwd := corecommon.Forward{Name: "future", Uri: "http://example.com"}
	cfg.Forwards = []corecommon.Forward{fwd}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Set NextAnnounce in the future so it is not due
	ih := btcommon.InfoHash("announce-hash-00002") // 20 bytes
	fs.mu.Lock()
	fs.Entries[ih] = map[string]ForwarderPeerEntry{
		fwd.GetName(): {
			Interval:     30,
			NextAnnounce: time.Now().Add(5 * time.Minute),
		},
	}
	fs.mu.Unlock()

	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000001")}

	fm.QueueEligibleAnnounces(ih, req)

	if got := len(fm.jobQueue); got != 0 {
		t.Fatalf("expected 0 jobs queued when not due, got %d", got)
	}
}
