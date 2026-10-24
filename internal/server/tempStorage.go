// tempStorage.go - Diagnostic response files and stoppable age-based cleanup.
package server

import (
	"fmt"
	"log"
	"os"
	"path"
	"time"
)

var ErrorLog = log.New(os.Stderr, `error#`, log.Lshortfile)

type TempStorage struct {
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
		ErrorLog.Println(err.Error())
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
			ErrorLog.Println(err.Error())
			continue
		}
		if info.ModTime().Before(deadline) {
			if err := os.Remove(path.Join(ts.path, entry.Name())); err != nil {
				ErrorLog.Println(err.Error())
			}
		}
	}
}

func (ts *TempStorage) SaveBencodeFromForwarder(p []byte, hash string, uri string) string {
	f, err := os.CreateTemp(ts.path, fmt.Sprintf("%s_", hash))
	if err != nil {
		ErrorLog.Println(err.Error())
		return ``
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n%s\n", hash, uri); err != nil {
		ErrorLog.Println(err.Error())
		return f.Name()
	}
	if _, err := f.Write(p); err != nil {
		ErrorLog.Println(err.Error())
	}
	return f.Name()
}

func NewTempStorage(_path string) (*TempStorage, error) {
	ts := TempStorage{
		path:  _path,
		tasks: newBackgroundTasks(),
	}
	if ts.path == `` {
		ts.path = path.Join(os.TempDir(), `retracker`)
	}
	if err := os.MkdirAll(ts.path, 0o755); err != nil {
		ts.tasks.close()
		ErrorLog.Println(err.Error())
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
