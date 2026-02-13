// core.go - Central application structure and orchestration.
// Contains Core struct that holds Storage, ForwarderStorage, ForwarderManager, and Receiver.
// Also provides HTTP stats endpoint handler and simpleStatsProvider for non-forwarder mode.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

type Core struct {
	Config           *config.Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Receiver         *Receiver
	forwarderMu      sync.Mutex
}

func NewCore(cfg *config.Config, tempStorage *TempStorage) *Core {
	storage := NewStorage(cfg)

	var forwarderStorage *ForwarderStorage
	var forwarderManager *ForwarderManager

	// Initialize forwarder system if forwards are configured
	if len(cfg.Forwards) > 0 {
		forwarderStorage = NewForwarderStorage()

		var prometheus *observability.Prometheus
		// Prometheus will be set later if enabled

		// Disable Storage's separate stats routine since ForwarderManager will handle it
		storage.disableStatsRoutine = true

		forwarderManager = NewForwarderManager(cfg, forwarderStorage, storage, prometheus, tempStorage)
		forwarderManager.Start()
	}

	core := Core{
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: forwarderManager,
		Receiver:         NewReceiver(cfg, storage, forwarderStorage, forwarderManager),
	}
	core.Receiver.Announce.TempStorage = tempStorage
	core.Receiver.UDP.TempStorage = tempStorage
	return &core
}

// HTTPStatsHandler handles /stats HTTP requests
func (core *Core) HTTPStatsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	collector := observability.NewStatsCollector()

	var provider observability.StatsDataProvider
	if core.ForwarderManager != nil {
		provider = &forwarderStatsProvider{fm: core.ForwarderManager, cfg: core.Config, now: now}
	} else {
		// When no forwarders are configured, create a simple provider using only Storage
		provider = &simpleStatsProvider{storage: core.Storage, cfg: core.Config, now: now}
	}

	stats := collector.CollectStats(provider)

	// Check for format parameter
	format := r.URL.Query().Get("format")
	if format == "json" {
		jsonData, err := collector.FormatJSON(stats)
		if err != nil {
			http.Error(w, "Failed to encode JSON: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	} else {
		text := collector.FormatText(stats)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(text))
	}
}

type addForwarderRequest struct {
	URLs []string `json:"urls"`
}

