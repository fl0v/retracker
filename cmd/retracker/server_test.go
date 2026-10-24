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
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	btresponse "github.com/fl0v/retracker/bittorrent/response"
	corecommon "github.com/fl0v/retracker/common"
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

func TestRootAnnounceOnlyOnPort6969(t *testing.T) {
	cfg := defaultConfig()
	core := server.NewCore(&cfg, nil)
	defer core.Close()
	mux := newMux(core)
	query := url.Values{
		"info_hash":  {string([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 255})},
		"peer_id":    {strings.Repeat("p", 20)},
		"port":       {"6881"},
		"uploaded":   {"0"},
		"downloaded": {"0"},
		"left":       {"100"},
		"compact":    {"1"},
		"event":      {"started"},
	}.Encode()
	var announceBody string
	for _, target := range []string{
		"http://retracker.local:6969/announce",
		"http://retracker.local:6969/",
		"http://127.0.0.1:6969/",
		"http://[::1]:6969/",
		"http://retracker.local/announce",
		"http://retracker.local:7000/announce",
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target+"?"+query, nil))
		if w.Code != http.StatusOK || w.Header().Get("Location") != "" {
			t.Fatalf("%s returned status=%d, redirect=%q", target, w.Code, w.Header().Get("Location"))
		}
		response, err := btresponse.Load(w.Body.Bytes())
		if err != nil || response.FailureReason != "" || response.Interval != cfg.AnnounceInterval {
			t.Fatalf("%s returned invalid announce: %s (error=%v)", target, w.Body.String(), err)
		}
		if announceBody == "" {
			announceBody = w.Body.String()
		} else if w.Body.String() != announceBody {
			t.Fatalf("%s does not match /announce response", target)
		}
	}
	for _, target := range []string{
		"http://retracker.local:7000/",
		"http://retracker.local:6969/unknown",
		"http://retracker.local:6969/announce/",
		"http://retracker.local:6969/metrics",
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target+"?"+query, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", target, w.Code)
		}
	}
	for _, target := range []string{"http://retracker.local/", "http://retracker.local:80/", "http://[::1]/"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target+"?"+query, nil))
		if w.Code != http.StatusOK || w.Body.String() != "retracker is live\n" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("%s did not return liveness confirmation: status=%d, body=%s", target, w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"http://retracker.local:6969/", "http://retracker.local:6969/announce"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		response, err := btresponse.Load(w.Body.Bytes())
		if w.Code != http.StatusBadRequest || err != nil || response.FailureReason == "" {
			t.Fatalf("%s did not reject a missing hash: status=%d, body=%s", target, w.Code, w.Body.String())
		}
	}
}

func TestMetricsHandlersIsolateTrackers(t *testing.T) {
	for i := range 2 {
		cfg := defaultConfig()
		if i == 0 {
			cfg.Forwards = []corecommon.Forward{{Uri: "udp://example.com:6969/announce"}}
		}
		core := server.NewCore(&cfg, nil)
		defer core.Close()
		handler, err := metricsHandler(core)
		if err != nil {
			t.Fatal(err)
		}
		if core.Receiver.Announce.Prometheus != core.Receiver.UDP.Prometheus {
			t.Fatal("HTTP and UDP received different metrics")
		}
		if core.ForwarderManager != nil && core.ForwarderManager.Prometheus != core.Receiver.Announce.Prometheus {
			t.Fatal("forwarder metrics not attached")
		}
		core.Receiver.Announce.Prometheus.Requests.Add(float64(i + 1))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		want := "requests 1\n"
		if i == 1 {
			want = "requests 2\n"
		}
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) || !strings.Contains(w.Body.String(), "go_goroutines ") || !strings.Contains(w.Body.String(), "process_start_time_seconds ") {
			t.Fatalf("incomplete or shared metrics: status=%d, body=%s", w.Code, w.Body.String())
		}
	}
}

func TestRunWithMetricsCanRestart(t *testing.T) {
	cfg := defaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.PrometheusEnabled = true
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := run(ctx, cfg)
		cancel()
		if err != nil {
			t.Fatalf("metrics-enabled startup failed: %v", err)
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
