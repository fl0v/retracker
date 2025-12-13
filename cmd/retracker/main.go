package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
	"github.com/fl0v/retracker/internal/server"
)

const VERSION = `0.10.0`

var (
	ErrorLog = log.New(os.Stderr, `error#`, log.Lshortfile)
	DebugLog = log.New(os.Stdout, `debug#`, log.Lshortfile)

	//go:embed favicon.ico
	faviconIco []byte
)

func envString(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "true" || v == "1"
	}
	return def
}

func helpText() {
	fmt.Println("# https://github.com/fl0v/retracker")
	flag.PrintDefaults()
}

func main() {
	configFile := flag.String("c", "", "Path to configuration file (YAML)")
	listen := flag.String("l", ":6969", "Listen address:port for HTTP (overrides config file, default: :6969)")
	udpListen := flag.String("u", "", "Listen address:port for UDP (empty to disable, overrides config file)")
	debug := flag.Bool("d", false, "Debug mode (overrides config file)")
	forwards := flag.String("f", "", "Load forwards from YAML file")
	ver := flag.Bool("v", false, "Show version")
	help := flag.Bool("h", false, "print this help")
	flag.Parse()

	if *help {
		helpText()
		os.Exit(0)
	}

	if *ver {
		fmt.Println(VERSION)
		os.Exit(0)
	}

	fmt.Printf("Starting version %s\n", VERSION)

	// Initialize config with defaults
	cfg := config.Config{
		Listen:                  ":6969",
		UDPListen:               "",
		Debug:                   false,
		Age:                     180,
		XRealIP:                 false,
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
		PrometheusEnabled:       false,
	}

	// Step 1: Load from config file (if provided via flag or env var)
	configPath := envString("RETRACKER_CONFIG", *configFile)
	if configPath != "" {
		if err := cfg.LoadFromFile(configPath); err != nil {
			ErrorLog.Fatalln("Failed to load config file:", err.Error())
		}
	}

	// Step 2: Override with environment variables
	if v := envString("RETRACKER_LISTEN", ""); v != "" {
		cfg.Listen = v
	}
	if v := envString("RETRACKER_UDP_LISTEN", ""); v != "" {
		cfg.UDPListen = v
	}
	if v := envBool("RETRACKER_DEBUG", false); v {
		cfg.Debug = v
	}

	// Step 3: Override with command-line flags (highest priority)
	if *listen != "" {
		cfg.Listen = *listen
	}
	// For UDP, empty string is a valid value (to disable), so we check if flag was provided
	// We'll use a pointer approach or check against a sentinel value
	// Since we can't easily detect if flag was set, we'll check if it differs from config file value
	// For now, if flag is explicitly set (even to empty), we use it
	udpListenSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "u" {
			udpListenSet = true
		}
	})
	if udpListenSet {
		cfg.UDPListen = *udpListen
	}
	debugSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "d" {
			debugSet = true
		}
	})
	if debugSet {
		cfg.Debug = *debug
	}

	// Validate announce interval
	if cfg.AnnounceInterval <= 0 {
		cfg.AnnounceInterval = 1800 // Default to 30 minutes
	}
	// Validate retry period
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = 300 // Default to 5 minutes
	}

	if *forwards != `` {
		if err := cfg.ReloadForwards(*forwards); err != nil {
			ErrorLog.Fatalln(err.Error())
		}
	}

	tempStorage, err := server.NewTempStorage(``)
	if err != nil {
		os.Exit(1)
	}

	core := server.NewCore(&cfg, tempStorage)

	// https://github.com/vvampirius/retracker/issues/7
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(faviconIco)))
		w.Write(faviconIco)
	})

	http.HandleFunc("/scrape", core.HTTPScrapeHandler)
	http.HandleFunc("/announce", core.Receiver.Announce.HTTPHandler)
	http.HandleFunc("/stats", core.HTTPStatsHandler)
	if cfg.PrometheusEnabled {
		p, err := observability.NewPrometheus()
		if err != nil {
			os.Exit(1)
		}
		core.Receiver.Announce.Prometheus = p
		core.Receiver.UDP.Prometheus = p
		if core.ForwarderManager != nil {
			core.ForwarderManager.Prometheus = p
		}
	}

	// Print configuration (after all setup is complete)
	cfg.PrintConfig()

	// Start UDP server if configured
	if cfg.UDPListen != "" {
		core.Receiver.UDP.TempStorage = tempStorage
		if err := core.Receiver.UDP.Start(cfg.UDPListen); err != nil {
			ErrorLog.Fatalln("Failed to start UDP server:", err.Error())
		}
		defer core.Receiver.UDP.Close()
	}

	fmt.Printf("HTTP tracker listening on %s\n", cfg.Listen)
	fmt.Println("Server ready to accept connections")
	fmt.Println()

	if err := http.ListenAndServe(cfg.Listen, nil); err != nil { // set listen port
		ErrorLog.Println(err)
	}
}
