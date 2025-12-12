package server

import (
	"errors"
	"testing"
	"time"

	btcommon "github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
)

func TestBuildAnnounceURI(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	tests := []struct {
		name     string
		forward  corecommon.Forward
		request  tracker.Request
		event    string
		contains []string
	}{
		{
			name:    "basic announce",
			forward: corecommon.Forward{Uri: "http://tracker.example.com/announce"},
			request: tracker.Request{
				InfoHash:   btcommon.InfoHash("12345678901234567890"),
				PeerID:     btcommon.PeerID("peer-id-0000000000"),
				Port:       6881,
				Uploaded:   1000,
				Downloaded: 2000,
				Left:       3000,
			},
			event:    "",
			contains: []string{"info_hash=", "peer_id=", "port=6881", "uploaded=1000", "downloaded=2000", "left=3000"},
		},
		{
			name:    "with event",
			forward: corecommon.Forward{Uri: "http://tracker.example.com/announce"},
			request: tracker.Request{
				InfoHash: btcommon.InfoHash("12345678901234567890"),
				PeerID:   btcommon.PeerID("peer-id-0000000000"),
				Port:     6881,
			},
			event:    "started",
			contains: []string{"event=started"},
		},
		{
			name:    "with IP override",
			forward: corecommon.Forward{Uri: "http://tracker.example.com/announce", Ip: "192.168.1.100"},
			request: tracker.Request{
				InfoHash: btcommon.InfoHash("12345678901234567890"),
				PeerID:   btcommon.PeerID("peer-id-0000000000"),
				Port:     6881,
			},
			event:    "",
			contains: []string{"ip=192.168.1.100", "ipv4=192.168.1.100"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri := fm.buildAnnounceURI(tt.forward, tt.request, tt.event)

			for _, substr := range tt.contains {
				if !containsString(uri, substr) {
					t.Errorf("URI %q should contain %q", uri, substr)
				}
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestCancelPendingJobs(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih1 := btcommon.InfoHash("hash-000000000000001")
	ih2 := btcommon.InfoHash("hash-000000000000002")
	peer1 := btcommon.PeerID("peer-00000000000001")
	peer2 := btcommon.PeerID("peer-00000000000002")

	// Mark some jobs as pending
	fm.markJobPending(ih1, "tracker1", peer1)
	fm.markJobPending(ih1, "tracker2", peer1)
	fm.markJobPending(ih1, "tracker1", peer2) // Same hash, different peer
	fm.markJobPending(ih2, "tracker1", peer1) // Different hash, same peer

	// Cancel jobs for ih1/peer1
	fm.CancelPendingJobs(ih1, peer1)

	// Verify ih1/peer1 jobs are canceled
	if fm.isJobPending(ih1, "tracker1", peer1) {
		t.Error("job ih1/tracker1/peer1 should be canceled")
	}
	if fm.isJobPending(ih1, "tracker2", peer1) {
		t.Error("job ih1/tracker2/peer1 should be canceled")
	}

	// Verify other jobs are not affected
	if !fm.isJobPending(ih1, "tracker1", peer2) {
		t.Error("job ih1/tracker1/peer2 should NOT be canceled")
	}
	if !fm.isJobPending(ih2, "tracker1", peer1) {
		t.Error("job ih2/tracker1/peer1 should NOT be canceled")
	}
}

func TestHandleAnnounceResultSuccess(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("success-test-hash001")
	fwd := corecommon.Forward{Name: "test-tracker", Uri: "http://tracker.example.com/announce"}

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
	}

	peers := []btcommon.Peer{
		{IP: "192.168.1.1", Port: 6881},
		{IP: "192.168.1.2", Port: 6882},
	}

	result := AnnounceResult{
		Success:      true,
		Peers:        peers,
		Interval:     1800,
		ResponseSize: 100,
		Duration:     50 * time.Millisecond,
	}

	fm.handleAnnounceResult(job, result)

	// Verify storage was updated
	fs.mu.RLock()
	forwarderMap, exists := fs.Entries[ih]
	fs.mu.RUnlock()
	if !exists {
		t.Fatal("storage entry should exist after successful announce")
	}
	entry, ok := forwarderMap[fwd.GetName()]
	if !ok {
		t.Fatal("forwarder entry should exist")
	}
	if len(entry.Peers) != 2 {
		t.Errorf("expected 2 peers in storage; got %d", len(entry.Peers))
	}

	// Verify failure count was reset (use pending lock which protects failCounts)
	fm.pendingMu.Lock()
	failCount := fm.failCounts[fwd.GetName()]
	fm.pendingMu.Unlock()
	if failCount != 0 {
		t.Errorf("failure count should be 0 after success; got %d", failCount)
	}
}

func TestHandleAnnounceResultFailure(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 3
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("failure-test-hash01")
	fwd := corecommon.Forward{Name: "failing-tracker", Uri: "http://tracker.example.com/announce"}

	// Add forwarder to the list (required for disableForwarder to work)
	fm.Forwarders = []corecommon.Forward{fwd}

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
	}

	result := AnnounceResult{
		Success:      false,
		Duration:     100 * time.Millisecond,
		Error:        errors.New("connection timeout"),
		NonRetryable: false,
	}

	// First failure
	fm.handleAnnounceResult(job, result)

	fm.pendingMu.Lock()
	failCount := fm.failCounts[fwd.GetName()]
	fm.pendingMu.Unlock()
	if failCount != 1 {
		t.Errorf("failure count should be 1; got %d", failCount)
	}
	if fm.isDisabled(fwd.GetName()) {
		t.Error("forwarder should not be disabled after 1 failure")
	}

	// More failures up to threshold
	fm.handleAnnounceResult(job, result)
	fm.handleAnnounceResult(job, result)

	if !fm.isDisabled(fwd.GetName()) {
		t.Error("forwarder should be disabled after reaching threshold")
	}
}

func TestHandleAnnounceResultNonRetryable(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderFailThreshold = 10 // High threshold
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("nonretry-test-hash1")
	fwd := corecommon.Forward{Name: "bad-tracker", Uri: "http://tracker.example.com/announce"}

	// Add forwarder to the list (required for disableForwarder to work)
	fm.Forwarders = []corecommon.Forward{fwd}

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
	}

	result := AnnounceResult{
		Success:      false,
		Duration:     10 * time.Millisecond,
		Error:        errors.New("no such host"),
		NonRetryable: true,
	}

	fm.handleAnnounceResult(job, result)

	// Should be disabled immediately
	if !fm.isDisabled(fwd.GetName()) {
		t.Error("forwarder should be disabled immediately for non-retryable error")
	}
}

func TestHandleAnnounceResultSuspend(t *testing.T) {
	cfg := makeCfg()
	cfg.ForwarderSuspendSeconds = 300
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("suspend-test-hash01")
	fwd := corecommon.Forward{Name: "rate-limited", Uri: "http://tracker.example.com/announce"}

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
	}

	result := AnnounceResult{
		Success:  false,
		Duration: 50 * time.Millisecond,
		Error:    errors.New("HTTP 429 Too Many Requests"),
		Suspend:  true,
	}

	fm.handleAnnounceResult(job, result)

	// Should be suspended but not disabled
	if fm.isDisabled(fwd.GetName()) {
		t.Error("forwarder should not be disabled for suspend result")
	}
	if !fm.isSuspended(fwd.GetName()) {
		t.Error("forwarder should be suspended after 429 response")
	}
}

func TestExecuteAnnounceSkipsDisabled(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("disabled-test-hash1")
	fwd := corecommon.Forward{Name: "disabled-tracker", Uri: "http://tracker.example.com/announce"}

	// Add forwarder to the list first (required for disableForwarder to work)
	fm.Forwarders = []corecommon.Forward{fwd}

	// Now disable the forwarder (this removes it from Forwarders and adds to disabledForwarders)
	fm.disableForwarder(fwd.GetName(), "test disable")

	// Verify forwarder is now disabled
	if !fm.isDisabled(fwd.GetName()) {
		t.Fatal("forwarder should be disabled after disableForwarder call")
	}

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
		Request: tracker.Request{
			InfoHash: ih,
			PeerID:   btcommon.PeerID("peer-00000000000000"),
		},
	}

	// Should return early due to disabled check
	fm.executeAnnounce(job)

	// Verify no storage entry was created (disabled forwarder should be skipped)
	fs.mu.RLock()
	forwarderMap, exists := fs.Entries[ih]
	fs.mu.RUnlock()
	if exists && forwarderMap != nil {
		if _, ok := forwarderMap[fwd.GetName()]; ok {
			t.Error("no storage entry should be created for disabled forwarder")
		}
	}
}

