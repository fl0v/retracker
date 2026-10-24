package main

import (
	"flag"
	"fmt"
	"strconv"

	"github.com/fl0v/retracker/internal/config"
)

func registerConfigFlags(flags *flag.FlagSet) {
	flags.String("c", "", "Path to configuration file (YAML)")
	flags.String("l", ":6969", "Listen address:port for HTTP (overrides config file, default: :6969)")
	flags.String("u", "", "Listen address:port for UDP (empty to disable, overrides config file)")
	flags.Bool("d", false, "Debug mode (overrides config file)")
	flags.String("f", "", "Load forwards from YAML file")
}

func defaultConfig() config.Config {
	return config.Config{
		Listen:                  ":6969",
		Age:                     180,
		ForwardTimeout:          30,
		ForwarderWorkers:        10,
		MaxForwarderWorkers:     20,
		ForwarderQueueSize:      10000,
		QueueScaleThresholdPct:  60,
		QueueRateLimitThreshold: 80,
		QueueThrottleThreshold:  60,
		QueueThrottleTopN:       20,
		RateLimitInitialPerSec:  10,
		RateLimitInitialBurst:   200,
		ForwarderSuspendSeconds: 300,
		ForwarderFailThreshold:  10,
		ForwarderRetryAttempts:  5,
		ForwarderRetryBaseMs:    500,
		AnnounceInterval:        1800,
		RetryPeriod:             300,
		StatsInterval:           60,
	}
}

// loadConfig applies defaults, YAML, environment, and explicitly supplied flags.
// The caller parses flags first; loading configuration starts no background work.
func loadConfig(flags *flag.FlagSet, lookupEnv func(string) (string, bool)) (config.Config, error) {
	cfg := defaultConfig()
	overrides := make(map[string]string)
	flags.Visit(func(f *flag.Flag) { overrides[f.Name] = f.Value.String() })

	configPath, _ := lookupEnv("RETRACKER_CONFIG")
	if value, supplied := overrides["c"]; supplied {
		configPath = value
	}
	if err := cfg.LoadFromFile(configPath); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}

	if value, _ := lookupEnv("RETRACKER_LISTEN"); value != "" {
		cfg.Listen = value
	}
	if value, present := lookupEnv("RETRACKER_UDP_LISTEN"); present {
		cfg.UDPListen = value
	}
	if value, _ := lookupEnv("RETRACKER_DEBUG"); value != "" {
		debug, err := strconv.ParseBool(value)
		if err != nil {
			return cfg, fmt.Errorf("parse RETRACKER_DEBUG: %w", err)
		}
		cfg.Debug = debug
	}

	if value, supplied := overrides["l"]; supplied {
		cfg.Listen = value
	}
	if value, supplied := overrides["u"]; supplied {
		cfg.UDPListen = value
	}
	if value, supplied := overrides["d"]; supplied {
		debug, err := strconv.ParseBool(value)
		if err != nil {
			return cfg, fmt.Errorf("parse debug flag: %w", err)
		}
		cfg.Debug = debug
	}

	if cfg.AnnounceInterval <= 0 {
		cfg.AnnounceInterval = 1800
	}
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = 300
	}
	forwardsPath, _ := lookupEnv("RETRACKER_FORWARDS")
	if value, supplied := overrides["f"]; supplied {
		forwardsPath = value
	}
	if forwardsPath != "" {
		if err := cfg.ReloadForwards(forwardsPath); err != nil {
			return cfg, fmt.Errorf("load forwarders: %w", err)
		}
	}
	return cfg, nil
}
