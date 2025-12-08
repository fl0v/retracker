package server

import (
	"testing"

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