type addForwarderResult struct {
	URL    string `json:"url"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type addForwardersResponse struct {
	Status  string               `json:"status"`
	Added   int                  `json:"added"`
	Failed  int                  `json:"failed"`
	Results []addForwarderResult `json:"results"`
}

func (core *Core) ensureForwarderManager() *ForwarderManager {
	core.forwarderMu.Lock()
	defer core.forwarderMu.Unlock()

	if core.ForwarderManager != nil {
		return core.ForwarderManager
	}

	if core.ForwarderStorage == nil {
		core.ForwarderStorage = NewForwarderStorage()
	}

	// ForwarderManager provides combined stats updates when forwarding is active.
	core.Storage.disableStatsRoutine = true

	var prom *observability.Prometheus
	var tempStorage *TempStorage
	if core.Receiver != nil && core.Receiver.Announce != nil {
		prom = core.Receiver.Announce.Prometheus
		tempStorage = core.Receiver.Announce.TempStorage
	}

	core.ForwarderManager = NewForwarderManager(core.Config, core.ForwarderStorage, core.Storage, prom, tempStorage)
	core.ForwarderManager.Start()

	if core.Receiver != nil {
		if core.Receiver.Announce != nil {
			core.Receiver.Announce.ForwarderStorage = core.ForwarderStorage
			core.Receiver.Announce.ForwarderManager = core.ForwarderManager
		}
		if core.Receiver.UDP != nil {
			core.Receiver.UDP.ForwarderStorage = core.ForwarderStorage
			core.Receiver.UDP.ForwarderManager = core.ForwarderManager
		}
	}

	return core.ForwarderManager
}

// HTTPAddForwarderHandler handles POST /add-forwarder and adds runtime forwarders in memory only.
func (core *Core) HTTPAddForwarderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	forwarderURLs := make([]string, 0, 8)
	if queryURLs := strings.TrimSpace(r.URL.Query().Get("urls")); queryURLs != "" {
		parts := strings.Split(queryURLs, ",")
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				forwarderURLs = append(forwarderURLs, trimmed)
			}
		}
	}

	if r.Method == http.MethodPost {
		var req addForwarderRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, u := range req.URLs {
			if trimmed := strings.TrimSpace(u); trimmed != "" {
				forwarderURLs = append(forwarderURLs, trimmed)
			}
		}
	}

	if len(forwarderURLs) == 0 {
		http.Error(w, "missing forwarder urls (use urls array or ?urls=url1,url2)", http.StatusBadRequest)
		return
	}

	fm := core.ensureForwarderManager()

	resp := addForwardersResponse{
		Status:  "added",
		Results: make([]addForwarderResult, 0, len(forwarderURLs)),
	}

	allDuplicates := true
	for _, forwarderURL := range forwarderURLs {
		forwarder, err := fm.AddRuntimeForwarder(forwarderURL)
		if err != nil {
			resp.Failed++
			resp.Results = append(resp.Results, addForwarderResult{
				URL:    forwarderURL,
				Status: "error",
				Error:  err.Error(),
			})
			if !errors.Is(err, ErrForwarderAlreadyExists) {
				allDuplicates = false
			}
			continue
		}

		resp.Added++
		resp.Results = append(resp.Results, addForwarderResult{
			URL:    forwarder.Uri,
			Name:   forwarder.GetName(),
			Status: "added",
		})
	}

	statusCode := http.StatusOK
	switch {
	case resp.Added > 0 && resp.Failed > 0:
		resp.Status = "partial"
	case resp.Added == 0 && resp.Failed > 0:
		resp.Status = "failed"
		if allDuplicates {
			statusCode = http.StatusConflict
		} else {
			statusCode = http.StatusBadRequest
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

// simpleStatsProvider provides stats when no forwarders are configured
type simpleStatsProvider struct {
	storage *Storage
	cfg     *config.Config
	now     time.Time
}

func (p *simpleStatsProvider) GetPendingCount() int {
	return 0
}

func (p *simpleStatsProvider) GetTrackedHashes() int {
	p.storage.requestsMu.Lock()
	defer p.storage.requestsMu.Unlock()
	return len(p.storage.Requests)
}

func (p *simpleStatsProvider) GetDisabledForwarders() int {
	return 0
}

func (p *simpleStatsProvider) GetDisabledForwarderDetails() []observability.DisabledForwarderInfo {
	return nil
}

func (p *simpleStatsProvider) GetSuspendedForwarderDetails() []observability.SuspendedForwarderInfo {
	return nil
}

func (p *simpleStatsProvider) GetActiveForwarders() int {
	return 0
}

func (p *simpleStatsProvider) GetForwarders() []observability.ForwarderStat {
	return nil
}

func (p *simpleStatsProvider) GetHashPeerStats() map[string]observability.HashPeerStat {
	hashPeerStats := make(map[string]observability.HashPeerStat)

	p.storage.requestsMu.Lock()
	defer p.storage.requestsMu.Unlock()

	for infoHash, requests := range p.storage.Requests {
		seenIPs := make(map[string]struct{})
		complete := 0
		incomplete := 0

		// Count local peers (unique by IP)
		for _, peerRequest := range requests {
			ipStr := string(peerRequest.Peer().IP)
			if ipStr != "" {
				if _, exists := seenIPs[ipStr]; !exists {
					seenIPs[ipStr] = struct{}{}
					if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
						complete++
					} else {
						incomplete++
					}
				}
			}
		}

		hashKey := fmt.Sprintf("%x", infoHash)
		hashPeerStats[hashKey] = observability.HashPeerStat{
			LocalUnique:     len(requests),
			ForwarderUnique: 0,
			Complete:        complete,
			Incomplete:      incomplete,
		}
	}

	return hashPeerStats
}

func (p *simpleStatsProvider) GetClientStats() *observability.ClientStats {
	// Map to track clients: "IP:ClientName" -> {hashCount, lastRequestTime}
	type clientInfo struct {
		hashCount       int
		lastRequest     time.Time
		announcedHashes map[string]bool // Track unique hashes
	}

	clients := make(map[string]*clientInfo)

	p.storage.requestsMu.Lock()
	for hash, requests := range p.storage.Requests {
		hashStr := fmt.Sprintf("%x", hash)
		for _, request := range requests {
			// Use IP from request (Peer() already handles fallback to remoteAddr)
			peer := request.Peer()
			clientIP := string(peer.IP)
			if clientIP == "" {
				clientIP = unknownClient
			}

			// Decode client from peer_id (more reliable than User-Agent)
			clientName := request.PeerID.DecodeClient()
			if clientName == unknownClient {
				// Fallback to User-Agent if peer_id decoding fails
				userAgent := request.UserAgent
				if userAgent != "" {
					clientName = userAgent
				} else {
					clientName = unknownClient
				}
			}

			clientKey := fmt.Sprintf("%s:%s", clientIP, clientName)
			if _, ok := clients[clientKey]; !ok {
				clients[clientKey] = &clientInfo{
					hashCount:       0,
					lastRequest:     request.Timestamp(),
					announcedHashes: make(map[string]bool),
				}
			}

			client := clients[clientKey]
			client.announcedHashes[hashStr] = true
			client.hashCount = len(client.announcedHashes)

			// Update last request time if this request is more recent
			requestTime := request.Timestamp()
			if requestTime.After(client.lastRequest) {
				client.lastRequest = requestTime
			}
		}
	}
	p.storage.requestsMu.Unlock()

	clientStats := &observability.ClientStats{
		ActiveClients: len(clients),
		Clients:       make([]observability.ClientInfo, 0, len(clients)),
	}

	for clientKey, info := range clients {
		secondsSinceLastRequest := int(p.now.Sub(info.lastRequest).Seconds())
		clientStats.Clients = append(clientStats.Clients, observability.ClientInfo{
			Key:                 clientKey,
			AnnouncedHashes:     info.hashCount,
			SecondsSinceLastReq: secondsSinceLastRequest,
		})
	}

	// Sort clients by IP (Key format is "IP:ClientName")
	sort.Slice(clientStats.Clients, func(i, j int) bool {
		return clientStats.Clients[i].Key < clientStats.Clients[j].Key
	})

	return clientStats
}

func (p *simpleStatsProvider) GetQueueMetrics() (depth, capacity, fillPct int) {
	return 0, 0, 0
}

func (p *simpleStatsProvider) GetWorkerMetrics() (active, max int) {
	return 0, 0
}

func (p *simpleStatsProvider) GetDropCounters() (droppedFull, rateLimited, throttled uint64) {
	return 0, 0, 0
}

func (p *simpleStatsProvider) GetConfig() *config.Config {
	return p.cfg
}
