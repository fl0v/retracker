package server

import (
	"errors"
	"fmt"
	"net"
	"strings"
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

func TestClassifyForwarderErrorPermanent(t *testing.T) {
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
			name:     "Wrapped DNS error",
			err:      fmt.Errorf("failed to get connection ID: %w", &net.DNSError{Err: "no such host", Name: "ratelimit.example"}),
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
			classified := classifyForwarderError(tt.err)
			result := classified != nil && classified.IsPermanent()
			if result != tt.expected {
				t.Errorf("classifyForwarderError(%v).IsPermanent() = %v, want %v", tt.err, result, tt.expected)
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

func TestClassifyForwarderErrorRateLimitPatterns(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "Nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "Too many requests",
			err:      errors.New("tracker error: Too Many Requests"),
			expected: true,
		},
		{
			name:     "Rate limit exceeded",
			err:      errors.New("tracker error: rate limit exceeded"),
			expected: true,
		},
		{
			name:     "Slow down",
			err:      errors.New("tracker says please slow down"),
			expected: true,
		},
		{
			name:     "Flood control",
			err:      errors.New("flood detected, retry later"),
			expected: true,
		},
		{
			name:     "Non-rate-limit error",
			err:      errors.New("tracker error: invalid passkey"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := false
			if tt.err != nil {
				result = isRateLimitMessage(strings.ToLower(tt.err.Error()))
			}
			if result != tt.expected {
				t.Errorf("isRateLimitMessage(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestClassifyForwarderErrorSuspend(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "Tracker error too many requests",
			err:      errors.New("tracker error: Too Many Requests"),
			expected: true,
		},
		{
			name:     "Tracker connect error rate limit",
			err:      errors.New("tracker error on connect: rate limit exceeded"),
			expected: true,
		},
		{
			name:     "Tracker error non-rate-limit",
			err:      errors.New("tracker error: invalid passkey"),
			expected: false,
		},
		{
			name:     "DNS error with rate-like hostname",
			err:      errors.New("failed to resolve UDP address: lookup ratelimit.example: no such host"),
			expected: false,
		},
		{
			name:     "Generic local flood text",
			err:      errors.New("write udp: local flood control"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classified := classifyForwarderError(tt.err)
			result := classified != nil && classified.ShouldSuspend()
			if result != tt.expected {
				t.Errorf("classifyForwarderError(%v).ShouldSuspend() = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestClassifyForwarderError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		expectedType ForwarderErrorType
	}{
		{
			name:         "HTTP 429 classified as rate limited",
			err:          errors.New("HTTP status 429: 429 Too Many Requests"),
			expectedType: ForwarderErrorRateLimited,
		},
		{
			name:         "UDP tracker rate limit classified as rate limited",
			err:          errors.New("tracker error: rate limit exceeded"),
			expectedType: ForwarderErrorRateLimited,
		},
		{
			name:         "Tracker rejection classified as permanent",
			err:          errors.New("HTTP status 404 (tracker rejection)"),
			expectedType: ForwarderErrorPermanent,
		},
		{
			name:         "DNS classified as permanent",
			err:          errors.New("failed to resolve UDP address: lookup tracker.example: no such host"),
			expectedType: ForwarderErrorPermanent,
		},
		{
			name:         "Transient HTTP status classified as retryable",
			err:          errors.New("HTTP status 503: 503 Service Unavailable"),
			expectedType: ForwarderErrorRetryable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classified := classifyForwarderError(tt.err)
			if classified == nil {
				t.Fatalf("expected non-nil classified error")
			}
			if classified.Type != tt.expectedType {
				t.Fatalf("expected type %q, got %q", tt.expectedType, classified.Type)
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

	// Use different main domains to avoid deduplication
	fwd1 := corecommon.Forward{Name: "active1", Uri: "http://example.com"}
	fwd2 := corecommon.Forward{Name: "disabled", Uri: "http://invalid.com"}
	fwd3 := corecommon.Forward{Name: "active2", Uri: "http://another.com"}
	cfg.Forwards = []corecommon.Forward{fwd1, fwd2, fwd3}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Verify all forwarders are in the list (different domains, so no deduplication)
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

func TestQueueThrottleLogStateChangesOnlyOnFillChange(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	if !fm.shouldLogQueueThrottle(78) {
		t.Fatalf("expected first fill pct to be loggable")
	}
	if fm.shouldLogQueueThrottle(78) {
		t.Fatalf("expected repeated same fill pct to be suppressed")
	}
	if !fm.shouldLogQueueThrottle(79) {
		t.Fatalf("expected changed fill pct to be loggable")
	}

	fm.resetQueueThrottleLogState()
	if !fm.shouldLogQueueThrottle(79) {
		t.Fatalf("expected fill pct to be loggable after reset")
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

func TestQueueEligibleAnnouncesPrioritizesNeverAnnouncedAndOverdue(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderQueueSize = 100
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.maxForwardersPerAnnounce = 2
	fm.queueThrottleThresh = 100 // Disable throttling

	fNew := corecommon.Forward{Name: "new", Uri: "http://new"}
	fOld := corecommon.Forward{Name: "overdue-long", Uri: "http://overdue-long"}
	fRecent := corecommon.Forward{Name: "overdue-short", Uri: "http://overdue-short"}
	fm.Forwarders = []corecommon.Forward{fNew, fOld, fRecent}

	ih := btcommon.InfoHash("prioritize-test-hash1") // 20 bytes
	now := time.Now()

	fs.mu.Lock()
	fs.Entries[ih] = map[string]ForwarderPeerEntry{
		fOld.GetName(): {
			NextAnnounce: now.Add(-10 * time.Minute),
		},
		fRecent.GetName(): {
			NextAnnounce: now.Add(-1 * time.Minute),
		},
	}
	fs.mu.Unlock()

	req := tracker.Request{PeerID: btcommon.PeerID("peer-00000000000003")}
	fm.QueueEligibleAnnounces(ih, req)

	if got := len(fm.jobQueue); got != 2 {
		t.Fatalf("expected 2 jobs queued; got %d", got)
	}

	job1 := <-fm.jobQueue
	job2 := <-fm.jobQueue
	names := map[string]bool{
		job1.ForwarderName: true,
		job2.ForwarderName: true,
	}

	if !names[fNew.GetName()] {
		t.Fatalf("expected never-announced forwarder to be queued")
	}
	if !names[fOld.GetName()] {
		t.Fatalf("expected most-overdue forwarder to be queued")
	}
	if names[fRecent.GetName()] {
		t.Fatalf("did not expect less-overdue forwarder to be queued")
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

func TestProcessScheduledReadyJobSkipsSuspendedForwarder(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.jobQueue = make(chan AnnounceJob, 1)

	forwarderName := "suspended-forwarder"
	fm.suspendForwarder(forwarderName, 2*time.Second)

	now := time.Now()
	job := AnnounceJob{
		InfoHash:      btcommon.InfoHash("sched-suspend-hash01"),
		ForwarderName: forwarderName,
		PeerID:        btcommon.PeerID("peer-00000000000004"),
	}

	fm.processScheduledReadyJob(job, now)

	if got := len(fm.jobQueue); got != 0 {
		t.Fatalf("expected suspended job to not be queued, got queue len %d", got)
	}

	fm.scheduledJobsMu.RLock()
	defer fm.scheduledJobsMu.RUnlock()

	if len(fm.scheduledJobs) == 0 {
		t.Fatalf("expected suspended job to be rescheduled")
	}

	found := false
	for scheduledAt, jobs := range fm.scheduledJobs {
		if scheduledAt.Before(now) {
			t.Fatalf("expected rescheduled time to be in the future, got %v", scheduledAt)
		}
		for _, scheduledJob := range jobs {
			if scheduledJob.ForwarderName == forwarderName && scheduledJob.InfoHash == job.InfoHash {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected suspended job to exist in scheduled jobs map")
	}
}

func TestProcessScheduledReadyJobQueuesWhenActive(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	fm.jobQueue = make(chan AnnounceJob, 1)

	job := AnnounceJob{
		InfoHash:      btcommon.InfoHash("sched-active-hash001"),
		ForwarderName: "active-forwarder",
		PeerID:        btcommon.PeerID("peer-00000000000005"),
	}

	fm.processScheduledReadyJob(job, time.Now())

	if got := len(fm.jobQueue); got != 1 {
		t.Fatalf("expected active job to be queued, got queue len %d", got)
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

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       net.IP
		expected bool
	}{
		{
			name:     "localhost 127.0.0.1",
			ip:       net.IPv4(127, 0, 0, 1),
			expected: true,
		},
		{
			name:     "localhost 127.1.1.1",
			ip:       net.IPv4(127, 1, 1, 1),
			expected: true,
		},
		{
			name:     "private network 10.0.0.1",
			ip:       net.IPv4(10, 0, 0, 1),
			expected: true,
		},
		{
			name:     "private network 10.255.255.255",
			ip:       net.IPv4(10, 255, 255, 255),
			expected: true,
		},
		{
			name:     "private network 172.16.0.1",
			ip:       net.IPv4(172, 16, 0, 1),
			expected: true,
		},
		{
			name:     "private network 172.31.255.255",
			ip:       net.IPv4(172, 31, 255, 255),
			expected: true,
		},
		{
			name:     "private network 172.15.255.255 (not in range)",
			ip:       net.IPv4(172, 15, 255, 255),
			expected: false,
		},
		{
			name:     "private network 172.32.0.1 (not in range)",
			ip:       net.IPv4(172, 32, 0, 1),
			expected: false,
		},
		{
			name:     "private network 192.168.0.1",
			ip:       net.IPv4(192, 168, 0, 1),
			expected: true,
		},
		{
			name:     "private network 192.168.255.255",
			ip:       net.IPv4(192, 168, 255, 255),
			expected: true,
		},
		{
			name:     "public IP 8.8.8.8",
			ip:       net.IPv4(8, 8, 8, 8),
			expected: false,
		},
		{
			name:     "public IP 1.1.1.1",
			ip:       net.IPv4(1, 1, 1, 1),
			expected: false,
		},
		{
			name:     "192.169.0.1 (not in 192.168.x.x range)",
			ip:       net.IPv4(192, 169, 0, 1),
			expected: false,
		},
		{
			name:     "nil IP",
			ip:       nil,
			expected: false,
		},
		{
			name:     "IPv6 address",
			ip:       net.ParseIP("2001:db8::1"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isBlockedIP(tt.ip)
			if result != tt.expected {
				t.Errorf("isBlockedIP(%v) = %v, want %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestValidateAndRemoveInvalidForwarders(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Mix of valid and invalid forwarders
	validHTTP := corecommon.Forward{Name: "valid-http", Uri: "http://example.com:80"}
	validHTTPS := corecommon.Forward{Name: "valid-https", Uri: "https://example.com:443"}
	validUDP := corecommon.Forward{Name: "valid-udp", Uri: "udp://tracker.example.com:6969"}
	invalidUDPNoPort := corecommon.Forward{Name: "invalid-udp", Uri: "udp://tracker.example.com"}
	invalidUDPNoPort2 := corecommon.Forward{Name: "invalid-udp-2", Uri: "udp://another.tracker.com"}

	cfg.Forwards = []corecommon.Forward{
		validHTTP,
		validHTTPS,
		validUDP,
		invalidUDPNoPort,
		invalidUDPNoPort2,
	}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Verify invalid UDP forwarders were removed
	if !fm.isDisabled(invalidUDPNoPort.GetName()) {
		t.Errorf("invalid UDP forwarder without port should be disabled")
	}

	if !fm.isDisabled(invalidUDPNoPort2.GetName()) {
		t.Errorf("invalid UDP forwarder without port should be disabled")
	}

	// Verify valid forwarders are still active
	if fm.isDisabled(validHTTP.GetName()) {
		t.Errorf("valid HTTP forwarder should not be disabled")
	}

	if fm.isDisabled(validHTTPS.GetName()) {
		t.Errorf("valid HTTPS forwarder should not be disabled")
	}

	if fm.isDisabled(validUDP.GetName()) {
		t.Errorf("valid UDP forwarder with port should not be disabled")
	}

	// Verify forwarders were removed from the active list
	// Note: All three valid forwarders share the same main domain (example.com),
	// so deduplication will keep only the UDP one (highest priority: UDP + port 6969 + tracker. subdomain)
	fm.forwardersMu.RLock()
	remainingCount := len(fm.Forwarders)
	remainingNames := make(map[string]bool)
	for _, f := range fm.Forwarders {
		remainingNames[f.GetName()] = true
	}
	fm.forwardersMu.RUnlock()

	if remainingCount != 1 {
		t.Errorf("expected 1 forwarder remaining (UDP wins due to deduplication by main domain), got %d", remainingCount)
	}

	// UDP forwarder should be kept (highest priority)
	if !remainingNames[validUDP.GetName()] {
		t.Errorf("valid UDP forwarder should be in active list (highest priority)")
	}

	// HTTP and HTTPS should be removed due to deduplication (same main domain as UDP)
	if remainingNames[validHTTP.GetName()] {
		t.Errorf("valid HTTP forwarder should be removed due to deduplication (same main domain as UDP)")
	}

	if remainingNames[validHTTPS.GetName()] {
		t.Errorf("valid HTTPS forwarder should be removed due to deduplication (same main domain as UDP)")
	}

	if remainingNames[invalidUDPNoPort.GetName()] {
		t.Errorf("invalid UDP forwarder should not be in active list")
	}

	if remainingNames[invalidUDPNoPort2.GetName()] {
		t.Errorf("invalid UDP forwarder should not be in active list")
	}
}

func TestValidateAndRemoveInvalidForwardersWithInvalidURI(t *testing.T) {
	cfg := makeCfg()
	cfg.Debug = true // Enable debug to see skip messages
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Forwarder with invalid URI should be skipped (not cause panic)
	invalidURI := corecommon.Forward{Name: "invalid-uri", Uri: "://invalid"}
	validUDP := corecommon.Forward{Name: "valid-udp", Uri: "udp://tracker.example.com:6969"}

	cfg.Forwards = []corecommon.Forward{invalidURI, validUDP}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Invalid URI should be skipped, valid UDP should remain
	if fm.isDisabled(validUDP.GetName()) {
		t.Errorf("valid UDP forwarder should not be disabled")
	}

	// Invalid URI forwarder should still be in the list (validation skipped it)
	fm.forwardersMu.RLock()
	remainingCount := len(fm.Forwarders)
	fm.forwardersMu.RUnlock()

	// Should have at least the valid one, invalid one may or may not be there
	if remainingCount < 1 {
		t.Errorf("expected at least 1 forwarder, got %d", remainingCount)
	}
}

func TestCheckAndRemoveBlockedForwarders(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Use localhost which should resolve to 127.0.0.1 (blocked)
	blockedLocalhost := corecommon.Forward{Name: "blocked-localhost", Uri: "http://localhost:8080"}
	// Use a public domain that should not be blocked
	validPublic := corecommon.Forward{Name: "valid-public", Uri: "http://example.com:80"}

	cfg.Forwards = []corecommon.Forward{blockedLocalhost, validPublic}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Verify localhost forwarder was removed (if DNS resolution works)
	// Note: This test may be flaky if DNS resolution fails, but localhost should always resolve
	if !fm.isDisabled(blockedLocalhost.GetName()) {
		// If DNS lookup failed, the forwarder won't be disabled
		// This is acceptable behavior - we keep forwarders if DNS fails
		t.Logf("localhost forwarder not disabled - DNS lookup may have failed (acceptable)")
	} else {
		// If it was disabled, verify it's removed from active list
		fm.forwardersMu.RLock()
		remainingNames := make(map[string]bool)
		for _, f := range fm.Forwarders {
			remainingNames[f.GetName()] = true
		}
		fm.forwardersMu.RUnlock()

		if remainingNames[blockedLocalhost.GetName()] {
			t.Errorf("blocked localhost forwarder should not be in active list")
		}
	}

	// Valid public forwarder should always remain
	if fm.isDisabled(validPublic.GetName()) {
		t.Errorf("valid public forwarder should not be disabled")
	}
}

func TestCheckAndRemoveBlockedForwardersWithInvalidURI(t *testing.T) {
	cfg := makeCfg()
	cfg.Debug = true
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Forwarder with invalid URI should be skipped
	invalidURI := corecommon.Forward{Name: "invalid-uri", Uri: "://invalid"}
	validPublic := corecommon.Forward{Name: "valid-public", Uri: "http://example.com:80"}

	cfg.Forwards = []corecommon.Forward{invalidURI, validPublic}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Valid forwarder should remain
	if fm.isDisabled(validPublic.GetName()) {
		t.Errorf("valid public forwarder should not be disabled")
	}
}

func TestCheckAndRemoveBlockedForwardersWithEmptyHostname(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}

	// Forwarder with empty hostname should be skipped
	emptyHost := corecommon.Forward{Name: "empty-host", Uri: "http://"}
	validPublic := corecommon.Forward{Name: "valid-public", Uri: "http://example.com:80"}

	cfg.Forwards = []corecommon.Forward{emptyHost, validPublic}

	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	// Valid forwarder should remain
	if fm.isDisabled(validPublic.GetName()) {
		t.Errorf("valid public forwarder should not be disabled")
	}
}

// TestRemoveDuplicateForwardersByDomain tests the deduplication logic
func TestRemoveDuplicateForwardersByDomain(t *testing.T) {
	tests := []struct {
		name           string
		forwarders     []corecommon.Forward
		expectedCount  int
		expectedToKeep string // Name of forwarder that should be kept
		description    string
	}{
		{
			name: "UDP wins over HTTP/HTTPS",
			forwarders: []corecommon.Forward{
				{Name: "http", Uri: "http://example.com:80"},
				{Name: "https", Uri: "https://example.com:443"},
				{Name: "udp", Uri: "udp://tracker.example.com:6969"},
			},
			expectedCount:  1,
			expectedToKeep: "udp",
			description:    "UDP has highest priority (1000 points)",
		},
		{
			name: "/announce path wins over plain path",
			forwarders: []corecommon.Forward{
				{Name: "plain", Uri: "http://example.com:80"},
				{Name: "announce", Uri: "http://example.com:80/announce"},
			},
			expectedCount:  1,
			expectedToKeep: "announce",
			description:    "/announce path has higher priority (100 points)",
		},
		{
			name: "Port 6969 wins over other ports",
			forwarders: []corecommon.Forward{
				{Name: "port80", Uri: "http://example.com:80"},
				{Name: "port6969", Uri: "http://example.com:6969"},
			},
			expectedCount:  1,
			expectedToKeep: "port6969",
			description:    "Port 6969 has higher priority (50 points)",
		},
		{
			name: "tracker. subdomain wins",
			forwarders: []corecommon.Forward{
				{Name: "plain", Uri: "http://example.com:80"},
				{Name: "tracker", Uri: "http://tracker.example.com:80"},
			},
			expectedCount:  1,
			expectedToKeep: "tracker",
			description:    "tracker. subdomain has higher priority (25 points)",
		},
		{
			name: "Longer URL wins as tiebreaker",
			forwarders: []corecommon.Forward{
				{Name: "short", Uri: "http://example.com:80"},
				{Name: "long", Uri: "http://example.com:80/very/long/path/here"},
			},
			expectedCount:  1,
			expectedToKeep: "long",
			description:    "Longer URL wins as tiebreaker (1 point per character)",
		},
		{
			name: "UDP with port 6969 wins over UDP without",
			forwarders: []corecommon.Forward{
				{Name: "udp-no-port", Uri: "udp://tracker.example.com:8080"},
				{Name: "udp-6969", Uri: "udp://tracker.example.com:6969"},
			},
			expectedCount:  1,
			expectedToKeep: "udp-6969",
			description:    "UDP with port 6969 has highest score",
		},
		{
			name: "Different domains are not deduplicated",
			forwarders: []corecommon.Forward{
				{Name: "example", Uri: "http://example.com:80"},
				{Name: "test", Uri: "http://test.com:80"},
				{Name: "other", Uri: "http://other.com:80"},
			},
			expectedCount:  3,
			expectedToKeep: "", // All should be kept
			description:    "Different main domains should not be deduplicated",
		},
		{
			name: "Complex priority: UDP with announce and 6969",
			forwarders: []corecommon.Forward{
				{Name: "http-plain", Uri: "http://example.com:80"},
				{Name: "http-announce", Uri: "http://example.com:80/announce"},
				{Name: "udp-6969", Uri: "udp://tracker.example.com:6969/announce"},
			},
			expectedCount:  1,
			expectedToKeep: "udp-6969",
			description:    "UDP with port 6969 and /announce should win",
		},
		{
			name: "Subdomain extraction works correctly",
			forwarders: []corecommon.Forward{
				{Name: "sub1", Uri: "http://sub1.example.com:80"},
				{Name: "sub2", Uri: "http://sub2.example.com:80"},
				{Name: "tracker", Uri: "http://tracker.example.com:80"},
			},
			expectedCount:  1,
			expectedToKeep: "tracker",
			description:    "All share main domain example.com, tracker. subdomain wins",
		},
		{
			name: "Single forwarder is kept",
			forwarders: []corecommon.Forward{
				{Name: "single", Uri: "http://example.com:80"},
			},
			expectedCount:  1,
			expectedToKeep: "single",
			description:    "Single forwarder should be kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := makeCfg()
			fs := NewForwarderStorage()
			st := &Storage{
				Config:   cfg,
				Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
			}

			cfg.Forwards = tt.forwarders
			fm := NewForwarderManager(cfg, fs, st, nil, nil)

			fm.forwardersMu.RLock()
			remainingCount := len(fm.Forwarders)
			remainingNames := make(map[string]bool)
			for _, f := range fm.Forwarders {
				remainingNames[f.GetName()] = true
			}
			fm.forwardersMu.RUnlock()

			if remainingCount != tt.expectedCount {
				t.Errorf("%s: expected %d forwarder(s) remaining, got %d", tt.description, tt.expectedCount, remainingCount)
			}

			if tt.expectedToKeep != "" {
				if !remainingNames[tt.expectedToKeep] {
					t.Errorf("%s: expected forwarder '%s' to be kept, but it was removed", tt.description, tt.expectedToKeep)
				}

				// Verify other forwarders were removed
				for _, fwd := range tt.forwarders {
					if fwd.GetName() != tt.expectedToKeep {
						if remainingNames[fwd.GetName()] {
							t.Errorf("%s: expected forwarder '%s' to be removed, but it was kept", tt.description, fwd.GetName())
						}
					}
				}
			} else {
				// All should be kept
				for _, fwd := range tt.forwarders {
					if !remainingNames[fwd.GetName()] {
						t.Errorf("%s: expected forwarder '%s' to be kept, but it was removed", tt.description, fwd.GetName())
					}
				}
			}
		})
	}
}
