package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
	"github.com/fl0v/retracker/internal/server"
)

const shutdownTimeout = 5 * time.Second

func run(ctx context.Context, cfg config.Config, loggers ...*slog.Logger) error {
	if ctx.Err() != nil {
		return nil
	}
	logger := observability.NewLogging(cfg.Debug, loggers...).Log()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	defer listener.Close()
	tempStorage, err := server.NewTempStorage("", logger)
	if err != nil {
		return fmt.Errorf("create temp storage: %w", err)
	}
	defer tempStorage.Close()
	core := server.NewCore(&cfg, tempStorage, logger)
	defer core.Close()
	mux := newMux(core)
	if cfg.PrometheusEnabled {
		handler, metricsErr := metricsHandler(core)
		if metricsErr != nil {
			return metricsErr
		}
		mux.Handle("/metrics", handler)
	}
	if cfg.UDPListen != "" {
		if udpErr := core.Receiver.UDP.Start(cfg.UDPListen); udpErr != nil {
			return fmt.Errorf("listen UDP: %w", udpErr)
		}
	}
	core.Start()
	logStartupConfig(logger, &cfg)
	logger.Info("Server ready to accept connections", "http_listen", listener.Addr().String(), "udp_listen", cfg.UDPListen)
	httpServer := newHTTPServer(mux, logger)
	return serve(ctx, httpServer, listener, shutdownTimeout)
}

// logStartupConfig uses labeled, single-line records instead of a struct dump.
func logStartupConfig(logger *slog.Logger, cfg *config.Config) {
	logger.Info("Tracker configuration", "announce_interval_seconds", cfg.AnnounceInterval, "retry_period_seconds", cfg.RetryPeriod, "peer_age_minutes", cfg.Age, "tracker_id", cfg.TrackerID, "debug", cfg.Debug, "metrics", cfg.PrometheusEnabled, "x_real_ip", cfg.XRealIP, "stats_interval_seconds", cfg.StatsInterval)
	logger.Info("Forwarder configuration", "configured", len(cfg.Forwards), "file", cfg.ForwardsFile, "workers", cfg.ForwarderWorkers, "max_workers", cfg.MaxForwarderWorkers, "queue_capacity", cfg.ForwarderQueueSize, "max_per_announce", cfg.MaxForwardersPerAnnounce)
	logger.Info("Forwarder queue thresholds", "scale_percent", cfg.QueueScaleThresholdPct, "rate_limit_percent", cfg.QueueRateLimitThreshold, "throttle_percent", cfg.QueueThrottleThreshold, "throttle_top", cfg.QueueThrottleTopN, "initial_per_second", cfg.RateLimitInitialPerSec, "initial_burst", cfg.RateLimitInitialBurst)
	logger.Info("Forwarder retry configuration", "timeout_seconds", cfg.ForwardTimeout, "attempts", cfg.ForwarderRetryAttempts, "backoff_ms", cfg.ForwarderRetryBaseMs, "suspend_seconds", cfg.ForwarderSuspendSeconds, "failure_threshold", cfg.ForwarderFailThreshold)
}

func newHTTPServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

// metricsHandler owns a registry for this tracker, including runtime metrics.
func metricsHandler(core *server.Core) (http.Handler, error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics, err := observability.NewPrometheus(registry)
	if err != nil {
		return nil, fmt.Errorf("initialize metrics: %w", err)
	}
	core.Receiver.Announce.Prometheus = metrics
	core.Receiver.UDP.Prometheus = metrics
	if core.ForwarderManager != nil {
		core.ForwarderManager.Prometheus = metrics
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), nil
}

func newMux(core *server.Core) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(faviconIco)))
		_, _ = w.Write(faviconIco)
	})
	mux.HandleFunc("/scrape", core.HTTPScrapeHandler)
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		_, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			port = "80" // HTTP Host omits the default port.
		}
		switch port {
		case "6969":
			core.Receiver.Announce.HTTPHandler(w, r)
		case "80":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "retracker is live")
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/announce", core.Receiver.Announce.HTTPHandler)
	mux.HandleFunc("/stats", core.HTTPStatsHandler)
	mux.HandleFunc("/add-forwarder", core.HTTPAddForwarderHandler)
	return mux
}

// serve joins the HTTP listener and gives active requests a bounded drain period.
func serve(ctx context.Context, httpServer *http.Server, listener net.Listener, timeout time.Duration) error {
	defer httpServer.Close()
	result := make(chan error, 1)
	go func() { result <- httpServer.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := httpServer.Shutdown(shutdownCtx)
	if err != nil {
		err = errors.Join(err, httpServer.Close())
	}
	serveErr := <-result
	if !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}
