package server

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/tracker"
	corecommon "github.com/fl0v/retracker/common"
)

type forwarderRoundTripper func(*http.Request) (*http.Response, error)

func (fn forwarderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type trackedHTTPBody struct {
	reader io.Reader
	read   int
	closed int
}

func (body *trackedHTTPBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	body.read += n
	return n, err
}

func (body *trackedHTTPBody) Close() error { body.closed++; return nil }

type endlessHTTPBody struct{}

func (endlessHTTPBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type failedHTTPBody struct{}

func (failedHTTPBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func newHTTPForwarderTest(t *testing.T, client *http.Client) (*ForwarderManager, AnnounceJob) {
	t.Helper()
	cfg := makeIntegrationCfg()
	fm := NewForwarderManager(cfg, NewForwarderStorage(), nil, nil, nil, WithHTTPClient(client))
	t.Cleanup(fm.Stop)
	req, err := tracker.MakeRequest("192.0.2.1", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1", "", "50", "started", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := corecommon.Forward{Uri: "https://tracker.example/announce", Host: "virtual.example"}
	return fm, AnnounceJob{InfoHash: req.InfoHash, PeerID: req.PeerID, ForwarderName: forwarder.GetName(), Forwarder: forwarder, Request: *req}
}

func paddedAnnounceResponse(size int) string {
	padding := size - 64
	for {
		prefix := fmt.Sprintf("d8:intervali1800e7:padding%d:", padding)
		const suffix = "5:peers0:e"
		delta := size - len(prefix) - padding - len(suffix)
		if delta == 0 {
			return prefix + strings.Repeat("x", padding) + suffix
		}
		padding += delta
	}
}

func TestHTTPAnnounceBodyLimitsAndClosure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reader    io.Reader
		length    int64
		wantError error
		wantRead  int
		wantSize  int
	}{
		{"small", strings.NewReader("d8:intervali1800e5:peers0:e"), -1, nil, 27, 27},
		{"exact limit", strings.NewReader(paddedAnnounceResponse(maxForwarderHTTPResponseBytes)), maxForwarderHTTPResponseBytes, nil, maxForwarderHTTPResponseBytes, maxForwarderHTTPResponseBytes},
		{"declared oversized", endlessHTTPBody{}, maxForwarderHTTPResponseBytes + 1, ErrForwarderResponseTooLarge, 0, 0},
		{"streamed oversized", endlessHTTPBody{}, -1, ErrForwarderResponseTooLarge, maxForwarderHTTPResponseBytes + 1, 0},
		{"misleading length", endlessHTTPBody{}, 1, ErrForwarderResponseTooLarge, maxForwarderHTTPResponseBytes + 1, 0},
		{"read failure", failedHTTPBody{}, -1, io.ErrUnexpectedEOF, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedHTTPBody{reader: tc.reader}
			client := &http.Client{Transport: forwarderRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: tc.length, Request: req}, nil
			})}
			fm, job := newHTTPForwarderTest(t, client)
			result := fm.doHTTPAnnounce(job)
			if tc.wantError != nil {
				if result.Success || !errors.Is(result.Error, tc.wantError) || result.NonRetryable || result.Suspend {
					t.Fatalf("unexpected failure classification: %+v", result)
				}
			} else if !result.Success || result.Interval != 1800 || result.ResponseSize != tc.wantSize {
				t.Fatalf("valid response was not preserved: %+v", result)
			}
			if body.read != tc.wantRead || body.closed != 1 {
				t.Fatalf("read %d bytes and closed %d times, want %d and 1", body.read, body.closed, tc.wantRead)
			}
		})
	}
}

func TestIgnoredHTTPBodiesHaveBoundedReads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		event     bool
		permanent bool
		suspend   bool
	}{
		{"rate limited", http.StatusTooManyRequests, false, false, true},
		{"rejected", http.StatusForbidden, false, true, false},
		{"server error", http.StatusInternalServerError, false, false, false},
		{"event success", http.StatusOK, true, false, false},
		{"event failure", http.StatusInternalServerError, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedHTTPBody{reader: endlessHTTPBody{}}
			client := &http.Client{Transport: forwarderRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status), Body: body, ContentLength: -1, Header: make(http.Header), Request: req}, nil
			})}
			fm, job := newHTTPForwarderTest(t, client)
			if tc.event {
				err := fm.doHTTPEventRequest(job.Forwarder, job.Request)
				if (err == nil) != (tc.status == http.StatusOK) {
					t.Fatalf("unexpected event status result: %v", err)
				}
			} else {
				result := fm.doHTTPAnnounce(job)
				if result.Success || result.NonRetryable != tc.permanent || result.Suspend != tc.suspend {
					t.Fatalf("HTTP classification changed: %+v", result)
				}
			}
			if body.read != maxForwarderHTTPDrainBytes || body.closed != 1 {
				t.Fatalf("ignored body read %d bytes, closed %d times", body.read, body.closed)
			}
		})
	}
}

