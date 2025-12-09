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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
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
	listen := flag.String("l", ":80", "Listen address:port for HTTP")
	udpListen := flag.String("u", "", "Listen address:port for UDP (empty to disable)")
	age := flag.Float64("a", 180, "Keep 'n' minutes peer in memory")
	debug := flag.Bool("d", false, "Debug mode")
	xrealip := flag.Bool("x", false, "Get RemoteAddr from X-Real-IP header")
	forwards := flag.String("f", "", "Load forwards from YAML file")
	forwardTimeout := flag.Int("t", 30, "Timeout (sec) for forward requests (used with -f)")
	forwarderWorkers := flag.Int("w", 10, "Number of workers for parallel forwarder processing")
	maxForwarderWorkers := flag.Int("W", 20, "Maximum workers for parallel forwarder processing")
	forwarderQueueSize := flag.Int("Q", 10000, "Forwarder announce job queue size")
	queueScaleThreshold := flag.Int("queue-scale-threshold", 60, "Queue fill %% to trigger worker scaling")
	queueRateLimitThreshold := flag.Int("queue-rate-limit-threshold", 80, "Queue fill %% to trigger initial announce rate limiting")
	queueThrottleThreshold := flag.Int("queue-throttle-threshold", 60, "Queue fill %% to throttle forwarders to fastest subset")
	queueThrottleTopN := flag.Int("queue-throttle-top", 20, "Number of fastest forwarders to keep when throttling")
	rateLimitInitialPerSec := flag.Int("rate-limit-initial-ps", 100, "Initial announces per second when rate limiting is active")
	rateLimitInitialBurst := flag.Int("rate-limit-initial-burst", 200, "Burst for initial announce rate limit")
	forwarderSuspend := flag.Int("forwarder-suspend", 300, "Seconds to suspend a forwarder after overload errors (e.g., HTTP 429)")
	forwarderFailThreshold := flag.Int("F", 10, "Forwarder fail threshold before disabling")
	forwarderRetryAttempts := flag.Int("R", 5, "Forwarder retry attempts (UDP/HTTP)")
	forwarderRetryBaseMs := flag.Int("B", 500, "Forwarder retry base backoff in ms (exponential)")
	enablePrometheus := flag.Bool("p", false, "Enable Prometheus metrics")
	announceResponseInterval := flag.Int("i", 30, "Announce response interval (sec)")
	minAnnounceInterval := flag.Int("m", 15, "Minimum announce interval (sec)")
	statsInterval := flag.Int("s", 60, "Statistics print interval (sec)")
	trackerID := flag.String("tracker-id", "", "Tracker ID to include in announce responses")
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

	if *minAnnounceInterval <= 0 {
		*minAnnounceInterval = *announceResponseInterval
	}
	if *minAnnounceInterval > *announceResponseInterval {
		*minAnnounceInterval = *announceResponseInterval
	}

	cfg := config.Config{
		AnnounceResponseInterval: *announceResponseInterval,
		MinAnnounceInterval:      *minAnnounceInterval,
		TrackerID:                *trackerID,
		Listen:                   *listen,
		UDPListen:                *udpListen,
		Debug:                    *debug,
		Age:                      *age,
		XRealIP:                  *xrealip,
		ForwardTimeout:           *forwardTimeout,
		ForwarderWorkers:         envInt("FORWARDER_WORKERS", *forwarderWorkers),
		MaxForwarderWorkers:      envInt("MAX_FORWARDER_WORKERS", *maxForwarderWorkers),
		ForwarderQueueSize:       envInt("FORWARDER_QUEUE_SIZE", *forwarderQueueSize),
		QueueScaleThresholdPct:   envInt("QUEUE_SCALE_THRESHOLD", *queueScaleThreshold),
		QueueRateLimitThreshold:  envInt("QUEUE_RATE_LIMIT_THRESHOLD", *queueRateLimitThreshold),
		QueueThrottleThreshold:   envInt("QUEUE_THROTTLE_THRESHOLD", *queueThrottleThreshold),
		QueueThrottleTopN:        envInt("QUEUE_THROTTLE_TOP", *queueThrottleTopN),
		RateLimitInitialPerSec:   envInt("RATE_LIMIT_INITIAL_PER_SEC", *rateLimitInitialPerSec),
		RateLimitInitialBurst:    envInt("RATE_LIMIT_INITIAL_BURST", *rateLimitInitialBurst),
		ForwarderSuspendSeconds:  envInt("FORWARDER_SUSPEND_SECONDS", *forwarderSuspend),
		ForwarderFailThreshold:   *forwarderFailThreshold,
		ForwarderRetryAttempts:   *forwarderRetryAttempts,
		ForwarderRetryBaseMs:     *forwarderRetryBaseMs,
		StatsInterval:            *statsInterval,
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
	if *enablePrometheus {
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
	cfg.PrintConfigWithPrometheus(*enablePrometheus)

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
