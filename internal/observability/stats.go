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

// Stats holds all collected statistics
type Stats struct {
	ScheduledAnnouncements int                     `json:"scheduled_announcements"`
	ScheduledAnnounces     []ScheduledAnnounce     `json:"scheduled_announces,omitempty"`
	TrackedHashes          int                     `json:"tracked_hashes"`
	DisabledForwarders     int                     `json:"disabled_forwarders"`
	ActiveForwarders       int                     `json:"active_forwarders"`
	Forwarders             []ForwarderStat         `json:"forwarders,omitempty"`
	HashPeerStats          map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
	ClientStats            *ClientStats            `json:"client_stats,omitempty"`
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
}

// NewStatsCollector creates a new stats collector
func NewStatsCollector() *StatsCollector {
	return &StatsCollector{}
}

// CollectStats collects statistics from a data provider
func (sc *StatsCollector) CollectStats(provider StatsDataProvider) *Stats {
	stats := &Stats{
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

	return stats
}

// FormatText formats stats as text (matching console output format)
func (sc *StatsCollector) FormatText(stats *Stats) string {
	var sb strings.Builder

	sb.WriteString("\n=== Statistics ===\n")
	sb.WriteString(fmt.Sprintf("Scheduled announcements: %d\n", stats.ScheduledAnnouncements))

	if len(stats.ScheduledAnnounces) > 0 {
		limit := len(stats.ScheduledAnnounces)
		if limit > 10 {
			limit = 10
		}
		sb.WriteString(fmt.Sprintf("Scheduled announces (next %d):\n", limit))
		for _, sa := range stats.ScheduledAnnounces[:limit] {
			hashDisplay := sa.InfoHash
			if len(hashDisplay) > 16 {
				hashDisplay = hashDisplay[:16] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s -> %s: %v\n", hashDisplay, sa.ForwarderName, sa.TimeToExec.Round(time.Second)))
		}
	}

	sb.WriteString(fmt.Sprintf("Tracked hashes: %d\n", stats.TrackedHashes))
	sb.WriteString(fmt.Sprintf("Disabled forwarders: %d\n", stats.DisabledForwarders))
	sb.WriteString(fmt.Sprintf("Active forwarders: %d\n", stats.ActiveForwarders))

	for _, forwarder := range stats.Forwarders {
		if forwarder.HasStats {
			sb.WriteString(fmt.Sprintf("  %s: avg response time %v, avg interval %ds (from %d samples)\n",
				forwarder.Name, forwarder.AvgResponseTime.Round(time.Millisecond), forwarder.AvgInterval, forwarder.SampleCount))
		} else {
			sb.WriteString(fmt.Sprintf("  %s: no statistics yet\n", forwarder.Name))
		}
	}

	// Print per-hash unique IP counts
	if len(stats.HashPeerStats) > 0 {
		sb.WriteString("Hash unique IPs (local/forwarders/total):\n")
		// Sort hashes for consistent output
		hashes := make([]string, 0, len(stats.HashPeerStats))
		for hash := range stats.HashPeerStats {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		for _, hash := range hashes {
			hashStats := stats.HashPeerStats[hash]
			hashDisplay := hash
			if len(hashDisplay) > 16 {
				hashDisplay = hashDisplay[:16] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s: %d/%d/%d\n", hashDisplay, hashStats.LocalUnique, hashStats.ForwarderUnique, hashStats.TotalUnique))
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
		ScheduledAnnouncements int                     `json:"scheduled_announcements"`
		ScheduledAnnounces     []ScheduledAnnounceJSON `json:"scheduled_announces,omitempty"`
		TrackedHashes          int                     `json:"tracked_hashes"`
		DisabledForwarders     int                     `json:"disabled_forwarders"`
		ActiveForwarders       int                     `json:"active_forwarders"`
		Forwarders             []ForwarderStatJSON     `json:"forwarders,omitempty"`
		HashPeerStats          map[string]HashPeerStat `json:"hash_peer_stats,omitempty"`
		ClientStats            *ClientStats            `json:"client_stats,omitempty"`
	}{
		ScheduledAnnouncements: stats.ScheduledAnnouncements,
		TrackedHashes:          stats.TrackedHashes,
		DisabledForwarders:     stats.DisabledForwarders,
		ActiveForwarders:       stats.ActiveForwarders,
		HashPeerStats:          stats.HashPeerStats,
		ClientStats:            stats.ClientStats,
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
	AvgResponseTime int    `json:"avg_response_time_ms"`
	AvgInterval     int    `json:"avg_interval_seconds"`
	SampleCount     int    `json:"sample_count"`
	HasStats        bool   `json:"has_stats"`
}
