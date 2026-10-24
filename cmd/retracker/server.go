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
	logger.Info("Server ready to accept connections", "http_listen", listener.Addr().String(), "udp_listen", cfg.UDPListen, "config", &cfg)
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	return serve(ctx, httpServer, listener, shutdownTimeout)
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
