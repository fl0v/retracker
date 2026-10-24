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

	corecommon "github.com/fl0v/retracker/common"
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
