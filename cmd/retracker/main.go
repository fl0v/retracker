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
	enablePrometheus := flag.Bool("p", false, "Enable Prometheus metrics")
	announceResponseInterval := flag.Int("i", 30, "Announce response interval (sec)")
	statsInterval := flag.Int("s", 60, "Statistics print interval (sec)")
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

	cfg := config.Config{
		AnnounceResponseInterval: *announceResponseInterval,
		Listen:                   *listen,
		UDPListen:                *udpListen,
		Debug:                    *debug,
		Age:                      *age,
		XRealIP:                  *xrealip,
		ForwardTimeout:           *forwardTimeout,
		ForwarderWorkers:         *forwarderWorkers,
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

	// Start UDP server if configured
	if cfg.UDPListen != "" {
		core.Receiver.UDP.TempStorage = tempStorage
		if err := core.Receiver.UDP.Start(cfg.UDPListen); err != nil {
			ErrorLog.Fatalln("Failed to start UDP server:", err.Error())
		}
		defer core.Receiver.UDP.Close()
	}

	if err := http.ListenAndServe(cfg.Listen, nil); err != nil { // set listen port
		ErrorLog.Println(err)
	}
}
