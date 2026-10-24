package observability

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestLoggingLevelsAndInjection(t *testing.T) {
	for _, debug := range []bool{false, true} {
		logger := NewLogging(debug).Log()
		if got := logger.Enabled(context.Background(), slog.LevelDebug); got != debug {
			t.Fatalf("debug enabled=%v, want %v", got, debug)
		}
		if !logger.Enabled(context.Background(), slog.LevelError) {
			t.Fatal("errors disabled")
		}
	}
	var output bytes.Buffer
	injected := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})).With("instance", "one")
	logger := NewLogging(false, injected).Log()
	if logger != injected {
		t.Fatal("injected logger replaced")
	}
	logger.Debug("request", "hash", "abc")
	if !bytes.Contains(output.Bytes(), []byte(`"instance":"one"`)) || !bytes.Contains(output.Bytes(), []byte(`"hash":"abc"`)) {
		t.Fatalf("missing structured attributes: %s", &output)
	}
	var zero Logging
	if zero.Log().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("zero-value logger enables debug")
	}
	if NewLogging(false, nil).Log() == nil {
		t.Fatal("nil injection did not select a default")
	}
}
