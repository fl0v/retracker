package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigurationPreservesDefaultsAndWrapsErrors(t *testing.T) {
	cfg := Config{Listen: ":6969", AnnounceInterval: 1800}
	if err := cfg.LoadFromFile(""); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(file, []byte("udp_listen: ':7000'\ndebug: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadFromFile(file); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":6969" || cfg.AnnounceInterval != 1800 || cfg.UDPListen != ":7000" || !cfg.Debug {
		t.Fatalf("configuration defaults lost: %+v", cfg)
	}
	err := cfg.LoadFromFile(file + ".missing")
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "open config") {
		t.Fatalf("missing error context or cause: %v", err)
	}
	if err := os.WriteFile(file, []byte("listen: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadFromFile(file); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("invalid YAML accepted: %v", err)
	}
}

func TestReloadForwardersKeepsPreviousConfigurationOnFailure(t *testing.T) {
	var cfg Config
	file := filepath.Join(t.TempDir(), "forwarders.yml")
	if err := os.WriteFile(file, []byte("- uri: udp://tracker.example.com:6969/announce\n  name: example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ReloadForwards(file); err != nil {
		t.Fatal(err)
	}
	if cfg.ForwardersCount() != 1 || cfg.ForwardsFile != file || cfg.Forwards[0].GetName() != "example" {
		t.Fatalf("forwarders not loaded: %+v", cfg)
	}
	if err := cfg.ReloadForwards(file + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost open error cause: %v", err)
	}
	if err := os.WriteFile(file, []byte("- uri: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ReloadForwards(file); err == nil || !strings.Contains(err.Error(), "parse forwarders") {
		t.Fatalf("invalid forwarders accepted: %v", err)
	}
	if cfg.ForwardersCount() != 1 || cfg.Forwards[0].GetName() != "example" {
		t.Fatal("failed reload replaced existing forwarders")
	}
	encoded, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stats map[string]any
	if err := json.Unmarshal(encoded, &stats); err != nil {
		t.Fatal(err)
	}
	if stats["forwarders_count"] != float64(1) || stats["Forwards"] != nil {
		t.Fatalf("incorrect public config statistics: %s", encoded)
	}
}
