package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadableTextRecordsKeepFieldsAndGroups(t *testing.T) {
	var output bytes.Buffer
	logger := NewTextLogger(&output, true).With("instance", "one").WithGroup("tracker").With("name", "upstream")
	record := slog.NewRecord(time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC), slog.LevelWarn, "Announce failed\n\twill retry\r\n", 0)
	record.AddAttrs(slog.Int("attempt", 2), slog.Any("error", errors.New("first line\nsecond line")))
	if err := logger.Handler().Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := "WARN Announce failed will retry instance=one tracker.name=upstream tracker.attempt=2 tracker.error=\"first line second line\"\n"
	if got := output.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadableTextLevelsAndEmptyRecords(t *testing.T) {
	var output bytes.Buffer
	logger := NewTextLogger(&output, false)
	logger.Debug("hidden")
	if output.Len() != 0 {
		t.Fatal("debug logging enabled by default")
	}
	logger = NewTextLogger(&output, true)
	logger.Debug("visible", "duration", time.Second, "enabled", true)
	if !strings.Contains(output.String(), "DEBUG visible duration=1s enabled=true\n") {
		t.Fatalf("bad debug output: %q", output.String())
	}
	output.Reset()
	logger = logger.WithGroup("").With()
	for _, record := range []slog.Record{slog.NewRecord(time.Time{}, slog.LevelInfo, "First", 0), slog.NewRecord(time.Time{}, slog.LevelInfo, "Second", 0)} {
		if err := logger.Handler().Handle(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); got != "INFO First\nINFO Second\n" {
		t.Fatalf("empty records formatted incorrectly: %q", got)
	}
}

func TestReadableTextConcurrentDerivedLoggers(t *testing.T) {
	var output bytes.Buffer
	logger := NewTextLogger(&output, false)
	var writers sync.WaitGroup
	for worker := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			child := logger.With("worker", worker).WithGroup("job")
			for number := range 20 {
				child.Info("Completed", "number", number)
			}
		}()
	}
	writers.Wait()
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 160 {
		t.Fatalf("got %d physical lines, want 160", len(lines))
	}
	for _, line := range lines {
		if strings.Count(line, "INFO Completed") != 1 || !strings.Contains(line, "worker=") || !strings.Contains(line, "job.number=") {
			t.Fatalf("interleaved record: %q", line)
		}
	}
}

type failedLogWriter struct{ err error }

func (writer failedLogWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestReadableTextPropagatesWriterErrors(t *testing.T) {
	want := errors.New("log destination failed")
	logger := NewTextLogger(failedLogWriter{err: want}, false)
	if err := logger.Handler().Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelError, "Failed", 0)); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
