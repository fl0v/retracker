package observability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StatsCollector collects and formats statistics
type StatsCollector struct{}

// ConfigInfo holds server configuration information
type ConfigInfo struct {
	HTTPListen               string  `json:"http_listen"`
	UDPListen                string  `json:"udp_listen"`
	Debug                    bool    `json:"debug"`
	XRealIP                  bool    `json:"x_real_ip"`
	PrometheusEnabled        bool    `json:"prometheus_enabled"`
	Age                      float64 `json:"age"`
	AnnounceResponseInterval int     `json:"announce_response_interval"`
	MinAnnounceInterval      int     `json:"min_announce_interval"`
	TrackerID                string  `json:"tracker_id,omitempty"`
	StatsInterval            int     `json:"stats_interval"`
	ForwardTimeout           int     `json:"forward_timeout"`
	ForwarderWorkers         int     `json:"forwarder_workers"`
	MaxForwarderWorkers      int     `json:"max_forwarder_workers"`
	ForwarderQueueSize       int     `json:"forwarder_queue_size"`
	QueueScaleThresholdPct   int     `json:"queue_scale_threshold_pct"`
	QueueRateLimitThreshold  int     `json:"queue_rate_limit_threshold"`
	QueueThrottleThreshold   int     `json:"queue_throttle_threshold"`
	QueueThrottleTopN        int     `json:"queue_throttle_top_n"`
	RateLimitInitialPerSec   int     `json:"rate_limit_initial_per_sec"`
	RateLimitInitialBurst    int     `json:"rate_limit_initial_burst"`
	ForwarderSuspendSeconds  int     `json:"forwarder_suspend_seconds"`
	ForwarderFailThreshold   int     `json:"forwarder_fail_threshold"`
	ForwarderRetryAttempts   int     `json:"forwarder_retry_attempts"`
	ForwarderRetryBaseMs     int     `json:"forwarder_retry_base_ms"`
	ForwardersCount          int     `json:"forwarders_count"`
	ForwardsFile             string  `json:"forwards_file,omitempty"`
}

// Stats holds all collected statistics
type Stats struct {
	Config                 *ConfigInfo             `json:"config,omitempty"`
	ScheduledAnnouncements int                     `json:"scheduled_announcements"`
	ScheduledAnnounces     []ScheduledAnnounce     `json:"scheduled_announces,omitempty"`
	TrackedHashes          int                     `json:"tracked_hashes"`
	DisabledForwarders     int                     `json:"disabled_forwarders"`
	ActiveForwarders       int                     `json:"active_forwarders"`
	Forwarders             []ForwarderStat         `json:"forwarders,omitempty"`
	HashPeerStats          map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
	ClientStats            *ClientStats            `json:"client_stats,omitempty"`
	QueueDepth             int                     `json:"queue_depth"`
	QueueCapacity          int                     `json:"queue_capacity"`
	QueueFillPct           int                     `json:"queue_fill_pct"`
	ActiveWorkers          int                     `json:"active_workers"`
	MaxWorkers             int                     `json:"max_workers"`
	DroppedFull            uint64                  `json:"dropped_full"`
	RateLimited            uint64                  `json:"rate_limited"`
	ThrottledForwarders    uint64                  `json:"throttled_forwarders"`
}

// ScheduledAnnounce represents a scheduled announcement
type ScheduledAnnounce struct {
	InfoHash      string        `json:"info_hash"`
	ForwarderName string        `json:"forwarder_name"`
	TimeToExec    time.Duration `json:"time_to_exec_seconds"`
}

// ForwarderStat represents statistics for a forwarder
type ForwarderStat struct {
	Name            string        `json:"name"`
	Protocol        string        `json:"protocol,omitempty"`
	AvgResponseTime time.Duration `json:"avg_response_time_ms"`
	AvgInterval     int           `json:"avg_interval_seconds"`
	SampleCount     int           `json:"sample_count"`
	HasStats        bool          `json:"has_stats"`
}

