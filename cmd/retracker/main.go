package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

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

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return 0
}

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

func envFloat64(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			return parsed
		}
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
	age := flag.Float64("a", 0, "Keep 'n' minutes peer in memory (overrides config file)")
	debug := flag.Bool("d", false, "Debug mode (overrides config file)")
	xrealip := flag.Bool("x", false, "Get RemoteAddr from X-Real-IP header (overrides config file)")
	forwards := flag.String("f", "", "Load forwards from YAML file")
	forwardTimeout := flag.Int("t", 0, "Timeout (sec) for forward requests (overrides config file)")
	forwarderWorkers := flag.Int("w", 0, "Number of workers for parallel forwarder processing (overrides config file)")
	maxForwarderWorkers := flag.Int("W", 0, "Maximum workers for parallel forwarder processing (overrides config file)")
	forwarderQueueSize := flag.Int("Q", 0, "Forwarder announce job queue size (overrides config file)")
	queueScaleThreshold := flag.Int("queue-scale-threshold", 0, "Queue fill %% to trigger worker scaling (overrides config file)")
	queueRateLimitThreshold := flag.Int("queue-rate-limit-threshold", 0, "Queue fill %% to trigger initial announce rate limiting (overrides config file)")
	queueThrottleThreshold := flag.Int("queue-throttle-threshold", 0, "Queue fill %% to throttle forwarders to fastest subset (overrides config file)")
	queueThrottleTopN := flag.Int("queue-throttle-top", 0, "Number of fastest forwarders to keep when throttling (overrides config file)")
	rateLimitInitialPerSec := flag.Int("rate-limit-initial-ps", 0, "Initial announces per second when rate limiting is active (overrides config file)")
	rateLimitInitialBurst := flag.Int("rate-limit-initial-burst", 0, "Burst for initial announce rate limit (overrides config file)")
	forwarderSuspend := flag.Int("forwarder-suspend", 0, "Seconds to suspend a forwarder after overload errors (e.g., HTTP 429) (overrides config file)")
	forwarderFailThreshold := flag.Int("F", 0, "Forwarder fail threshold before disabling (overrides config file)")
	forwarderRetryAttempts := flag.Int("R", 0, "Forwarder retry attempts (UDP/HTTP) (overrides config file)")
	forwarderRetryBaseMs := flag.Int("B", 0, "Forwarder retry base backoff in ms (exponential) (overrides config file)")
	enablePrometheus := flag.Bool("p", false, "Enable Prometheus metrics (overrides config file)")
	announceResponseInterval := flag.Int("i", 0, "Announce response interval (sec) (overrides config file)")
	minAnnounceInterval := flag.Int("m", 0, "Minimum announce interval (sec) (overrides config file)")
	statsInterval := flag.Int("s", 0, "Statistics print interval (sec) (overrides config file)")
	trackerID := flag.String("tracker-id", "", "Tracker ID to include in announce responses (overrides config file)")
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
		Listen:                   ":6969",
		UDPListen:                "",
		Debug:                    false,
		Age:                      180,
		XRealIP:                  false,
		ForwardTimeout:           30,
		ForwarderWorkers:         10,
		MaxForwarderWorkers:      20,
		ForwarderQueueSize:       10000,
		QueueScaleThresholdPct:   60,
		QueueRateLimitThreshold:  80,
		QueueThrottleThreshold:   60,
		QueueThrottleTopN:        20,
		RateLimitInitialPerSec:   100,
		RateLimitInitialBurst:    200,
		ForwarderSuspendSeconds:  300,
		ForwarderFailThreshold:   10,
		ForwarderRetryAttempts:   5,
		ForwarderRetryBaseMs:     500,
		AnnounceResponseInterval: 30,
		MinAnnounceInterval:      15,
		StatsInterval:            60,
		PrometheusEnabled:        false,
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
	if v := envFloat64("RETRACKER_AGE", 0); v > 0 {
		cfg.Age = v
	}
	if v := envBool("RETRACKER_X_REAL_IP", false); v {
		cfg.XRealIP = v
	}
	if v := envInt("RETRACKER_FORWARD_TIMEOUT"); v > 0 {
		cfg.ForwardTimeout = v
	}
	if v := envInt("FORWARDER_WORKERS"); v > 0 {
		cfg.ForwarderWorkers = v
	}
	if v := envInt("MAX_FORWARDER_WORKERS"); v > 0 {
		cfg.MaxForwarderWorkers = v
	}
	if v := envInt("FORWARDER_QUEUE_SIZE"); v > 0 {
		cfg.ForwarderQueueSize = v
	}
	if v := envInt("QUEUE_SCALE_THRESHOLD"); v > 0 {
		cfg.QueueScaleThresholdPct = v
	}
	if v := envInt("QUEUE_RATE_LIMIT_THRESHOLD"); v > 0 {
		cfg.QueueRateLimitThreshold = v
	}
	if v := envInt("QUEUE_THROTTLE_THRESHOLD"); v > 0 {
		cfg.QueueThrottleThreshold = v
	}
	if v := envInt("QUEUE_THROTTLE_TOP"); v > 0 {
		cfg.QueueThrottleTopN = v
	}
	if v := envInt("RATE_LIMIT_INITIAL_PER_SEC"); v > 0 {
		cfg.RateLimitInitialPerSec = v
	}
	if v := envInt("RATE_LIMIT_INITIAL_BURST"); v > 0 {
		cfg.RateLimitInitialBurst = v
	}
	if v := envInt("FORWARDER_SUSPEND_SECONDS"); v > 0 {
		cfg.ForwarderSuspendSeconds = v
	}
	if v := envInt("RETRACKER_ANNOUNCE_INTERVAL"); v > 0 {
		cfg.AnnounceResponseInterval = v
	}
	if v := envInt("RETRACKER_STATS_INTERVAL"); v > 0 {
		cfg.StatsInterval = v
	}
	if v := envBool("RETRACKER_PROMETHEUS", false); v {
		cfg.PrometheusEnabled = v
	}
	if v := envString("RETRACKER_TRACKER_ID", ""); v != "" {
		cfg.TrackerID = v
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
	if *age > 0 {
		cfg.Age = *age
	}
	xrealipSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "x" {
			xrealipSet = true
		}
	})
	if xrealipSet {
		cfg.XRealIP = *xrealip
	}
	if *forwardTimeout > 0 {
		cfg.ForwardTimeout = *forwardTimeout
	}
	if *forwarderWorkers > 0 {
		cfg.ForwarderWorkers = *forwarderWorkers
	}
	if *maxForwarderWorkers > 0 {
		cfg.MaxForwarderWorkers = *maxForwarderWorkers
	}
	if *forwarderQueueSize > 0 {
		cfg.ForwarderQueueSize = *forwarderQueueSize
	}
	if *queueScaleThreshold > 0 {
		cfg.QueueScaleThresholdPct = *queueScaleThreshold
	}
	if *queueRateLimitThreshold > 0 {
		cfg.QueueRateLimitThreshold = *queueRateLimitThreshold
	}
	if *queueThrottleThreshold > 0 {
		cfg.QueueThrottleThreshold = *queueThrottleThreshold
	}
	if *queueThrottleTopN > 0 {
		cfg.QueueThrottleTopN = *queueThrottleTopN
	}
	if *rateLimitInitialPerSec > 0 {
		cfg.RateLimitInitialPerSec = *rateLimitInitialPerSec
	}
	if *rateLimitInitialBurst > 0 {
		cfg.RateLimitInitialBurst = *rateLimitInitialBurst
	}
	if *forwarderSuspend > 0 {
		cfg.ForwarderSuspendSeconds = *forwarderSuspend
	}
	if *forwarderFailThreshold > 0 {
		cfg.ForwarderFailThreshold = *forwarderFailThreshold
	}
	if *forwarderRetryAttempts > 0 {
		cfg.ForwarderRetryAttempts = *forwarderRetryAttempts
	}
	if *forwarderRetryBaseMs > 0 {
		cfg.ForwarderRetryBaseMs = *forwarderRetryBaseMs
	}
	if *announceResponseInterval > 0 {
		cfg.AnnounceResponseInterval = *announceResponseInterval
	}
	if *minAnnounceInterval > 0 {
		cfg.MinAnnounceInterval = *minAnnounceInterval
	}
	if *statsInterval > 0 {
		cfg.StatsInterval = *statsInterval
	}
	prometheusSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "p" {
			prometheusSet = true
		}
	})
	if prometheusSet {
		cfg.PrometheusEnabled = *enablePrometheus
	}
	if *trackerID != "" {
		cfg.TrackerID = *trackerID
	}

	// Validate and adjust min announce interval
	if cfg.MinAnnounceInterval <= 0 {
		cfg.MinAnnounceInterval = cfg.AnnounceResponseInterval
	}
	if cfg.MinAnnounceInterval > cfg.AnnounceResponseInterval {
		cfg.MinAnnounceInterval = cfg.AnnounceResponseInterval
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
