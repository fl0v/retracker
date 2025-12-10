package config

import (
	"fmt"
	"log"
	"os"

	gopkginyaml "gopkg.in/yaml.v2"

	"github.com/fl0v/retracker/common"
)

var (
	ErrorLog = log.New(os.Stderr, `error#`, log.Lshortfile)
	DebugLog = log.New(os.Stdout, `debug#`, log.Lshortfile)
)

type Config struct {
	AnnounceInterval        int              `yaml:"announce_interval"`
	RetryPeriod             int              `yaml:"retry_period"`
	TrackerID               string           `yaml:"tracker_id"`
	Listen                  string           `yaml:"listen"`
	UDPListen               string           `yaml:"udp_listen"`
	Debug                   bool             `yaml:"debug"`
	Age                     float64          `yaml:"age"`
	XRealIP                 bool             `yaml:"x_real_ip"`
	Forwards                []common.Forward `yaml:"-"` // Not loaded from main config file
	ForwardsFile            string           `yaml:"-"` // Path to forwards YAML file (if loaded)
	ForwardTimeout          int              `yaml:"forward_timeout"`
	ForwarderWorkers        int              `yaml:"forwarder_workers"`
	ForwarderQueueSize      int              `yaml:"forwarder_queue_size"`
	MaxForwarderWorkers     int              `yaml:"max_forwarder_workers"`
	QueueScaleThresholdPct  int              `yaml:"queue_scale_threshold_pct"`
	QueueRateLimitThreshold int              `yaml:"queue_rate_limit_threshold"`
	QueueThrottleThreshold  int              `yaml:"queue_throttle_threshold"`
	QueueThrottleTopN       int              `yaml:"queue_throttle_top_n"`
	RateLimitInitialPerSec  int              `yaml:"rate_limit_initial_per_sec"`
	RateLimitInitialBurst   int              `yaml:"rate_limit_initial_burst"`
	ForwarderSuspendSeconds int              `yaml:"forwarder_suspend_seconds"`
	ForwarderFailThreshold  int              `yaml:"forwarder_fail_threshold"`
	ForwarderRetryAttempts  int              `yaml:"forwarder_retry_attempts"`
	ForwarderRetryBaseMs    int              `yaml:"forwarder_retry_base_ms"`
	StatsInterval           int              `yaml:"stats_interval"`
	PrometheusEnabled       bool             `yaml:"prometheus_enabled"`
}

// LoadFromFile loads configuration from a YAML file
// Missing values will use defaults (zero values for Go types)
func (config *Config) LoadFromFile(path string) error {
	if path == "" {
		return nil // No config file specified, use defaults
	}

	f, err := os.Open(path)
	if err != nil {
		ErrorLog.Printf("Failed to open config file '%s': %s\n", path, err.Error())
		return err
	}
	defer f.Close()

	decoder := gopkginyaml.NewDecoder(f)
	if err := decoder.Decode(config); err != nil {
		ErrorLog.Printf("Failed to parse config file '%s': %s\n", path, err.Error())
		return err
	}

	DebugLog.Printf("Loaded configuration from '%s'\n", path)
	return nil
}

func (config *Config) ReloadForwards(fileName string) error {
	f, err := os.Open(fileName)
	if err != nil {
		ErrorLog.Printf("Failed to open forwarders file '%s': %s\n", fileName, err.Error())
		return err
	}
	defer f.Close()
	forwards := make([]common.Forward, 0)
	decoder := gopkginyaml.NewDecoder(f)
	if err := decoder.Decode(&forwards); err != nil {
		ErrorLog.Printf("Failed to parse forwarders file '%s': %s\n", fileName, err.Error())
		return err
	}
	config.Forwards = forwards
	config.ForwardsFile = fileName
	DebugLog.Printf("Loaded %d forwarder(s) from '%s'\n", len(forwards), fileName)
	for i, forward := range forwards {
		forwardName := forward.GetName()
		forwardInfo := fmt.Sprintf("  [%d] %s", i+1, forwardName)
		if forward.Uri != "" {
			forwardInfo += fmt.Sprintf(" -> %s", forward.Uri)
		}
		if forward.Ip != "" {
			forwardInfo += fmt.Sprintf(" (IP: %s)", forward.Ip)
		}
		if forward.Host != "" {
			forwardInfo += fmt.Sprintf(" (Host: %s)", forward.Host)
		}
		DebugLog.Println(forwardInfo)
	}
	return nil
}

