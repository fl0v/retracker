package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fl0v/retracker/internal/config"
)

func TestRuntimeForwarderInitializationDuringRequests(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 1800, ForwarderQueueSize: 10, ForwarderWorkers: 1, StatsInterval: 1}
	core := NewCore(cfg, nil)
	t.Cleanup(func() {
		if core.ForwarderManager != nil {
			core.ForwarderManager.Stop()
		}
	})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				var failure string
				if worker%2 == 0 {
					_, failure = core.Receiver.Announce.ProcessAnnounce("192.0.2.1", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1", "", "50", "started", "", "", "")
				} else {
					_, failure = core.Receiver.UDP.ProcessAnnounce("2001:4860::1", strings.Repeat("h", 20), strings.Repeat("q", 20), "6881", "0", "0", "1", "", "50", "started", "", "", "")
				}
				if failure != "" {
					t.Errorf("announce failed: %s", failure)
				}
				core.HTTPStatsHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stats?format=json", nil))
				if _, err := core.getScrapeResponse([]string{strings.Repeat("h", 20)}); err != nil {
					t.Errorf("scrape failed: %v", err)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		// Invalid URLs initialize the manager without introducing network traffic.
		req := httptest.NewRequest(http.MethodPost, "/add-forwarder", strings.NewReader(`{"urls":["ftp://invalid.example"]}`))
		core.HTTPAddForwarderHandler(httptest.NewRecorder(), req)
	}()
	close(start)
	wg.Wait()
	if core.ForwarderManager == nil {
		t.Fatal("runtime forwarder manager was not initialized")
	}
}

func TestHTTPAddForwarderHandler(t *testing.T) {
	cfg := &config.Config{
		ForwarderQueueSize:     10,
		ForwarderWorkers:       1,
		ForwarderFailThreshold: 2,
		ForwarderRetryAttempts: 1,
		ForwarderRetryBaseMs:   10,
		ForwardTimeout:         1,
	}

	tempStorage, err := NewTempStorage("")
	if err != nil {
		t.Fatalf("failed to create temp storage: %v", err)
	}

	core := NewCore(cfg, tempStorage)
	if core.ForwarderManager != nil {
		t.Fatalf("expected no forwarder manager initially")
	}
	t.Cleanup(func() {
		if core.ForwarderManager != nil {
			core.ForwarderManager.Stop()
		}
	})

	body := []byte(`{"urls":["https://dynamic-tracker.example.com:443/announce"]}`)
	req := httptest.NewRequest(http.MethodPost, "/add-forwarder", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if core.ForwarderManager == nil {
		t.Fatalf("expected forwarder manager to be initialized")
	}

	var resp addForwardersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "added" || resp.Added != 1 || resp.Failed != 0 {
		t.Fatalf("unexpected single-add response: %+v", resp)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/add-forwarder", bytes.NewReader(body))
	dupRec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(dupRec, dupReq)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("expected status 409 for duplicate, got %d", dupRec.Code)
	}

	badReq := httptest.NewRequest(http.MethodPost, "/add-forwarder", bytes.NewReader([]byte(`{"urls":["ftp://bad.example.com"]}`)))
	badRec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for bad protocol, got %d", badRec.Code)
	}

	multiBody := []byte(`{"urls":["https://ok1.example.com/announce","ftp://bad.example.com","https://ok2.example.com/announce"]}`)
	multiReq := httptest.NewRequest(http.MethodPost, "/add-forwarder", bytes.NewReader(multiBody))
	multiRec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(multiRec, multiReq)
	if multiRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for partial multi-add, got %d: %s", multiRec.Code, multiRec.Body.String())
	}
	var multiResp addForwardersResponse
	if err := json.Unmarshal(multiRec.Body.Bytes(), &multiResp); err != nil {
		t.Fatalf("failed to decode multi response: %v", err)
	}
	if multiResp.Status != "partial" || multiResp.Added != 2 || multiResp.Failed != 1 {
		t.Fatalf("unexpected multi-add response: %+v", multiResp)
	}

	dupOnlyBody := []byte(`{"urls":["https://ok1.example.com/announce","https://ok2.example.com/announce"]}`)
	dupOnlyReq := httptest.NewRequest(http.MethodPost, "/add-forwarder", bytes.NewReader(dupOnlyBody))
	dupOnlyRec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(dupOnlyRec, dupOnlyReq)
	if dupOnlyRec.Code != http.StatusConflict {
		t.Fatalf("expected status 409 for all-duplicate request, got %d", dupOnlyRec.Code)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/add-forwarder?urls=https://get1.example.com/announce,https://get2.example.com/announce,", nil)
	getRec := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for GET urls query, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var getResp addForwardersResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if getResp.Status != "added" || getResp.Added != 2 || getResp.Failed != 0 {
		t.Fatalf("unexpected GET add response: %+v", getResp)
	}
}
