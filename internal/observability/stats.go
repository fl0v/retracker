package observability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fl0v/retracker/internal/config"
)

// StatsCollector collects and formats statistics
type StatsCollector struct{}

// Stats holds all collected statistics
type Stats struct {
	Config              *config.Config          `json:"config,omitempty"`
	PendingJobs         int                     `json:"pending_jobs"`
	TrackedHashes       int                     `json:"tracked_hashes"`
	DisabledForwarders  int                     `json:"disabled_forwarders"`
	ActiveForwarders    int                     `json:"active_forwarders"`
	Forwarders          []ForwarderStat         `json:"forwarders,omitempty"`
	HashPeerStats       map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
	ClientStats         *ClientStats            `json:"client_stats,omitempty"`
	QueueDepth          int                     `json:"queue_depth"`
	QueueCapacity       int                     `json:"queue_capacity"`
	QueueFillPct        int                     `json:"queue_fill_pct"`
	ActiveWorkers       int                     `json:"active_workers"`
	MaxWorkers          int                     `json:"max_workers"`
	DroppedFull         uint64                  `json:"dropped_full"`
	RateLimited         uint64                  `json:"rate_limited"`
	ThrottledForwarders uint64                  `json:"throttled_forwarders"`
}

// ForwarderStat represents statistics for a forwarder
type ForwarderStat struct {
	Name                     string        `json:"name"`
	Protocol                 string        `json:"protocol,omitempty"`
	AvgResponseTime          time.Duration `json:"avg_response_time_ms"`
	LastInterval             int           `json:"last_interval_seconds"`
	SampleCount              int           `json:"sample_count"`
	SecondsSinceLastAnnounce int           `json:"seconds_since_last_announce"`
	HasStats                 bool          `json:"has_stats"`
}

// SimpleStats holds only essential statistics for console output
type SimpleStats struct {
	PendingJobs        int `json:"pending_jobs"`
	TrackedHashes      int `json:"tracked_hashes"`
	DisabledForwarders int `json:"disabled_forwarders"`
	ActiveForwarders   int `json:"active_forwarders"`
	ActiveWorkers      int `json:"active_workers"`
	ActiveClients      int `json:"active_clients"`
}

// HashPeerStat represents peer statistics for a hash
type HashPeerStat struct {
	LocalUnique     int `json:"local_unique"`
	ForwarderUnique int `json:"forwarder_unique"`
	Complete        int `json:"complete"`
	Incomplete      int `json:"incomplete"`
}

// ClientStats represents client statistics
type ClientStats struct {
	ActiveClients int          `json:"active_clients"`
	Clients       []ClientInfo `json:"clients,omitempty"`
}

// ClientInfo represents information about a client
type ClientInfo struct {
	Key                 string `json:"key"`
	AnnouncedHashes     int    `json:"announced_hashes"`
	SecondsSinceLastReq int    `json:"seconds_since_last_request"`
}

// StatsDataProvider interface for collecting stats data
type StatsDataProvider interface {
	GetPendingCount() int
	GetTrackedHashes() int
	GetDisabledForwarders() int
	GetActiveForwarders() int
	GetForwarders() []ForwarderStat
	GetHashPeerStats() map[string]HashPeerStat
	GetClientStats() *ClientStats
	GetQueueMetrics() (depth, capacity, fillPct int)
	GetWorkerMetrics() (active, max int)
	GetDropCounters() (droppedFull, rateLimited, throttled uint64)
	GetConfig() *config.Config
}

// NewStatsCollector creates a new stats collector
func NewStatsCollector() *StatsCollector {
	return &StatsCollector{}
}

// CollectStats collects statistics from a data provider
func (sc *StatsCollector) CollectStats(provider StatsDataProvider) *Stats {
	stats := &Stats{
		Config:             provider.GetConfig(),
		PendingJobs:        provider.GetPendingCount(),
		TrackedHashes:      provider.GetTrackedHashes(),
		DisabledForwarders: provider.GetDisabledForwarders(),
		ActiveForwarders:   provider.GetActiveForwarders(),
		HashPeerStats:      provider.GetHashPeerStats(),
		ClientStats:        provider.GetClientStats(),
	}

	// Get forwarder stats directly from provider
	stats.Forwarders = provider.GetForwarders()

	// Queue and worker metrics
	stats.QueueDepth, stats.QueueCapacity, stats.QueueFillPct = provider.GetQueueMetrics()
	stats.ActiveWorkers, stats.MaxWorkers = provider.GetWorkerMetrics()
	stats.DroppedFull, stats.RateLimited, stats.ThrottledForwarders = provider.GetDropCounters()

	return stats
}

