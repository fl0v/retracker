package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fl0v/retracker/internal/server"
)

func TestServeDrainsActiveRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	httpServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			_, _ = io.WriteString(w, "completed")
		case <-r.Context().Done():
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serving := make(chan error, 1)
	go func() { serving <- serve(ctx, httpServer, listener, time.Second) }()
	response := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, requestErr := client.Get("http://" + listener.Addr().String())
		if requestErr != nil {
			response <- requestErr.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-serving:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP shutdown did not finish")
	}
	if got := <-response; got != "completed" {
		t.Fatalf("active request did not drain: %q", got)
	}
}

func TestServeForcesCloseAfterDrainTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	httpServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(handlerDone)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serving := make(chan error, 1)
	go func() { serving <- serve(ctx, httpServer, listener, 20*time.Millisecond) }()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: time.Second}
		resp, _ := client.Get("http://" + listener.Addr().String())
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not start")
	}
	cancel()
	select {
	case err := <-serving:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want drain deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain deadline was not enforced")
	}
	for _, done := range []chan struct{}{handlerDone, clientDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("forced close did not release the request")
		}
	}
}

func TestRunClosesHTTPAndUDPOnCancellation(t *testing.T) {
	cfg := defaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.UDPListen = "127.0.0.1:0"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.UDPListen = "invalid address"
	if err := run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "listen UDP") {
		t.Fatalf("expected UDP startup failure, got %v", err)
	}
}

func TestDedicatedMuxRoutes(t *testing.T) {
	// Multiple cores can bind their routes without touching the global HTTP mux.
	for range 2 {
		cfg := defaultConfig()
		core := server.NewCore(&cfg, nil)
		t.Cleanup(func() { _ = core.Close() })
		mux := newMux(core)
		for _, path := range []string{"/favicon.ico", "/stats?format=json"} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Fatalf("route %s returned %d with %d bytes", path, w.Code, w.Body.Len())
			}
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if w.Code != http.StatusNotFound {
			t.Fatal("metrics enabled without configuration")
		}
	}
}

func TestServeReportsListenerFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	httpServer := &http.Server{ReadHeaderTimeout: time.Second}
	if err := serve(context.Background(), httpServer, listener, time.Second); err == nil {
		t.Fatal("closed listener did not report an error")
	}
}

func TestRunHandlesCanceledContextAndListenFailure(t *testing.T) {
	cfg := defaultConfig()
	cfg.Listen = "invalid address"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, cfg); err != nil {
		t.Fatalf("canceled startup returned %v", err)
	}
	if err := run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "listen HTTP") {
		t.Fatalf("expected HTTP listen failure, got %v", err)
	}
}

func TestShutdownSignalHelper(_ *testing.T) {
	if os.Getenv("RETRACKER_SHUTDOWN_TEST") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("retracker", flag.ExitOnError)
	os.Args = []string{"retracker", "-l", "127.0.0.1:0", "-u", "127.0.0.1:0"}
	main()
	os.Exit(0)
}

func TestSIGTERMStopsHTTPAndUDP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM integration test requires Unix signals")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownSignalHelper$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "RETRACKER_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "RETRACKER_SHUTDOWN_TEST=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var processErr error
	go func() { processErr = cmd.Wait(); close(exited) }()
	defer func() { _ = cmd.Process.Kill(); <-exited }()
	ready := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "Server ready to accept connections") {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-exited:
		t.Fatalf("server exited before readiness: %v", processErr)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		if processErr != nil {
			t.Fatalf("SIGTERM did not exit cleanly: %v", processErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server ignored SIGTERM")
	}
	<-readerDone
}
