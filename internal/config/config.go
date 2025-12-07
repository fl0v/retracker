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
	AnnounceResponseInterval int
	Listen                   string
	UDPListen                string
	Debug                    bool
	Age                      float64
	XRealIP                  bool
	Forwards                 []common.Forward
	ForwardsFile             string // Path to forwards YAML file (if loaded)
	ForwardTimeout           int
	ForwarderWorkers         int
	StatsInterval            int
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
func (config *Config) PrintConfig() {
	config.PrintConfigWithPrometheus(false)
}

// PrintConfigWithPrometheus prints all configuration settings including Prometheus status
func (config *Config) PrintConfigWithPrometheus(enablePrometheus bool) {
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
	fmt.Printf("Prometheus Metrics: %v\n", enablePrometheus)
	fmt.Printf("Peer Age (minutes): %.1f\n", config.Age)
	fmt.Printf("Announce Response Interval: %d seconds\n", config.AnnounceResponseInterval)
	fmt.Printf("Statistics Interval: %d seconds\n", config.StatsInterval)

	if len(config.Forwards) > 0 {
		fmt.Printf("Forwarders: %d configured\n", len(config.Forwards))
		if config.ForwardsFile != "" {
			fmt.Printf("  Forwards File: %s\n", config.ForwardsFile)
		}
		fmt.Printf("  Forward Timeout: %d seconds\n", config.ForwardTimeout)
		fmt.Printf("  Forwarder Workers: %d\n", config.ForwarderWorkers)
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