// HashPeerStat represents peer statistics for a hash
type HashPeerStat struct {
	LocalUnique     int `json:"local_unique"`
	ForwarderUnique int `json:"forwarder_unique"`
	TotalUnique     int `json:"total_unique"`
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
	GetScheduledAnnounces() []ScheduledAnnounce
	GetTrackedHashes() int
	GetDisabledForwarders() int
	GetActiveForwarders() int
	GetForwarders() []ForwarderStat
	GetHashPeerStats() map[string]HashPeerStat
	GetClientStats() *ClientStats
	GetQueueMetrics() (depth, capacity, fillPct int)
	GetWorkerMetrics() (active, max int)
	GetDropCounters() (droppedFull, rateLimited, throttled uint64)
	GetConfig() *ConfigInfo
}

// NewStatsCollector creates a new stats collector
func NewStatsCollector() *StatsCollector {
	return &StatsCollector{}
}

// CollectStats collects statistics from a data provider
func (sc *StatsCollector) CollectStats(provider StatsDataProvider) *Stats {
	stats := &Stats{
		Config:                 provider.GetConfig(),
		ScheduledAnnouncements: provider.GetPendingCount(),
		ScheduledAnnounces:     provider.GetScheduledAnnounces(),
		TrackedHashes:          provider.GetTrackedHashes(),
		DisabledForwarders:     provider.GetDisabledForwarders(),
		ActiveForwarders:       provider.GetActiveForwarders(),
		HashPeerStats:          provider.GetHashPeerStats(),
		ClientStats:            provider.GetClientStats(),
	}

	// Get forwarder stats directly from provider
	stats.Forwarders = provider.GetForwarders()

	// Queue and worker metrics
	stats.QueueDepth, stats.QueueCapacity, stats.QueueFillPct = provider.GetQueueMetrics()
	stats.ActiveWorkers, stats.MaxWorkers = provider.GetWorkerMetrics()
	stats.DroppedFull, stats.RateLimited, stats.ThrottledForwarders = provider.GetDropCounters()

	return stats
}

