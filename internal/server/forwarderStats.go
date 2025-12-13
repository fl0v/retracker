// forwarderStats.go - Statistics collection and reporting for forwarders.
// Implements StatsDataProvider interface for observability.
// Records response times (EMA), tracks per-hash peer stats, client stats.
// Key function: GetHashPeerStats() returns seeders/leechers per hash.
package server

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	CoreCommon "github.com/fl0v/retracker/common"
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

const (
	emaAlphaResponseTime = 0.15 // EMA weight for response time - 15% weight to latest sample
)

func (fm *ForwarderManager) recordStats(forwarderName string, responseTime time.Duration, interval int) {
	fm.statsMu.Lock()
	defer fm.statsMu.Unlock()

	stats, ok := fm.stats[forwarderName]
	if !ok {
		stats = &ForwarderStats{SampleCount: 0}
		fm.stats[forwarderName] = stats
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if stats.SampleCount == 0 {
		stats.AvgResponseTime = responseTime
		stats.LastInterval = interval
		stats.LastAnnounceTime = time.Now()
	} else {
		stats.AvgResponseTime = time.Duration(
			float64(responseTime)*emaAlphaResponseTime + float64(stats.AvgResponseTime)*(1-emaAlphaResponseTime),
		)
		stats.LastInterval = interval
		stats.LastAnnounceTime = time.Now()
	}
	stats.SampleCount++
}

func (fm *ForwarderManager) statsRoutine() {
	ticker := time.NewTicker(time.Duration(fm.Config.StatsInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-fm.stopChan:
			return
		case <-ticker.C:
			fm.printStats()
		}
	}
}

func (fm *ForwarderManager) printStats() {
	now := time.Now()
	collector := observability.NewStatsCollector()
	provider := &forwarderStatsProvider{fm: fm, cfg: fm.Config, now: now}

	fullStats := collector.CollectStats(provider)
	if fm.Prometheus != nil {
		fm.updatePrometheusQueue(fullStats)
	}

	simpleStats := collector.CollectSimpleStats(provider)
	text := collector.FormatSimpleText(simpleStats)
	fmt.Print(text)
}

// forwarderStatsProvider implements observability.StatsDataProvider
type forwarderStatsProvider struct {
	fm  *ForwarderManager
	cfg *config.Config
	now time.Time
}

func (p *forwarderStatsProvider) GetPendingCount() int {
	p.fm.pendingMu.Lock()
	defer p.fm.pendingMu.Unlock()
	return len(p.fm.pendingJobs)
}

func (p *forwarderStatsProvider) GetTrackedHashes() int {
	trackedHashSet := make(map[string]struct{})

	p.fm.Storage.mu.RLock()
	for infoHash := range p.fm.Storage.Entries {
		trackedHashSet[fmt.Sprintf("%x", infoHash)] = struct{}{}
	}
	p.fm.Storage.mu.RUnlock()

	if p.fm.MainStorage != nil {
		p.fm.MainStorage.requestsMu.Lock()
		for infoHash := range p.fm.MainStorage.Requests {
			trackedHashSet[fmt.Sprintf("%x", infoHash)] = struct{}{}
		}
		p.fm.MainStorage.requestsMu.Unlock()
	}

	return len(trackedHashSet)
}

func (p *forwarderStatsProvider) GetDisabledForwarders() int {
	p.fm.pendingMu.Lock()
	defer p.fm.pendingMu.Unlock()
	return len(p.fm.disabledForwarders)
}

func (p *forwarderStatsProvider) GetActiveForwarders() int {
	p.fm.forwardersMu.RLock()
	defer p.fm.forwardersMu.RUnlock()
	return len(p.fm.Forwarders)
}

func (p *forwarderStatsProvider) GetForwarders() []observability.ForwarderStat {
	p.fm.forwardersMu.RLock()
	forwarders := make([]CoreCommon.Forward, len(p.fm.Forwarders))
	copy(forwarders, p.fm.Forwarders)
	p.fm.forwardersMu.RUnlock()

	p.fm.statsMu.RLock()
	forwarderStats := make(map[string]struct {
		AvgResponseTime  time.Duration
		LastInterval     int
		LastAnnounceTime time.Time
		Count            int
	})

	for forwarderName, stats := range p.fm.stats {
		stats.mu.RLock()
		if stats.SampleCount > 0 {
			forwarderStats[forwarderName] = struct {
				AvgResponseTime  time.Duration
				LastInterval     int
				LastAnnounceTime time.Time
				Count            int
			}{
				AvgResponseTime:  stats.AvgResponseTime,
				LastInterval:     stats.LastInterval,
				LastAnnounceTime: stats.LastAnnounceTime,
				Count:            stats.SampleCount,
			}
		}
		stats.mu.RUnlock()
	}
	p.fm.statsMu.RUnlock()

	result := make([]observability.ForwarderStat, len(forwarders))
	for i, forwarder := range forwarders {
		forwarderName := forwarder.GetName()
		protocol := forwarder.GetProtocol()
		if stats, ok := forwarderStats[forwarderName]; ok {
			secondsSinceLastAnnounce := int(p.now.Sub(stats.LastAnnounceTime).Seconds())
			result[i] = observability.ForwarderStat{
				Name:                     forwarderName,
				Protocol:                 protocol,
				AvgResponseTime:          stats.AvgResponseTime,
				LastInterval:             stats.LastInterval,
				SampleCount:              stats.Count,
				SecondsSinceLastAnnounce: secondsSinceLastAnnounce,
				HasStats:                 true,
			}
		} else {
			result[i] = observability.ForwarderStat{
				Name:     forwarderName,
				Protocol: protocol,
				HasStats: false,
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].HasStats && !result[j].HasStats {
			return true
		}
		if !result[i].HasStats && result[j].HasStats {
			return false
		}
		if !result[i].HasStats && !result[j].HasStats {
			return result[i].Name < result[j].Name
		}
		return result[i].AvgResponseTime < result[j].AvgResponseTime
	})

	return result
}

func (p *forwarderStatsProvider) GetHashPeerStats() map[string]observability.HashPeerStat {
	allHashes := make(map[common.InfoHash]struct{})

	p.fm.Storage.mu.RLock()
	for infoHash := range p.fm.Storage.Entries {
		allHashes[infoHash] = struct{}{}
	}
	p.fm.Storage.mu.RUnlock()

	if p.fm.MainStorage != nil {
		p.fm.MainStorage.requestsMu.Lock()
		for infoHash := range p.fm.MainStorage.Requests {
			allHashes[infoHash] = struct{}{}
		}
		p.fm.MainStorage.requestsMu.Unlock()
	}

	hashPeerStats := make(map[string]observability.HashPeerStat)

	for infoHash := range allHashes {
		localUnique := 0
		forwarderUnique := 0
		seenIPs := make(map[string]struct{})
		complete := 0
		incomplete := 0

		// Count local peers (unique by IP)
		if p.fm.MainStorage != nil {
			p.fm.MainStorage.requestsMu.Lock()
			if requests, ok := p.fm.MainStorage.Requests[infoHash]; ok {
				localUnique = len(requests)
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
			}
			p.fm.MainStorage.requestsMu.Unlock()
		}

		// Count forwarder peers (unique by IP, counted as seeders since we lack state)
		p.fm.Storage.mu.RLock()
		if forwarders, ok := p.fm.Storage.Entries[infoHash]; ok {
			for _, entry := range forwarders {
				forwarderUnique += len(entry.Peers)
				for _, peer := range entry.Peers {
					ipStr := string(peer.IP)
					if ipStr != "" {
						if _, exists := seenIPs[ipStr]; !exists {
							seenIPs[ipStr] = struct{}{}
							complete++ // Forwarder peers counted as seeders
						}
					}
				}
			}
		}
		p.fm.Storage.mu.RUnlock()

		hashKey := fmt.Sprintf("%x", infoHash)
		hashPeerStats[hashKey] = observability.HashPeerStat{
			LocalUnique:     localUnique,
			ForwarderUnique: forwarderUnique,
			Complete:        complete,
			Incomplete:      incomplete,
		}
	}

	return hashPeerStats
}

func (p *forwarderStatsProvider) GetClientStats() *observability.ClientStats {
	if p.fm.MainStorage == nil {
		return nil
	}

	type clientInfo struct {
		hashCount       int
		lastRequest     time.Time
		announcedHashes map[string]bool
	}

	clients := make(map[string]*clientInfo)

	p.fm.MainStorage.requestsMu.Lock()
	for hash, requests := range p.fm.MainStorage.Requests {
		hashStr := fmt.Sprintf("%x", hash)
		for _, request := range requests {
			peer := request.Peer()
			clientIP := string(peer.IP)
			if clientIP == "" {
				clientIP = unknownClient
			}

			clientName := request.PeerID.DecodeClient()
			if clientName == unknownClient {
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

			requestTime := request.Timestamp()
			if requestTime.After(client.lastRequest) {
				client.lastRequest = requestTime
			}
		}
	}
	p.fm.MainStorage.requestsMu.Unlock()

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

	sort.Slice(clientStats.Clients, func(i, j int) bool {
		return clientStats.Clients[i].Key < clientStats.Clients[j].Key
	})

	return clientStats
}

func (p *forwarderStatsProvider) GetQueueMetrics() (depth, capacity, fillPct int) {
	depth = len(p.fm.jobQueue)
	capacity = cap(p.fm.jobQueue)
	fillPct = p.fm.queueFillPct()
	return
}

func (p *forwarderStatsProvider) GetWorkerMetrics() (active, max int) {
	return p.fm.workerCount, p.fm.maxWorkers
}

func (p *forwarderStatsProvider) GetDropCounters() (droppedFull, rateLimited, throttled uint64) {
	return atomic.LoadUint64(&p.fm.droppedFullCount), atomic.LoadUint64(&p.fm.rateLimitedCount), 0
}

func (p *forwarderStatsProvider) GetConfig() *config.Config {
	return p.cfg
}