func TestExecuteAnnounceSkipsSuspended(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("suspended-test-has1")
	fwd := corecommon.Forward{Name: "suspended-tracker", Uri: "http://tracker.example.com/announce"}

	// Suspend the forwarder
	fm.suspendForwarder(fwd.GetName(), 5*time.Minute)

	job := AnnounceJob{
		InfoHash:      ih,
		ForwarderName: fwd.GetName(),
		Forwarder:     fwd,
		Request: tracker.Request{
			InfoHash: ih,
			PeerID:   btcommon.PeerID("peer-00000000000000"),
		},
	}

	// Should not panic or do anything
	fm.executeAnnounce(job)

	// Verify no storage entry was created
	fs.mu.RLock()
	forwarderMap, exists := fs.Entries[ih]
	fs.mu.RUnlock()
	if exists && forwarderMap != nil {
		if _, ok := forwarderMap[fwd.GetName()]; ok {
			t.Error("no storage entry should be created for suspended forwarder")
		}
	}
}

func TestAnnounceResultStruct(t *testing.T) {
	// Test that AnnounceResult struct has expected fields and behavior
	result := AnnounceResult{
		Success:      true,
		Peers:        []btcommon.Peer{{IP: "1.2.3.4", Port: 6881}},
		Interval:     1800,
		ResponseSize: 256,
		Duration:     100 * time.Millisecond,
	}

	if !result.Success {
		t.Error("Success should be true")
	}
	if len(result.Peers) != 1 {
		t.Errorf("expected 1 peer; got %d", len(result.Peers))
	}
	if result.Interval != 1800 {
		t.Errorf("expected interval 1800; got %d", result.Interval)
	}
	if result.ResponseSize != 256 {
		t.Errorf("expected ResponseSize 256; got %d", result.ResponseSize)
	}
	if result.Duration != 100*time.Millisecond {
		t.Errorf("expected Duration 100ms; got %v", result.Duration)
	}
	// Zero values should be defaults
	if result.Error != nil {
		t.Error("Error should be nil by default")
	}
	if result.NonRetryable {
		t.Error("NonRetryable should be false by default")
	}
	if result.Suspend {
		t.Error("Suspend should be false by default")
	}

	// Test failure result
	failResult := AnnounceResult{
		Success:      false,
		Error:        errors.New("test error"),
		NonRetryable: true,
	}
	if failResult.Success {
		t.Error("Success should be false for failure result")
	}
	if failResult.Error == nil {
		t.Error("Error should not be nil for failure result")
	}
	if !failResult.NonRetryable {
		t.Error("NonRetryable should be true")
	}
}