// PrintConfig prints all configuration settings in a formatted way
// Prometheus status is always displayed from Config.PrometheusEnabled
func (config *Config) PrintConfig() {
	fmt.Println("\n=== Configuration ===")
	fmt.Printf("Version: %s\n", "0.10.0") // TODO: get from main package
	fmt.Printf("HTTP Listen: %s\n", config.Listen)
	if config.UDPListen != "" {
		fmt.Printf("UDP Listen: %s\n", config.UDPListen)
	} else {
		fmt.Printf("UDP Listen: disabled\n")
	}
	fmt.Printf("Debug Mode: %v\n", config.Debug)
	fmt.Printf("X-Real-IP Header: %v\n", config.XRealIP)
	fmt.Printf("Prometheus Metrics: %v\n", config.PrometheusEnabled)
	fmt.Printf("Peer Age (minutes): %.1f\n", config.Age)
	fmt.Printf("Announce Interval: %d seconds\n", config.AnnounceInterval)
	if config.TrackerID != "" {
		fmt.Printf("Tracker ID: %s\n", config.TrackerID)
	}
	fmt.Printf("Statistics Interval: %d seconds\n", config.StatsInterval)

	if len(config.Forwards) > 0 {
		fmt.Printf("Forwarders: %d configured\n", len(config.Forwards))
		if config.ForwardsFile != "" {
			fmt.Printf("  Forwards File: %s\n", config.ForwardsFile)
		}
		fmt.Printf("  Forward Timeout: %d seconds\n", config.ForwardTimeout)
		fmt.Printf("  Forwarder Workers: %d (max %d)\n", config.ForwarderWorkers, config.MaxForwarderWorkers)
		fmt.Printf("  Forwarder Queue Size: %d\n", config.ForwarderQueueSize)
		fmt.Printf("  Queue Scale Threshold: %d%%\n", config.QueueScaleThresholdPct)
		fmt.Printf("  Queue Rate Limit Threshold: %d%%\n", config.QueueRateLimitThreshold)
		fmt.Printf("  Queue Throttle Threshold: %d%% (top %d forwarders)\n", config.QueueThrottleThreshold, config.QueueThrottleTopN)
		fmt.Printf("  Rate Limit Initial: %d/sec (burst %d)\n", config.RateLimitInitialPerSec, config.RateLimitInitialBurst)
		fmt.Printf("  Forwarder suspend: %d seconds\n", config.ForwarderSuspendSeconds)
		fmt.Printf("  Forwarder Fail Threshold: %d\n", config.ForwarderFailThreshold)
		fmt.Printf("  Forwarder Retry Attempts: %d\n", config.ForwarderRetryAttempts)
		fmt.Printf("  Forwarder Retry Base (ms): %d\n", config.ForwarderRetryBaseMs)
		fmt.Println("  Forwarder List:")
		for i, forward := range config.Forwards {
			forwardName := forward.GetName()
			forwardInfo := fmt.Sprintf("    [%d] %s", i+1, forwardName)
			if forward.Uri != "" {
				forwardInfo += fmt.Sprintf(" -> %s", forward.Uri)
			}
			if forward.Ip != "" {
				forwardInfo += fmt.Sprintf(" (IP: %s)", forward.Ip)
			}
			if forward.Host != "" {
				forwardInfo += fmt.Sprintf(" (Host: %s)", forward.Host)
			}
			fmt.Println(forwardInfo)
		}
	} else {
		fmt.Println("Forwarders: none configured")
	}
	fmt.Println("==================")
	fmt.Println()
}
