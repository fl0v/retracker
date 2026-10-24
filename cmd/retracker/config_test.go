package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseConfigFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	flags := flag.NewFlagSet("retracker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	registerConfigFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return flags
}

func configEnvironment(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, present := values[key]
		return value, present
	}
}

func writeConfigFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config with spaces.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigPrecedence(t *testing.T) {
	const yamlConfig = "listen: ':7000'\nudp_listen: ':7001'\ndebug: true\nage: 42\n"
	for _, tc := range []struct {
		name   string
		yaml   string
		env    map[string]string
		args   []string
		listen string
		udp    string
		debug  bool
	}{
		{name: "defaults", listen: ":6969"},
		{name: "YAML survives unset flags", yaml: yamlConfig, listen: ":7000", udp: ":7001", debug: true},
		{name: "environment HTTP without YAML", env: map[string]string{"RETRACKER_LISTEN": ":8000"}, listen: ":8000"},
		{name: "environment overrides YAML", yaml: yamlConfig, env: map[string]string{"RETRACKER_LISTEN": ":8000", "RETRACKER_UDP_LISTEN": ":8001", "RETRACKER_DEBUG": "false"}, listen: ":8000", udp: ":8001"},
		{name: "empty environment disables UDP", yaml: yamlConfig, env: map[string]string{"RETRACKER_UDP_LISTEN": ""}, listen: ":7000", debug: true},
		{name: "CLI overrides environment", yaml: yamlConfig, env: map[string]string{"RETRACKER_LISTEN": ":8000", "RETRACKER_UDP_LISTEN": ":8001", "RETRACKER_DEBUG": "true"}, args: []string{"-l", ":9000", "-u=", "-d=false"}, listen: ":9000"},
		{name: "explicit default HTTP flag overrides YAML", yaml: yamlConfig, args: []string{"-l", ":6969"}, listen: ":6969", udp: ":7001", debug: true},
		{name: "CLI enables debug over environment false", env: map[string]string{"RETRACKER_DEBUG": "false"}, args: []string{"-d"}, listen: ":6969", debug: true},
		{name: "environment zero disables debug", yaml: yamlConfig, env: map[string]string{"RETRACKER_DEBUG": "0"}, listen: ":7000", udp: ":7001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.yaml != "" {
				args = append(args, "-c", writeConfigFixture(t, tc.yaml))
			}
			args = append(args, tc.args...)
			cfg, err := loadConfig(parseConfigFlags(t, args...), configEnvironment(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Listen != tc.listen || cfg.UDPListen != tc.udp || cfg.Debug != tc.debug {
				t.Fatalf("got listen=%q UDP=%q debug=%t; want %q %q %t", cfg.Listen, cfg.UDPListen, cfg.Debug, tc.listen, tc.udp, tc.debug)
			}
			if cfg.AnnounceInterval != 1800 || cfg.RetryPeriod != 300 || cfg.ForwarderWorkers != 10 {
				t.Fatal("omitted settings did not preserve defaults")
			}
			if tc.yaml != "" && cfg.Age != 42 {
				t.Fatal("unrelated YAML setting was overridden")
			}
		})
	}
}

func TestConfigFileSelection(t *testing.T) {
	envPath := writeConfigFixture(t, "listen: ':7000'\n")
	flagPath := writeConfigFixture(t, "listen: ':8000'\n")
	for _, tc := range []struct {
		name   string
		args   []string
		listen string
	}{
		{"environment", nil, ":7000"},
		{"explicit flag", []string{"-c", flagPath}, ":8000"},
		{"explicit empty flag", []string{"-c="}, ":6969"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(parseConfigFlags(t, tc.args...), configEnvironment(map[string]string{"RETRACKER_CONFIG": envPath}))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Listen != tc.listen {
				t.Fatalf("got %q, want %q", cfg.Listen, tc.listen)
			}
		})
	}
}

func TestForwarderFileSelection(t *testing.T) {
	envPath := writeConfigFixture(t, "- uri: 'https://env.example/announce'\n")
	flagPath := writeConfigFixture(t, "- uri: 'udp://cli.example:6969/announce'\n")
	for _, tc := range []struct {
		name string
		args []string
		uri  string
	}{
		{"environment", nil, "https://env.example/announce"},
		{"explicit flag", []string{"-f", flagPath}, "udp://cli.example:6969/announce"},
		{"explicit empty flag", []string{"-f="}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(parseConfigFlags(t, tc.args...), configEnvironment(map[string]string{"RETRACKER_FORWARDS": envPath}))
			if err != nil {
				t.Fatal(err)
			}
			if tc.uri == "" {
				if len(cfg.Forwards) != 0 || cfg.ForwardsFile != "" {
					t.Fatal("explicit empty flag did not disable forwarders")
				}
			} else if len(cfg.Forwards) != 1 || cfg.Forwards[0].Uri != tc.uri {
				t.Fatalf("unexpected forwarders: %+v", cfg.Forwards)
			}
		})
	}
}

func TestConfigErrors(t *testing.T) {
	badYAML := writeConfigFixture(t, "listen: [\n")
	missing := filepath.Join(t.TempDir(), "missing.yml")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"invalid debug environment", nil, map[string]string{"RETRACKER_DEBUG": "invalid"}, "RETRACKER_DEBUG"},
		{"missing config", []string{"-c", missing}, nil, "load config"},
		{"malformed config", []string{"-c", badYAML}, nil, "load config"},
		{"missing forwarders", []string{"-f", missing}, nil, "load forwarders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(parseConfigFlags(t, tc.args...), configEnvironment(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}
