package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	corecommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/observability"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestCoreLoggerIsolationAndRuntimeForwarderInheritance(t *testing.T) {
	var outputs [2]logBuffer
	for i := range 2 {
		logger := slog.New(slog.NewJSONHandler(&outputs[i], nil)).With("instance", i)
		cfg := makeCfg()
		// Exercise both startup and runtime manager construction.
		if i == 0 {
			cfg.Forwards = []corecommon.Forward{{Uri: "udp://example.com:6969/announce"}}
		}
		core := NewCore(cfg, nil, logger)
		defer core.Close()
		if core.Log() != logger || core.Storage.Log() != logger || core.Receiver.Announce.Log() != logger || core.Receiver.UDP.Log() != logger {
			t.Fatal("core services did not inherit the injected logger")
		}
		fm := core.ForwarderManager
		if fm == nil {
			fm = core.ensureForwarderManager()
		}
		if fm.Log() != logger || fm.udpForwarder.Log() != logger {
			t.Fatal("forwarder logger was not inherited")
		}
		w := httptest.NewRecorder()
		core.Receiver.Announce.HTTPHandler(w, httptest.NewRequest(http.MethodGet, "/announce?info_hash=invalid", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid announce returned %d", w.Code)
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(outputs[i].String()), &record); err != nil {
			t.Fatal(err)
		}
		if record["instance"] != float64(i) || record["level"] != "ERROR" || record["info_hash"] != "696e76616c6964" || record["error"] == "" {
			t.Fatalf("missing request error context: %v", record)
		}
	}
	if strings.Contains(outputs[0].String(), `"instance":1`) || strings.Contains(outputs[1].String(), `"instance":0`) {
		t.Fatal("logs leaked between instances")
	}
}

func TestForwarderStructuredOutcomeFields(t *testing.T) {
	var output logBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, nil, WithForwarderLogger(logger))
	defer fm.Stop()
	result := AnnounceResult{Success: true, Duration: time.Second, Interval: 1800, ResponseSize: 42}
	fm.logAnnounceResult("UDP", "udp://example.com:6969/announce", "abc", result)
	result.Success = false
	result.Error = errors.New("busy")
	fm.logAnnounceResult("UDP", "udp://example.com:6969/announce", "abc", result)
	fm.logRetryableAnnounceResult("UDP", "udp://example.com:6969/announce", "abc", result, 2, 5)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want three outcome records, got %s", output.String())
	}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["protocol"] != "UDP" || record["info_hash"] != "abc" || record["tracker"] != "udp://example.com:6969/announce" {
			t.Fatalf("missing outcome fields: %v", record)
		}
		if i > 0 && (record["error"] != "busy" || record["level"] != "WARN") {
			t.Fatalf("missing failure details: %v", record)
		}
		if i == 2 && (record["failure_count"] != float64(2) || record["failure_threshold"] != float64(5)) {
			t.Fatalf("missing retry details: %v", record)
		}
	}
}

func TestProtocolWarningUsesInjectedLogger(t *testing.T) {
	var output logBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil)).With("instance", "parser")
	core := NewCore(makeCfg(), nil, logger)
	defer core.Close()
	_, failure := core.Receiver.Announce.ProcessAnnounce("203.0.113.10", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "100", "", "10", "unknown-event", "client", "1", "0")
	if failure != "" {
		t.Fatal(failure)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "WARN" || record["instance"] != "parser" || !strings.Contains(record["msg"].(string), "unknown-event") {
		t.Fatalf("protocol warning lost context or severity: %v", record)
	}
}

func TestTempStorageUsesInjectedLogger(t *testing.T) {
	var output logBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ts, err := NewTempStorage(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	_ = ts.Close()
	if ts.Log() != logger {
		t.Fatal("diagnostic storage replaced its logger")
	}
	ts.path = filepath.Join(t.TempDir(), "missing")
	if got := ts.SaveBencodeFromForwarder(nil, "hash", "uri"); got != "" {
		t.Fatal("invalid diagnostic path unexpectedly accepted")
	}
	if !strings.Contains(output.String(), `"level":"ERROR"`) {
		t.Fatal("diagnostic error did not reach injected logger")
	}
}

func TestStatisticsLogUsesSeparateReadableRecords(t *testing.T) {
	var output logBuffer
	cfg := makeCfg()
	cfg.ForwarderWorkers = 3
	storage := NewStorage(cfg)
	defer storage.Close()
	cache := NewForwarderStorage()
	metrics, err := observability.NewPrometheus(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	fm := NewForwarderManager(cfg, cache, storage, metrics, nil, WithForwarderLogger(observability.NewTextLogger(&output, false)))
	defer fm.Stop()
	req := regressionRequest(t)
	ctx := storage.Update(req)
	fm.pendingJobs["one"] = &announceJobState{peerContext: ctx}
	fm.pendingJobs["two"] = &announceJobState{peerContext: ctx}
	fm.Forwarders = []corecommon.Forward{{Name: "upstream", Uri: "udp://8.8.8.8:6969/announce"}}
	fm.disabledForwarders["disabled"] = &DisabledForwarder{Reason: "failure", DisabledAt: time.Now().Unix(), FailureCount: 2}
	fm.suspendedForwarders["suspended"] = &SuspendedForwarder{Reason: "busy", SuspendedUntil: time.Now().Add(time.Minute)}
	fm.printStats()
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	want := []string{"Pending jobs count=2", "Tracked hashes count=1", "Disabled forwarders count=1", "Suspended forwarders count=1", "Active forwarders count=1", "Active workers count=3", "Active clients count=1"}
	if len(lines) != len(want) {
		t.Fatalf("want seven records, got %q", output.String())
	}
	for i, line := range lines {
		if line != "INFO "+want[i] || strings.Contains(line, `\n`) || strings.Contains(line, "msg=") || strings.Contains(line, "summary=") {
			t.Fatalf("bad statistic line: %q", line)
		}
	}
	families, err := metricsRegistryValues(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if families["forwarder_workers"] != 3 || families["forwarders_disabled"] != 1 || families["forwarders_suspended"] != 1 {
		t.Fatalf("statistics logging lost metric updates: %v", families)
	}
}

func metricsRegistryValues(metrics *observability.Prometheus) (map[string]float64, error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		return nil, err
	}
	values := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Gauge != nil {
				values[family.GetName()] = metric.Gauge.GetValue()
			}
		}
	}
	return values, nil
}

func TestDiagnosticLogsAreReadableSingleLines(t *testing.T) {
	var output logBuffer
	logger := observability.NewTextLogger(&output, true)
	temp, err := NewTempStorage(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer temp.Close()
	fm := NewForwarderManager(makeCfg(), NewForwarderStorage(), nil, nil, temp, WithForwarderLogger(logger))
	defer fm.Stop()
	fm.logParseError([]byte("first line\n\tsecond line\r\n"), "hash", "http://tracker.example/announce", errors.New("invalid\nbencode"))
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want three diagnostic records, got %q", output.String())
	}
	if strings.Contains(output.String(), `\n`) || strings.Contains(output.String(), `\t`) || strings.Contains(output.String(), "msg=") {
		t.Fatalf("escaped formatting remains: %q", output.String())
	}
	if !strings.Contains(lines[0], "Failed to parse response: invalid bencode") || !strings.Contains(lines[1], "Raw response data: first line second line") || !strings.Contains(lines[2], "Response saved to:") {
		t.Fatalf("diagnostic information lost: %q", lines)
	}
}