// CollectSimpleStats collects only essential statistics from a data provider
func (sc *StatsCollector) CollectSimpleStats(provider StatsDataProvider) *SimpleStats {
	activeClients := 0
	if clientStats := provider.GetClientStats(); clientStats != nil {
		activeClients = clientStats.ActiveClients
	}
	activeWorkers, _ := provider.GetWorkerMetrics()

	return &SimpleStats{
		PendingJobs:        provider.GetPendingCount(),
		TrackedHashes:      provider.GetTrackedHashes(),
		DisabledForwarders: provider.GetDisabledForwarders(),
		ActiveForwarders:   provider.GetActiveForwarders(),
		ActiveWorkers:      activeWorkers,
		ActiveClients:      activeClients,
	}
}

// FormatSimpleText formats simple stats as an aligned table for console output
func (sc *StatsCollector) FormatSimpleText(stats *SimpleStats) string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Pending Jobs:", stats.PendingJobs))
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Tracked Hashes:", stats.TrackedHashes))
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Disabled Forwarders:", stats.DisabledForwarders))
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Active Forwarders:", stats.ActiveForwarders))
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Workers:", stats.ActiveWorkers))
	sb.WriteString(fmt.Sprintf("%-25s %10d\n", "Active Clients:", stats.ActiveClients))
	sb.WriteString("\n")
	return sb.String()
}

