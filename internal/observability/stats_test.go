package observability_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

func TestStatsTextReportsForwardersAndPeers(t *testing.T) {
	stats := &observability.Stats{
		Config: &config.Config{
			Listen: ":7000", UDPListen: ":7001", TrackerID: "test-tracker", PrometheusEnabled: true,
			ForwardsFile: "forwarders.yml", Forwards: []common.Forward{{Uri: "https://tracker.example/announce"}},
		},
		QueueDepth: 7, QueueCapacity: 10, QueueFillPct: 70, ActiveWorkers: 2, MaxWorkers: 4,
		Forwarders: []observability.ForwarderStat{
			{Name: "responsive", Protocol: "udp", HasStats: true, AvgResponseTime: 150 * time.Millisecond, LastInterval: 1800, SampleCount: 3},
			{Name: "waiting"},
		},
		DisabledDetails:  []observability.DisabledForwarderInfo{{Name: "disabled", FailureCount: 5, Reason: "connection refused after repeated attempts", DisabledAtUnix: 1700000000}},
		SuspendedDetails: []observability.SuspendedForwarderInfo{{Name: "suspended", Reason: strings.Repeat("retry later ", 10), SecondsRemaining: 120}},
		HashPeerStats: map[string]observability.HashPeerStat{
			"bbbb": {Complete: 2, Incomplete: 3, LocalUnique: 4, ForwarderUnique: 5},
			"aaaa": {Complete: 1},
		},
		ClientStats: &observability.ClientStats{ActiveClients: 1, Clients: []observability.ClientInfo{{Key: "192.0.2.1:qBittorrent", AnnouncedHashes: 2, SecondsSinceLastReq: 10}}},
	}
	output := observability.NewStatsCollector().FormatText(stats)
	for _, want := range []string{"HTTP Listen: :7000", "UDP Listen: :7001", "Tracker ID: test-tracker", "Prometheus Metrics: true", "forwarders.yml", "Queue: depth 7 / 10 (70%)", "Workers: 2 active / 4 max", "responsive", "150ms", "waiting", "connection refused after repeated attempts", "Suspended Forwarders:", "retry later", "192.0.2.1:qBittorrent"} {
		if !strings.Contains(output, want) {
			t.Errorf("stats output missing %q", want)
		}
	}
	if strings.Index(output, "aaaa") >= strings.Index(output, "bbbb") {
		t.Fatal("hash rows are not sorted")
	}
}

func TestStatsTextEmptyAndWrappedReasons(t *testing.T) {
	collector := observability.NewStatsCollector()
	for _, stats := range []*observability.Stats{
		{},
		{Config: &config.Config{}, ClientStats: &observability.ClientStats{}},
	} {
		output := collector.FormatText(stats)
		if !strings.Contains(output, "Suspended Forwarders:\n(none)") || strings.Contains(output, "%!") {
			t.Fatalf("invalid empty stats output: %s", output)
		}
		if stats.Config != nil && (!strings.Contains(output, "UDP Listen: disabled") || !strings.Contains(output, "Forwarders: none configured") || !strings.Contains(output, "Active clients: 0")) {
			t.Fatalf("missing disabled state: %s", output)
		}
	}
	// Long words and Unicode reasons must survive wrapping without lost characters.
	for _, reason := range []string{strings.Repeat("x", 125), strings.Repeat("界", 125), strings.Repeat("temporary network failure ", 8)} {
		stats := &observability.Stats{DisabledDetails: []observability.DisabledForwarderInfo{{Name: "tracker", Reason: reason}}}
		output := collector.FormatText(stats)
		compactOutput := strings.Join(strings.Fields(output), "")
		if !utf8.ValidString(output) || !strings.Contains(compactOutput, strings.Join(strings.Fields(reason), "")) {
			t.Fatalf("wrapped reason lost content: %q", reason)
		}
	}
}

func TestStatsJSONUsesMillisecondsAndOmitsPrivateConfig(t *testing.T) {
	stats := &observability.Stats{
		Config:     &config.Config{Listen: ":7000", RetryPeriod: 12345, Forwards: []common.Forward{{Uri: "https://secret.example/announce"}}},
		QueueDepth: 7, RateLimited: 3,
		Forwarders: []observability.ForwarderStat{{Name: "tracker", AvgResponseTime: 1500*time.Millisecond + time.Microsecond, LastInterval: 1800, HasStats: true}},
	}
	payload, err := observability.NewStatsCollector().FormatJSON(stats)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Config      map[string]json.RawMessage        `json:"config"`
		Forwarders  []observability.ForwarderStatJSON `json:"forwarders"`
		QueueDepth  int                               `json:"queue_depth"`
		RateLimited int                               `json:"rate_limited"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Forwarders) != 1 || decoded.Forwarders[0].AvgResponseTime != 1500 || decoded.Forwarders[0].LastInterval != 1800 || decoded.QueueDepth != 7 || decoded.RateLimited != 3 {
		t.Fatalf("incorrect JSON values: %s", payload)
	}
	if string(decoded.Config["forwarders_count"]) != "1" {
		t.Fatalf("missing forwarder count: %s", payload)
	}
	for _, key := range []string{"retry_period", "forwards"} {
		if _, present := decoded.Config[key]; present {
			t.Errorf("private config field %q exposed", key)
		}
	}
	if strings.Contains(string(payload), "secret.example") {
		t.Fatal("forwarder URL leaked into stats configuration")
	}
}