func TestLogParseErrorTextDetection(t *testing.T) {
	cfg := makeCfg()
	cfg.Debug = true
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)
	tempStorage, _ := NewTempStorage("")
	fm.TempStorage = tempStorage

	// Test with text payload (should be detected as text)
	textPayload := []byte("d8:completei5e10:incompletei3e8:intervali1800ee")
	// This should not panic and should detect as text
	fm.logParseError(textPayload, "testhash", "http://example.com", errors.New("parse error"))

	// Test with binary payload (should be detected as binary)
	binaryPayload := make([]byte, 100)
	for i := range binaryPayload {
		binaryPayload[i] = byte(i % 256)
	}
	// This should not panic and should detect as binary
	fm.logParseError(binaryPayload, "testhash", "http://example.com", errors.New("parse error"))

	// Test with empty payload
	fm.logParseError([]byte{}, "testhash", "http://example.com", errors.New("empty response"))
}

func TestMarkUnmarkJobPending(t *testing.T) {
	cfg := makeCfg()
	fs := NewForwarderStorage()
	st := &Storage{
		Config:   cfg,
		Requests: make(map[btcommon.InfoHash]map[btcommon.PeerID]tracker.Request),
	}
	fm := NewForwarderManager(cfg, fs, st, nil, nil)

	ih := btcommon.InfoHash("pending-test-hash01")
	forwarderName := "test-tracker"
	peerID := btcommon.PeerID("peer-00000000000000")

	// Initially not pending
	if fm.isJobPending(ih, forwarderName, peerID) {
		t.Error("job should not be pending initially")
	}

	// Mark as pending
	fm.markJobPending(ih, forwarderName, peerID)
	if !fm.isJobPending(ih, forwarderName, peerID) {
		t.Error("job should be pending after marking")
	}

	// Unmark
	fm.unmarkJobPending(ih, forwarderName, peerID)
	if fm.isJobPending(ih, forwarderName, peerID) {
		t.Error("job should not be pending after unmarking")
	}
}

func TestShouldSuspendForwarder(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		err        error
		expected   bool
	}{
		{"429 Too Many Requests", 429, nil, true},
		{"503 Service Unavailable", 503, nil, false}, // Only 429 triggers suspend
		{"200 OK", 200, nil, false},
		{"404 Not Found", 404, nil, false},
		{"500 Internal Server Error", 500, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldSuspendForwarder(tt.statusCode, tt.err)
			if result != tt.expected {
				t.Errorf("shouldSuspendForwarder(%d, %v) = %v, want %v",
					tt.statusCode, tt.err, result, tt.expected)
			}
		})
	}
}
