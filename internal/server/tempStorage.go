// tempStorage.go - Diagnostic response files and stoppable age-based cleanup.
package server

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"time"

	"github.com/fl0v/retracker/internal/observability"
)

type TempStorage struct {
	observability.Logging
	path  string
	tasks *backgroundTasks
}

func (ts *TempStorage) cleanRoutine() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if ts.tasks.ctx.Err() != nil {
			return
		}
		ts.cleanExpired()
		select {
		case <-ts.tasks.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (ts *TempStorage) cleanExpired() {
	entries, err := os.ReadDir(ts.path)
	if err != nil {
		ts.Log().Error("Read diagnostic directory", "path", ts.path, "error", err)
		return
	}
	now := time.Now()
	deadline := now.AddDate(0, 0, -1)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			ts.Log().Error("Read diagnostic file metadata", "path", ts.path, "error", err)
			continue
		}
		if info.ModTime().Before(deadline) {
			if err := os.Remove(path.Join(ts.path, entry.Name())); err != nil {
				ts.Log().Error("Remove expired diagnostic file", "path", ts.path, "error", err)
			}
		}
	}
}

func (ts *TempStorage) SaveBencodeFromForwarder(p []byte, hash string, uri string) string {
	f, err := os.CreateTemp(ts.path, fmt.Sprintf("%s_", hash))
	if err != nil {
		ts.Log().Error("Create diagnostic file", "path", ts.path, "error", err)
		return ``
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n%s\n", hash, uri); err != nil {
		ts.Log().Error("Write diagnostic metadata", "path", ts.path, "error", err)
		return f.Name()
	}
	if _, err := f.Write(p); err != nil {
		ts.Log().Error("Write diagnostic payload", "path", ts.path, "error", err)
	}
	return f.Name()
}

func NewTempStorage(_path string, loggers ...*slog.Logger) (*TempStorage, error) {
	ts := TempStorage{
		Logging: observability.NewLogging(false, loggers...),
		path:    _path,
		tasks:   newBackgroundTasks(),
	}
	if ts.path == `` {
		ts.path = path.Join(os.TempDir(), `retracker`)
	}
	if err := os.MkdirAll(ts.path, 0o755); err != nil {
		ts.tasks.close()
		ts.Log().Error("Create diagnostic directory", "path", ts.path, "error", err)
		return nil, err
	}
	ts.tasks.run(ts.cleanRoutine)
	return &ts, nil
}

func (ts *TempStorage) Close() error {
	if ts.tasks != nil {
		ts.tasks.close()
	}
	return nil
}
