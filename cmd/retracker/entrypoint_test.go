package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEntrypointPreservesArguments(t *testing.T) {
	entrypoint, err := filepath.Abs("../../docker/docker-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"environment handled by binary", nil, ""},
		{"flags", []string{"-l", ":9000", "-d=false", "-u="}, "-l\n:9000\n-d=false\n-u=\n"},
		{"explicit binary and spaced path", []string{"./retracker", "-c", "config with spaces.yml"}, "-c\nconfig with spaces.yml\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := "#!/bin/sh\nfor arg do printf '%s\\n' \"$arg\"; done\n"
			if writeErr := os.WriteFile(filepath.Join(dir, "retracker"), []byte(stub), 0o700); writeErr != nil {
				t.Fatal(writeErr)
			}
			cmd := exec.Command("sh", append([]string{entrypoint}, tc.args...)...)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "RETRACKER_UPDATE_FORWARDERS=false", "RETRACKER_LISTEN=:8000", "RETRACKER_DEBUG=true"}
			output, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("entrypoint failed: %v: %s", runErr, output)
			}
			if string(output) != tc.want {
				t.Fatalf("got arguments %q, want %q", output, tc.want)
			}
		})
	}
}

func TestEntrypointExplicitCommand(t *testing.T) {
	cmd := exec.Command("sh", "../../docker/docker-entrypoint.sh", "printf", "%s", "custom command")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "RETRACKER_UPDATE_FORWARDERS=false"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint failed: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != "custom command" {
		t.Fatalf("explicit command was not preserved: %q", output)
	}
}