// FormatText formats stats as text (detailed format for /stats endpoint)
func (sc *StatsCollector) FormatText(stats *Stats) string {
	var sb strings.Builder

	sb.WriteString("\n=== Statistics ===\n")

	// Print configuration section
	if stats.Config != nil {
		sb.WriteString("\n=== Configuration ===\n")
		sb.WriteString(fmt.Sprintf("HTTP Listen: %s\n", stats.Config.Listen))
		if stats.Config.UDPListen != "" {
			sb.WriteString(fmt.Sprintf("UDP Listen: %s\n", stats.Config.UDPListen))
		} else {
			sb.WriteString("UDP Listen: disabled\n")
		}
		sb.WriteString(fmt.Sprintf("Debug Mode: %v\n", stats.Config.Debug))
		sb.WriteString(fmt.Sprintf("X-Real-IP Header: %v\n", stats.Config.XRealIP))
		sb.WriteString(fmt.Sprintf("Prometheus Metrics: %v\n", stats.Config.PrometheusEnabled))
		sb.WriteString(fmt.Sprintf("Peer Age (minutes): %.1f\n", stats.Config.Age))
		sb.WriteString(fmt.Sprintf("Announce Interval: %d seconds\n", stats.Config.AnnounceInterval))
		if stats.Config.TrackerID != "" {
			sb.WriteString(fmt.Sprintf("Tracker ID: %s\n", stats.Config.TrackerID))
		}
		sb.WriteString(fmt.Sprintf("Statistics Interval: %d seconds\n", stats.Config.StatsInterval))
		forwardersCount := stats.Config.ForwardersCount()
		if forwardersCount > 0 {
			sb.WriteString(fmt.Sprintf("Forwarders: %d configured\n", forwardersCount))
			if stats.Config.ForwardsFile != "" {
				sb.WriteString(fmt.Sprintf("  Forwards File: %s\n", stats.Config.ForwardsFile))
			}
			sb.WriteString(fmt.Sprintf("  Forward Timeout: %d seconds\n", stats.Config.ForwardTimeout))
			sb.WriteString(fmt.Sprintf("  Forwarder Workers: %d (max %d)\n", stats.Config.ForwarderWorkers, stats.Config.MaxForwarderWorkers))
			sb.WriteString(fmt.Sprintf("  Forwarder Queue Size: %d\n", stats.Config.ForwarderQueueSize))
			sb.WriteString(fmt.Sprintf("  Queue Scale Threshold: %d%%\n", stats.Config.QueueScaleThresholdPct))
			sb.WriteString(fmt.Sprintf("  Queue Rate Limit Threshold: %d%%\n", stats.Config.QueueRateLimitThreshold))
			sb.WriteString(fmt.Sprintf("  Queue Throttle Threshold: %d%% (limit to %d forwarders)\n", stats.Config.QueueThrottleThreshold, stats.Config.QueueThrottleTopN))
			sb.WriteString(fmt.Sprintf("  Rate Limit Initial: %d/sec (burst %d)\n", stats.Config.RateLimitInitialPerSec, stats.Config.RateLimitInitialBurst))
			sb.WriteString(fmt.Sprintf("  Forwarder suspend: %d seconds\n", stats.Config.ForwarderSuspendSeconds))
			sb.WriteString(fmt.Sprintf("  Forwarder Fail Threshold: %d\n", stats.Config.ForwarderFailThreshold))
			sb.WriteString(fmt.Sprintf("  Forwarder Retry Attempts: %d\n", stats.Config.ForwarderRetryAttempts))
			sb.WriteString(fmt.Sprintf("  Forwarder Retry Base (ms): %d\n", stats.Config.ForwarderRetryBaseMs))
		} else {
			sb.WriteString("Forwarders: none configured\n")
		}
		sb.WriteString("==================\n")
	}
	sb.WriteString(fmt.Sprintf("Pending jobs: %d\n", stats.PendingJobs))
	sb.WriteString(fmt.Sprintf("Tracked hashes: %d\n", stats.TrackedHashes))
	sb.WriteString(fmt.Sprintf("Disabled forwarders: %d\n", stats.DisabledForwarders))
	sb.WriteString(fmt.Sprintf("Active forwarders: %d\n", stats.ActiveForwarders))
	sb.WriteString(fmt.Sprintf("Queue: depth %d / %d (%d%%)\n", stats.QueueDepth, stats.QueueCapacity, stats.QueueFillPct))
	sb.WriteString(fmt.Sprintf("Workers: %d active / %d max\n", stats.ActiveWorkers, stats.MaxWorkers))
	sb.WriteString(fmt.Sprintf("Queue drops (full): %d\n", stats.DroppedFull))
	sb.WriteString(fmt.Sprintf("Rate limited: %d\n", stats.RateLimited))
	sb.WriteString(fmt.Sprintf("Throttled forwarders skipped: %d\n", stats.ThrottledForwarders))

	// Format forwarders as a table
	if len(stats.Forwarders) > 0 {
		// Find maximum tracker name length for proper padding
		maxNameLen := 0
		for _, forwarder := range stats.Forwarders {
			if len(forwarder.Name) > maxNameLen {
				maxNameLen = len(forwarder.Name)
			}
		}
		// Ensure minimum width for readability
		if maxNameLen < 20 {
			maxNameLen = 20
		}

		sb.WriteString("\nForwarders:\n")
		// Order: Protocol, Tracker, AvgResponse, LastInterval, AnnounceCount, SecondsFromLastAnnounce
		headerFormat := fmt.Sprintf("%%-10s %%-%ds %%-20s %%-15s %%-15s %%-25s\n", maxNameLen)
		sb.WriteString(fmt.Sprintf(headerFormat, "Protocol", "Tracker", "AvgResponse", "LastInterval", "AnnounceCount", "SecondsFromLastAnnounce"))
		sb.WriteString(strings.Repeat("-", 10+maxNameLen+20+15+15+25+5) + "\n")
		for _, forwarder := range stats.Forwarders {
			protocol := forwarder.Protocol
			if protocol == "" {
				protocol = "-"
			}
			if forwarder.HasStats {
				responseTime := forwarder.AvgResponseTime.Round(time.Millisecond).String()
				rowFormat := fmt.Sprintf("%%-10s %%-%ds %%-20s %%-15d %%-15d %%-25d\n", maxNameLen)
				sb.WriteString(fmt.Sprintf(rowFormat,
					protocol, forwarder.Name, responseTime, forwarder.LastInterval,
					forwarder.SampleCount, forwarder.SecondsSinceLastAnnounce))
			} else {
				rowFormat := fmt.Sprintf("%%-10s %%-%ds %%-20s %%-15s %%-15s %%-25s\n", maxNameLen)
				sb.WriteString(fmt.Sprintf(rowFormat,
					protocol, forwarder.Name, "no statistics yet", "-", "-", "-"))
			}
		}
	}

	// Print per-hash stats as a table with scrape-like info
	if len(stats.HashPeerStats) > 0 {
		sb.WriteString("\nTracked Hashes:\n")
		sb.WriteString(fmt.Sprintf("%-40s %-10s %-12s %-15s %-18s\n",
			"Hash", "Complete", "Incomplete", "LocalUnique", "ForwarderUnique"))
		sb.WriteString(strings.Repeat("-", 95) + "\n")
		// Sort hashes for consistent output
		hashes := make([]string, 0, len(stats.HashPeerStats))
		for hash := range stats.HashPeerStats {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		for _, hash := range hashes {
			hashStats := stats.HashPeerStats[hash]
			sb.WriteString(fmt.Sprintf("%-40s %-10d %-12d %-15d %-18d\n",
				hash, hashStats.Complete, hashStats.Incomplete,
				hashStats.LocalUnique, hashStats.ForwarderUnique))
		}
	}

	// Print client statistics as a table
	if stats.ClientStats != nil {
		if stats.ClientStats.ActiveClients == 0 {
			sb.WriteString("\nActive clients: 0\n")
		} else {
			sb.WriteString(fmt.Sprintf("\nActive clients: %d\n", stats.ClientStats.ActiveClients))
			sb.WriteString(fmt.Sprintf("%-50s %-18s %-25s\n", "Key", "AnnouncedHashes", "SecondsSinceLastReq"))
			sb.WriteString(strings.Repeat("-", 93) + "\n")
			for _, client := range stats.ClientStats.Clients {
				sb.WriteString(fmt.Sprintf("%-50s %-18d %-25d\n",
					client.Key, client.AnnouncedHashes, client.SecondsSinceLastReq))
			}
		}
	}

	sb.WriteString("==================\n\n")
	return sb.String()
}

// FormatJSON formats stats as JSON
func (sc *StatsCollector) FormatJSON(stats *Stats) ([]byte, error) {
	// Create a JSON-friendly version of stats
	jsonStats := struct {
		Config              *config.Config          `json:"config,omitempty"`
		PendingJobs         int                     `json:"pending_jobs"`
		TrackedHashes       int                     `json:"tracked_hashes"`
		DisabledForwarders  int                     `json:"disabled_forwarders"`
		ActiveForwarders    int                     `json:"active_forwarders"`
		Forwarders          []ForwarderStatJSON     `json:"forwarders,omitempty"`
		HashPeerStats       map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
		ClientStats         *ClientStats            `json:"client_stats,omitempty"`
		QueueDepth          int                     `json:"queue_depth"`
		QueueCapacity       int                     `json:"queue_capacity"`
		QueueFillPct        int                     `json:"queue_fill_pct"`
		ActiveWorkers       int                     `json:"active_workers"`
		MaxWorkers          int                     `json:"max_workers"`
		DroppedFull         uint64                  `json:"dropped_full"`
		RateLimited         uint64                  `json:"rate_limited"`
		ThrottledForwarders uint64                  `json:"throttled_forwarders"`
	}{
		Config:              stats.Config,
		PendingJobs:         stats.PendingJobs,
		TrackedHashes:       stats.TrackedHashes,
		DisabledForwarders:  stats.DisabledForwarders,
		ActiveForwarders:    stats.ActiveForwarders,
		HashPeerStats:       stats.HashPeerStats,
		ClientStats:         stats.ClientStats,
		QueueDepth:          stats.QueueDepth,
		QueueCapacity:       stats.QueueCapacity,
		QueueFillPct:        stats.QueueFillPct,
		ActiveWorkers:       stats.ActiveWorkers,
		MaxWorkers:          stats.MaxWorkers,
		DroppedFull:         stats.DroppedFull,
		RateLimited:         stats.RateLimited,
		ThrottledForwarders: stats.ThrottledForwarders,
	}

	// Convert forwarder stats
	jsonStats.Forwarders = make([]ForwarderStatJSON, len(stats.Forwarders))
	for i, f := range stats.Forwarders {
		jsonStats.Forwarders[i] = ForwarderStatJSON{
			Name:                     f.Name,
			Protocol:                 f.Protocol,
			AvgResponseTime:          int(f.AvgResponseTime.Milliseconds()),
			LastInterval:             f.LastInterval,
			SampleCount:              f.SampleCount,
			SecondsSinceLastAnnounce: f.SecondsSinceLastAnnounce,
			HasStats:                 f.HasStats,
		}
	}

	return json.MarshalIndent(jsonStats, "", "  ")
}

// ForwarderStatJSON is the JSON representation of ForwarderStat
type ForwarderStatJSON struct {
	Name                     string `json:"name"`
	Protocol                 string `json:"protocol,omitempty"`
	AvgResponseTime          int    `json:"avg_response_time_ms"`
	LastInterval             int    `json:"last_interval_seconds"`
	SampleCount              int    `json:"sample_count"`
	SecondsSinceLastAnnounce int    `json:"seconds_since_last_announce"`
	HasStats                 bool   `json:"has_stats"`
}