type idleTrackingTransport struct {
	forwarderRoundTripper
	closed atomic.Int32
}

func (transport *idleTrackingTransport) CloseIdleConnections() { transport.closed.Add(1) }

func TestInjectedClientIsSharedAndCallerOwned(t *testing.T) {
	var requests atomic.Int32
	transport := &idleTrackingTransport{forwarderRoundTripper: func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Host != "virtual.example" || len(req.URL.Query().Get("info_hash")) != 20 {
			t.Errorf("forwarded request metadata changed: %s, %q", req.URL, req.Host)
		}
		if _, ok := req.Context().Deadline(); !ok {
			t.Error("request is missing the configured deadline")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("d8:intervali1800e5:peers0:e")), Header: make(http.Header), Request: req}, nil
	}}
	client := &http.Client{Transport: transport}
	fm, job := newHTTPForwarderTest(t, client)
	if result := fm.doHTTPAnnounce(job); !result.Success {
		t.Fatal(result.Error)
	}
	job.Request.Event = EventCompleted
	if err := fm.doHTTPEventRequest(job.Forwarder, job.Request); err != nil {
		t.Fatal(err)
	}
	fm.Stop()
	if requests.Load() != 2 || transport.closed.Load() != 0 {
		t.Fatal("client was not shared or its transport was closed by the manager")
	}
	client.CloseIdleConnections()
	if transport.closed.Load() != 1 {
		t.Fatal("caller could not close its transport")
	}
}

func TestOwnedHTTPPoolReusesConnectionsAndClosesOnStop(t *testing.T) {
	var connections, requests atomic.Int32
	closed := make(chan struct{}, 4)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "busy")
			return
		}
		_, _ = io.WriteString(w, "d8:intervali1800e5:peers0:e")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	upstream.Start()
	defer upstream.Close()
	fm, job := newHTTPForwarderTest(t, nil)
	job.Forwarder.Uri = upstream.URL
	if result := fm.doHTTPAnnounce(job); result.Success {
		t.Fatal("HTTP 500 was accepted")
	}
	if result := fm.doHTTPAnnounce(job); !result.Success {
		t.Fatal(result.Error)
	}
	if err := fm.doHTTPEventRequest(job.Forwarder, job.Request); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 1 {
		t.Fatalf("expected connection reuse, got %d connections", connections.Load())
	}
	fm.Stop()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("owned idle connection remained open after Stop")
	}
}

func TestHTTPResponseLimitAppliesAfterGzipDecompression(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = io.Copy(writer, io.LimitReader(endlessHTTPBody{}, maxForwarderHTTPResponseBytes+1))
		_ = writer.Close()
	}))
	defer upstream.Close()
	fm, job := newHTTPForwarderTest(t, nil)
	job.Forwarder.Uri = upstream.URL
	result := fm.doHTTPAnnounce(job)
	if result.Success || !errors.Is(result.Error, ErrForwarderResponseTooLarge) {
		t.Fatalf("compressed oversized response was not rejected: %+v", result)
	}
}

func TestInjectedClientPreservesTLSAndTimeoutSettings(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "d8:intervali1800e5:peers0:e")
	}))
	defer upstream.Close()
	client := upstream.Client()
	defer client.CloseIdleConnections()
	fm, job := newHTTPForwarderTest(t, client)
	job.Forwarder.Uri = upstream.URL
	if result := fm.doHTTPAnnounce(job); !result.Success {
		t.Fatalf("injected TLS trust configuration was not used: %v", result.Error)
	}
	timedClient := &http.Client{Transport: client.Transport, Timeout: 50 * time.Millisecond}
	timedManager, timedJob := newHTTPForwarderTest(t, timedClient)
	timedJob.Forwarder.Uri = upstream.URL + "/slow"
	result := timedManager.doHTTPAnnounce(timedJob)
	var timeoutError net.Error
	if result.Success || !errors.As(result.Error, &timeoutError) || !timeoutError.Timeout() || result.NonRetryable || result.Suspend {
		t.Fatalf("injected timeout was not preserved: %+v", result)
	}
}

func TestSharedHTTPClientCancelsBodyReads(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			started := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "d8:interval")
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer upstream.Close()
			fm, job := newHTTPForwarderTest(t, nil)
			job.Forwarder.Uri = upstream.URL
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job.state = &announceJobState{peerContext: ctx}
			done := make(chan AnnounceResult, 1)
			go func() { done <- fm.doHTTPAnnounce(job) }()
			waitForShutdown(t, started)
			if shutdown {
				fm.Stop()
			} else {
				cancel()
			}
			select {
			case result := <-done:
				if result.Success || !errors.Is(result.Error, context.Canceled) {
					t.Fatalf("canceled body read returned %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("body read ignored cancellation")
			}
		})
	}
}
