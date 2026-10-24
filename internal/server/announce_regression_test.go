package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

func TestParseRemoteAddr(t *testing.T) {
	ra := &ReceiverAnnounce{}
	for _, tc := range []struct{ input, want string }{
		{"192.0.2.1:6881", "192.0.2.1"},
		{"[2001:4860::1]:6881", "2001:4860::1"},
		{"invalid", "127.0.0.1"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := ra.parseRemoteAddr(tc.input, "127.0.0.1")
			if got != tc.want || net.ParseIP(got) == nil {
				t.Fatalf("got %q, want valid IP %q", got, tc.want)
			}
		})
	}
}

func TestBusyRetryInterval(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retry, want int
	}{
		{"configured", 120, 120},
		{"default", 0, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{AnnounceInterval: 1800, RetryPeriod: tc.retry, ForwarderQueueSize: 1, QueueRateLimitThreshold: 80}
			storage := &Storage{Config: cfg, Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request)}
			fs := NewForwarderStorage()
			fm := NewForwarderManager(cfg, fs, storage, nil, nil)
			defer fm.udpForwarder.Stop()
			fm.rateLimiter = &tokenBucket{lastTick: time.Now()}
			fm.jobQueue <- AnnounceJob{}
			ra := NewReceiverAnnounce(cfg, storage, fs, fm)
			resp, failure := ra.ProcessAnnounce("192.0.2.1", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1", "", "50", "started", "", "", "")
			if failure != "" || resp == nil {
				t.Fatalf("invalid response: %v, %q", resp, failure)
			}
			if resp.FailureReason == "" || resp.Interval != tc.want || resp.MinInterval != tc.want {
				t.Fatalf("expected busy response with retry %d, got %+v", tc.want, resp)
			}
		})
	}
}
