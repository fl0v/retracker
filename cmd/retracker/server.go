package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
	"github.com/fl0v/retracker/internal/server"
)

const shutdownTimeout = 5 * time.Second

func run(ctx context.Context, cfg config.Config) error {
	if ctx.Err() != nil {
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	defer listener.Close()
	tempStorage, err := server.NewTempStorage("")
	if err != nil {
		return fmt.Errorf("create temp storage: %w", err)
	}
	defer tempStorage.Close()
	core := server.NewCore(&cfg, tempStorage)
	defer core.Close()
	mux := newMux(core)
	if cfg.PrometheusEnabled {
		metrics, metricsErr := observability.NewPrometheus()
		if metricsErr != nil {
			return fmt.Errorf("initialize metrics: %w", metricsErr)
		}
		core.Receiver.Announce.Prometheus = metrics
		core.Receiver.UDP.Prometheus = metrics
		if core.ForwarderManager != nil {
			core.ForwarderManager.Prometheus = metrics
		}
		mux.Handle("/metrics", promhttp.Handler())
	}
	if cfg.UDPListen != "" {
		if udpErr := core.Receiver.UDP.Start(cfg.UDPListen); udpErr != nil {
			return fmt.Errorf("listen UDP: %w", udpErr)
		}
	}
	core.Start()
	cfg.PrintConfig()
	fmt.Printf("HTTP tracker listening on %s\nServer ready to accept connections\n", listener.Addr())
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return serve(ctx, httpServer, listener, shutdownTimeout)
}

func newMux(core *server.Core) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(faviconIco)))
		_, _ = w.Write(faviconIco)
	})
	mux.HandleFunc("/scrape", core.HTTPScrapeHandler)
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