// FormatText formats stats as text (matching console output format)
func (sc *StatsCollector) FormatText(stats *Stats) string {
	var sb strings.Builder

	sb.WriteString("\n=== Statistics ===\n")

	// Print configuration section
	if stats.Config != nil {
		sb.WriteString("\n=== Configuration ===\n")
		sb.WriteString(fmt.Sprintf("HTTP Listen: %s\n", stats.Config.HTTPListen))
		if stats.Config.UDPListen != "" {
			sb.WriteString(fmt.Sprintf("UDP Listen: %s\n", stats.Config.UDPListen))
		} else {
			sb.WriteString("UDP Listen: disabled\n")
		}
		sb.WriteString(fmt.Sprintf("Debug Mode: %v\n", stats.Config.Debug))
		sb.WriteString(fmt.Sprintf("X-Real-IP Header: %v\n", stats.Config.XRealIP))
		sb.WriteString(fmt.Sprintf("Prometheus Metrics: %v\n", stats.Config.PrometheusEnabled))
		sb.WriteString(fmt.Sprintf("Peer Age (minutes): %.1f\n", stats.Config.Age))
		sb.WriteString(fmt.Sprintf("Announce Response Interval: %d seconds\n", stats.Config.AnnounceResponseInterval))
		sb.WriteString(fmt.Sprintf("Minimum Announce Interval: %d seconds\n", stats.Config.MinAnnounceInterval))
		if stats.Config.TrackerID != "" {
			sb.WriteString(fmt.Sprintf("Tracker ID: %s\n", stats.Config.TrackerID))
		}
		sb.WriteString(fmt.Sprintf("Statistics Interval: %d seconds\n", stats.Config.StatsInterval))
		if stats.Config.ForwardersCount > 0 {
			sb.WriteString(fmt.Sprintf("Forwarders: %d configured\n", stats.Config.ForwardersCount))
			if stats.Config.ForwardsFile != "" {
				sb.WriteString(fmt.Sprintf("  Forwards File: %s\n", stats.Config.ForwardsFile))
			}
			sb.WriteString(fmt.Sprintf("  Forward Timeout: %d seconds\n", stats.Config.ForwardTimeout))
			sb.WriteString(fmt.Sprintf("  Forwarder Workers: %d (max %d)\n", stats.Config.ForwarderWorkers, stats.Config.MaxForwarderWorkers))
			sb.WriteString(fmt.Sprintf("  Forwarder Queue Size: %d\n", stats.Config.ForwarderQueueSize))
			sb.WriteString(fmt.Sprintf("  Queue Scale Threshold: %d%%\n", stats.Config.QueueScaleThresholdPct))
			sb.WriteString(fmt.Sprintf("  Queue Rate Limit Threshold: %d%%\n", stats.Config.QueueRateLimitThreshold))
			sb.WriteString(fmt.Sprintf("  Queue Throttle Threshold: %d%% (top %d forwarders)\n", stats.Config.QueueThrottleThreshold, stats.Config.QueueThrottleTopN))
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
	sb.WriteString(fmt.Sprintf("Scheduled announcements: %d\n", stats.ScheduledAnnouncements))

	if len(stats.ScheduledAnnounces) > 0 {
		limit := len(stats.ScheduledAnnounces)
		if limit > 10 {
			limit = 10
		}
		sb.WriteString(fmt.Sprintf("Scheduled announces (next %d):\n", limit))
		for _, sa := range stats.ScheduledAnnounces[:limit] {
			sb.WriteString(fmt.Sprintf("  %s -> %s: %v\n", sa.InfoHash, sa.ForwarderName, sa.TimeToExec.Round(time.Second)))
		}
	}

	sb.WriteString(fmt.Sprintf("Tracked hashes: %d\n", stats.TrackedHashes))
	sb.WriteString(fmt.Sprintf("Disabled forwarders: %d\n", stats.DisabledForwarders))
	sb.WriteString(fmt.Sprintf("Active forwarders: %d\n", stats.ActiveForwarders))
	sb.WriteString(fmt.Sprintf("Queue: depth %d / %d (%d%%)\n", stats.QueueDepth, stats.QueueCapacity, stats.QueueFillPct))
	sb.WriteString(fmt.Sprintf("Workers: %d active / %d max\n", stats.ActiveWorkers, stats.MaxWorkers))
	sb.WriteString(fmt.Sprintf("Queue drops (full): %d\n", stats.DroppedFull))
	sb.WriteString(fmt.Sprintf("Rate limited: %d\n", stats.RateLimited))
	sb.WriteString(fmt.Sprintf("Throttled forwarders skipped: %d\n", stats.ThrottledForwarders))

	for _, forwarder := range stats.Forwarders {
		if forwarder.HasStats {
			protocolStr := ""
			if forwarder.Protocol != "" {
				protocolStr = fmt.Sprintf(" [%s]", forwarder.Protocol)
			}
			sb.WriteString(fmt.Sprintf("  %s%s: avg response time %v, avg interval %ds (from %d samples)\n",
				forwarder.Name, protocolStr, forwarder.AvgResponseTime.Round(time.Millisecond), forwarder.AvgInterval, forwarder.SampleCount))
		} else {
			protocolStr := ""
			if forwarder.Protocol != "" {
				protocolStr = fmt.Sprintf(" [%s]", forwarder.Protocol)
			}
			sb.WriteString(fmt.Sprintf("  %s%s: no statistics yet\n", forwarder.Name, protocolStr))
		}
	}

	// Print per-hash unique peer counts
	if len(stats.HashPeerStats) > 0 {
		sb.WriteString("Hash unique peers (local/forwarders/total):\n")
		// Sort hashes for consistent output
		hashes := make([]string, 0, len(stats.HashPeerStats))
		for hash := range stats.HashPeerStats {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		for _, hash := range hashes {
			hashStats := stats.HashPeerStats[hash]
			sb.WriteString(fmt.Sprintf("  %s: %d/%d/%d\n", hash, hashStats.LocalUnique, hashStats.ForwarderUnique, hashStats.TotalUnique))
		}
	}

	// Print client statistics
	if stats.ClientStats != nil {
		if stats.ClientStats.ActiveClients == 0 {
			sb.WriteString("Active clients: 0\n")
		} else {
			sb.WriteString(fmt.Sprintf("Active clients: %d\n", stats.ClientStats.ActiveClients))
			for _, client := range stats.ClientStats.Clients {
				sb.WriteString(fmt.Sprintf("  %s: %d announced hash(es), %d seconds since last request\n",
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
		Config                 *ConfigInfo             `json:"config,omitempty"`
		ScheduledAnnouncements int                     `json:"scheduled_announcements"`
		ScheduledAnnounces     []ScheduledAnnounceJSON `json:"scheduled_announces,omitempty"`
		TrackedHashes          int                     `json:"tracked_hashes"`
		DisabledForwarders     int                     `json:"disabled_forwarders"`
		ActiveForwarders       int                     `json:"active_forwarders"`
		Forwarders             []ForwarderStatJSON     `json:"forwarders,omitempty"`
		HashPeerStats          map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
		ClientStats            *ClientStats            `json:"client_stats,omitempty"`
		QueueDepth             int                     `json:"queue_depth"`
		QueueCapacity          int                     `json:"queue_capacity"`
		QueueFillPct           int                     `json:"queue_fill_pct"`
		ActiveWorkers          int                     `json:"active_workers"`
		MaxWorkers             int                     `json:"max_workers"`
		DroppedFull            uint64                  `json:"dropped_full"`
		RateLimited            uint64                  `json:"rate_limited"`
		ThrottledForwarders    uint64                  `json:"throttled_forwarders"`
	}{
		Config:                 stats.Config,
		ScheduledAnnouncements: stats.ScheduledAnnouncements,
		TrackedHashes:          stats.TrackedHashes,
		DisabledForwarders:     stats.DisabledForwarders,
		ActiveForwarders:       stats.ActiveForwarders,
		HashPeerStats:          stats.HashPeerStats,
		ClientStats:            stats.ClientStats,
		QueueDepth:             stats.QueueDepth,
		QueueCapacity:          stats.QueueCapacity,
		QueueFillPct:           stats.QueueFillPct,
		ActiveWorkers:          stats.ActiveWorkers,
		MaxWorkers:             stats.MaxWorkers,
		DroppedFull:            stats.DroppedFull,
		RateLimited:            stats.RateLimited,
		ThrottledForwarders:    stats.ThrottledForwarders,
	}

	// Convert scheduled announces
	jsonStats.ScheduledAnnounces = make([]ScheduledAnnounceJSON, len(stats.ScheduledAnnounces))
	for i, sa := range stats.ScheduledAnnounces {
		jsonStats.ScheduledAnnounces[i] = ScheduledAnnounceJSON{
			InfoHash:      sa.InfoHash,
			ForwarderName: sa.ForwarderName,
			TimeToExec:    int(sa.TimeToExec.Seconds()),
		}
	}

	// Convert forwarder stats
	jsonStats.Forwarders = make([]ForwarderStatJSON, len(stats.Forwarders))
	for i, f := range stats.Forwarders {
		jsonStats.Forwarders[i] = ForwarderStatJSON{
			Name:            f.Name,
			Protocol:        f.Protocol,
			AvgResponseTime: int(f.AvgResponseTime.Milliseconds()),
			AvgInterval:     f.AvgInterval,
			SampleCount:     f.SampleCount,
			HasStats:        f.HasStats,
		}
	}

	return json.MarshalIndent(jsonStats, "", "  ")
}

// ScheduledAnnounceJSON is the JSON representation of ScheduledAnnounce
type ScheduledAnnounceJSON struct {
	InfoHash      string `json:"info_hash"`
	ForwarderName string `json:"forwarder_name"`
	TimeToExec    int    `json:"time_to_exec_seconds"`
}

// ForwarderStatJSON is the JSON representation of ForwarderStat
type ForwarderStatJSON struct {
	Name            string `json:"name"`
	Protocol        string `json:"protocol,omitempty"`
	AvgResponseTime int    `json:"avg_response_time_ms"`
	AvgInterval     int    `json:"avg_interval_seconds"`
	SampleCount     int    `json:"sample_count"`
	HasStats        bool   `json:"has_stats"`
}
