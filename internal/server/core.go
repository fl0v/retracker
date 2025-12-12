package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

type Core struct {
	Config           *config.Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Receiver         *Receiver
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
		seenLocal := make(map[common.PeerID]struct{})
		seenIPs := make(map[string]struct{})
		complete := 0
		incomplete := 0

		// Count local peers (unique by peer ID) and calculate Complete/Incomplete
		for peerID, peerRequest := range requests {
			seenLocal[peerID] = struct{}{}

			// Track unique IPs for Complete/Incomplete calculation
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
			LocalUnique:     len(seenLocal),
			ForwarderUnique: 0,
			TotalUnique:     len(seenLocal),
			Complete:        complete,
			Incomplete:      incomplete,
			Downloaded:      complete, // Downloaded is same as Complete
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
