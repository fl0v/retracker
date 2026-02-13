// forwarderManager.go - Core forwarder orchestration and worker pool management.
// Manages job queue, worker scaling, rate limiting, throttling, and forwarder lifecycle.
// Handles job scheduling, deduplication, and forwarder enable/disable/suspend logic.
package server

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

const (
	EventStopped   = "stopped"
	EventCompleted = "completed"
)

var (
	InfoLogFwd  = log.New(os.Stdout, `info#`, log.Lshortfile)
	DebugLogFwd = log.New(os.Stdout, `debug#`, log.Lshortfile)
	WarnLogFwd  = log.New(os.Stderr, `warn#`, log.Lshortfile)
	ErrorLogFwd = log.New(os.Stderr, `error#`, log.Lshortfile)
)

// ForwarderStats tracks statistics per forwarder (aggregated across all hashes).
type ForwarderStats struct {
	AvgResponseTime  time.Duration
	LastInterval     int
	LastAnnounceTime time.Time
	SampleCount      int
	mu               sync.RWMutex
}

// DisabledForwarder stores info about a disabled forwarder
type DisabledForwarder struct {
	Reason       string
	DisabledAt   int64
	FailureCount int
}

// SuspendedForwarder stores info about a temporarily suspended forwarder
type SuspendedForwarder struct {
	Reason         string
	SuspendedAt    time.Time
	SuspendedUntil time.Time
}

const maxReasonLength = 200

// ForwarderManager manages forwarding announces to external trackers
type ForwarderManager struct {
	Config             *config.Config
	Storage            *ForwarderStorage
	MainStorage        *Storage
	Forwarders         []CoreCommon.Forward
	Workers            int
	jobQueue           chan AnnounceJob
	stopChan           chan struct{}
	Prometheus         *observability.Prometheus
	TempStorage        *TempStorage
	pendingJobs        map[string]bool
	pendingMu          sync.Mutex
	stats              map[string]*ForwarderStats
	statsMu            sync.RWMutex
	udpForwarder       *UDPForwarder
	failCounts         map[string]int
	disabledForwarders map[string]*DisabledForwarder
	forwardersMu       sync.RWMutex

	// Queue/worker control
	maxWorkers               int
	queueScaleThreshold      int
	queueRateLimitThresh     int
	queueThrottleThresh      int
	queueThrottleTopN        int
	maxForwardersPerAnnounce int
	queueThrottleLogMu       sync.Mutex
	lastThrottleLogFillPct   int
	workerCount              int
	workerCountMu            sync.Mutex
	scaleDownChan            chan struct{}
	rateLimiter              *tokenBucket
	droppedFullCount         uint64
	rateLimitedCount         uint64

	suspendedMu         sync.Mutex
	suspendedForwarders map[string]*SuspendedForwarder

	// Scheduled jobs
	scheduledJobs   map[time.Time][]AnnounceJob
	scheduledJobsMu sync.RWMutex
}

type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	rate     int
	burst    int
	lastTick time.Time
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	if tb.lastTick.IsZero() {
		tb.lastTick = now
		tb.tokens = tb.burst
	}
	elapsed := now.Sub(tb.lastTick)
	if elapsed > 0 && tb.rate > 0 {
		refill := int(elapsed.Seconds() * float64(tb.rate))
		if refill > 0 {
			tb.tokens = min(tb.burst, tb.tokens+refill)
			tb.lastTick = now
		}
	}
	if tb.tokens > 0 {
		tb.tokens--
		return true
	}
	return false
}

// NewForwarderManager creates a new ForwarderManager
func NewForwarderManager(cfg *config.Config, storage *ForwarderStorage, mainStorage *Storage, prom *observability.Prometheus, tempStorage *TempStorage) *ForwarderManager {
	queueSize := cfg.ForwarderQueueSize
	if queueSize <= 0 {
		queueSize = 1000
	}
	maxWorkers := cfg.MaxForwarderWorkers
	if maxWorkers <= 0 {
		maxWorkers = cfg.ForwarderWorkers * 2
	}
	queueScaleThresh := cfg.QueueScaleThresholdPct
	if queueScaleThresh <= 0 {
		queueScaleThresh = 60
	}
	queueRateLimitThresh := cfg.QueueRateLimitThreshold
	if queueRateLimitThresh <= 0 {
		queueRateLimitThresh = 80
	}
	queueThrottleThresh := cfg.QueueThrottleThreshold
	if queueThrottleThresh <= 0 {
		queueThrottleThresh = 60
	}
	queueThrottleTopN := cfg.QueueThrottleTopN
	if queueThrottleTopN <= 0 {
		queueThrottleTopN = 20
	}
	maxForwardersPerAnnounce := cfg.MaxForwardersPerAnnounce
	if maxForwardersPerAnnounce <= 0 {
		maxForwardersPerAnnounce = 100
	}
	ratePerSec := cfg.RateLimitInitialPerSec
	if ratePerSec <= 0 {
		ratePerSec = 100
	}
	rateBurst := cfg.RateLimitInitialBurst
	if rateBurst <= 0 {
		rateBurst = 200
	}

	fm := &ForwarderManager{
		Config:                   cfg,
		Storage:                  storage,
		MainStorage:              mainStorage,
		Forwarders:               cfg.Forwards,
		Workers:                  cfg.ForwarderWorkers,
		jobQueue:                 make(chan AnnounceJob, queueSize),
		stopChan:                 make(chan struct{}),
		Prometheus:               prom,
		TempStorage:              tempStorage,
		pendingJobs:              make(map[string]bool),
		stats:                    make(map[string]*ForwarderStats),
		udpForwarder:             NewUDPForwarder(cfg.Debug, cfg.ForwardTimeout, cfg.ForwarderRetryAttempts, cfg.ForwarderRetryBaseMs),
		failCounts:               make(map[string]int),
		disabledForwarders:       make(map[string]*DisabledForwarder),
		maxWorkers:               maxWorkers,
		queueScaleThreshold:      queueScaleThresh,
		queueRateLimitThresh:     queueRateLimitThresh,
		queueThrottleThresh:      queueThrottleThresh,
		queueThrottleTopN:        queueThrottleTopN,
		maxForwardersPerAnnounce: maxForwardersPerAnnounce,
		lastThrottleLogFillPct:   -1,
		workerCount:              cfg.ForwarderWorkers,
		rateLimiter: &tokenBucket{
			rate:   ratePerSec,
			burst:  rateBurst,
			tokens: rateBurst,
		},
		suspendedForwarders: make(map[string]*SuspendedForwarder),
		scaleDownChan:       make(chan struct{}, 100),
		scheduledJobs:       make(map[time.Time][]AnnounceJob),
	}

	for _, forwarder := range cfg.Forwards {
		fm.stats[forwarder.GetName()] = &ForwarderStats{SampleCount: 0}
	}

	// Check and remove forwarders with invalid configuration
	fm.validateAndRemoveInvalidForwarders()

	// Check and remove forwarders that resolve to blocked IPs (localhost/private)
	fm.checkAndRemoveBlockedForwarders()

	// Remove duplicate forwarders by main domain
	fm.removeDuplicateForwardersByDomain()

	return fm
}

func (fm *ForwarderManager) Start() {
	for i := 0; i < fm.Workers; i++ {
		go fm.worker()
	}
	if fm.Prometheus != nil {
		fm.Prometheus.WorkerCount.Set(float64(fm.workerCount))
	}
	go fm.scaleWorkers()
	go fm.schedulerRoutine()

	if fm.Config.StatsInterval > 0 {
		go fm.statsRoutine()
	}
}

func (fm *ForwarderManager) Stop() {
	close(fm.stopChan)
	close(fm.jobQueue)
	if fm.udpForwarder != nil {
		fm.udpForwarder.Stop()
	}
}

// Job management

func (fm *ForwarderManager) jobKey(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) string {
	return fmt.Sprintf("%x:%s:%x", infoHash, forwarderName, peerID)
}

func (fm *ForwarderManager) isJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) bool {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	return fm.pendingJobs[fm.jobKey(infoHash, forwarderName, peerID)]
}

func (fm *ForwarderManager) markJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	fm.pendingJobs[fm.jobKey(infoHash, forwarderName, peerID)] = true
}

func (fm *ForwarderManager) unmarkJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	delete(fm.pendingJobs, fm.jobKey(infoHash, forwarderName, peerID))
}

// Worker pool

func (fm *ForwarderManager) worker() {
	baseWorkers := fm.Workers
	for {
		select {
		case <-fm.stopChan:
			return
		case <-fm.scaleDownChan:
			fm.workerCountMu.Lock()
			if fm.workerCount > baseWorkers {
				fm.workerCount--
				fm.workerCountMu.Unlock()
				InfoLogFwd.Printf("Worker scaling down, remaining: %d\n", fm.workerCount)
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(fm.workerCount))
				}
				return
			}
			fm.workerCountMu.Unlock()
		case job, ok := <-fm.jobQueue:
			if !ok {
				return
			}
			if !job.ScheduledTime.IsZero() && time.Now().Before(job.ScheduledTime) {
				if !fm.scheduleJob(job) {
					fm.unmarkJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
				}
				continue
			}
			fm.executeAnnounce(job)
			fm.unmarkJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
		}
	}
}

func (fm *ForwarderManager) scaleWorkers() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-fm.stopChan:
			return
		case <-ticker.C:
			fillPct := fm.queueFillPct()
			fm.workerCountMu.Lock()
			currentCount := fm.workerCount
			baseWorkers := fm.Workers

			switch {
			case fillPct >= fm.queueScaleThreshold && currentCount < fm.maxWorkers:
				fm.workerCount++
				newCount := fm.workerCount
				fm.workerCountMu.Unlock()
				go fm.worker()
				InfoLogFwd.Printf("Scaling workers up to %d (queue %d%%)\n", newCount, fillPct)
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(newCount))
				}
			case fillPct < 40 && currentCount > baseWorkers:
				select {
				case fm.scaleDownChan <- struct{}{}:
					InfoLogFwd.Printf("Signaling worker to scale down (queue %d%%, workers: %d)\n", fillPct, currentCount)
				default:
				}
				fm.workerCountMu.Unlock()
			default:
				fm.workerCountMu.Unlock()
			}
		}
	}
}

func (fm *ForwarderManager) queueFillPct() int {
	capacity := cap(fm.jobQueue)
	if capacity == 0 {
		return 0
	}
	return len(fm.jobQueue) * 100 / capacity
}

func (fm *ForwarderManager) isQueueFull() bool {
	return len(fm.jobQueue) >= cap(fm.jobQueue)
}

func (fm *ForwarderManager) updatePrometheusQueue(stats *observability.Stats) {
	fm.Prometheus.QueueDepth.Set(float64(stats.QueueDepth))
	fm.Prometheus.QueueCapacity.Set(float64(stats.QueueCapacity))
	fm.Prometheus.QueueFillPct.Set(float64(stats.QueueFillPct))
	fm.Prometheus.WorkerCount.Set(float64(stats.ActiveWorkers))
	fm.Prometheus.DisabledForwarders.Set(float64(stats.DisabledForwarders))
	fm.Prometheus.SuspendedForwarders.Set(float64(stats.SuspendedForwarders))

	// Keep metric labels aligned with current state by resetting detail vectors before repopulating.
	fm.Prometheus.DisabledForwarderInfo.Reset()
	fm.Prometheus.DisabledForwarderFailureCount.Reset()
	fm.Prometheus.DisabledForwarderSecondsSince.Reset()
	for _, disabled := range stats.DisabledDetails {
		fm.Prometheus.DisabledForwarderInfo.WithLabelValues(disabled.Name, disabled.Reason).Set(1)
		fm.Prometheus.DisabledForwarderFailureCount.WithLabelValues(disabled.Name).Set(float64(disabled.FailureCount))
		fm.Prometheus.DisabledForwarderSecondsSince.WithLabelValues(disabled.Name).Set(float64(disabled.SecondsSinceDisabled))
	}

	fm.Prometheus.SuspendedForwarderInfo.Reset()
	fm.Prometheus.SuspendedForwarderSecondsRemain.Reset()
	for _, suspended := range stats.SuspendedDetails {
		fm.Prometheus.SuspendedForwarderInfo.WithLabelValues(suspended.Name, suspended.Reason).Set(1)
		fm.Prometheus.SuspendedForwarderSecondsRemain.WithLabelValues(suspended.Name).Set(float64(suspended.SecondsRemaining))
	}
}

// Failure management

func (fm *ForwarderManager) registerFailure(forwarderName string) {
	if fm.Config.ForwarderFailThreshold <= 0 {
		return
	}
	fm.pendingMu.Lock()
	fm.failCounts[forwarderName]++
	count := fm.failCounts[forwarderName]
	threshold := fm.Config.ForwarderFailThreshold
	fm.pendingMu.Unlock()

	if count >= threshold {
		fm.disableForwarder(forwarderName, "repeated failures (threshold reached)")
	}
}

func (fm *ForwarderManager) resetFailure(forwarderName string) {
	fm.pendingMu.Lock()
	delete(fm.failCounts, forwarderName)
	fm.pendingMu.Unlock()
}

func (fm *ForwarderManager) disableForwarder(forwarderName string, reason string) {
	fm.pendingMu.Lock()
	failureCount := fm.failCounts[forwarderName]
	delete(fm.failCounts, forwarderName)
	fm.pendingMu.Unlock()

	fm.forwardersMu.Lock()
	forwarderIndex := -1
	for i, f := range fm.Forwarders {
		if f.GetName() == forwarderName {
			forwarderIndex = i
			break
		}
	}

	if forwarderIndex >= 0 {
		truncatedReason := reason
		if len(truncatedReason) > maxReasonLength {
			truncatedReason = truncatedReason[:maxReasonLength] + "..."
		}

		fm.pendingMu.Lock()
		fm.disabledForwarders[forwarderName] = &DisabledForwarder{
			Reason:       truncatedReason,
			DisabledAt:   time.Now().Unix(),
			FailureCount: failureCount,
		}
		fm.pendingMu.Unlock()

		fm.Forwarders = append(fm.Forwarders[:forwarderIndex], fm.Forwarders[forwarderIndex+1:]...)
	}
	fm.forwardersMu.Unlock()

	fm.statsMu.Lock()
	delete(fm.stats, forwarderName)
	fm.statsMu.Unlock()

	fm.Storage.mu.Lock()
	for infoHash := range fm.Storage.Entries {
		delete(fm.Storage.Entries[infoHash], forwarderName)
		if len(fm.Storage.Entries[infoHash]) == 0 {
			delete(fm.Storage.Entries, infoHash)
		}
	}
	fm.Storage.mu.Unlock()

	InfoLogFwd.Printf("Disabled forwarder %s: %s (failure count: %d)", forwarderName, reason, failureCount)
}

func (fm *ForwarderManager) isDisabled(forwarderName string) bool {
	fm.pendingMu.Lock()
	_, disabled := fm.disabledForwarders[forwarderName]
	fm.pendingMu.Unlock()
	return disabled
}

func (fm *ForwarderManager) isSuspended(forwarderName string) bool {
	_, suspended := fm.getSuspendedUntil(forwarderName, time.Now())
	return suspended
}

func (fm *ForwarderManager) getSuspendedUntil(forwarderName string, now time.Time) (time.Time, bool) {
	fm.suspendedMu.Lock()
	defer fm.suspendedMu.Unlock()
	entry, ok := fm.suspendedForwarders[forwarderName]
	if !ok {
		return time.Time{}, false
	}
	until := entry.SuspendedUntil
	if now.After(until) {
		delete(fm.suspendedForwarders, forwarderName)
		return time.Time{}, false
	}
	return until, true
}

func (fm *ForwarderManager) suspendForwarder(forwarderName string, duration time.Duration) {
	fm.suspendForwarderWithReason(forwarderName, duration, "temporary suspension")
}

func (fm *ForwarderManager) suspendForwarderWithReason(forwarderName string, duration time.Duration, reason string) {
	now := time.Now()
	truncatedReason := reason
	if len(truncatedReason) > maxReasonLength {
		truncatedReason = truncatedReason[:maxReasonLength] + "..."
	}

	fm.suspendedMu.Lock()
	fm.suspendedForwarders[forwarderName] = &SuspendedForwarder{
		Reason:         truncatedReason,
		SuspendedAt:    now,
		SuspendedUntil: now.Add(duration),
	}
	fm.suspendedMu.Unlock()
	InfoLogFwd.Printf("Suspended forwarder %s for %v (reason: %s)\n", forwarderName, duration, truncatedReason)
}

// DNS blocking checks

// isBlockedIP checks if an IP address is in a blocked range
// Returns true for 127.x.x.x (localhost) or private network ranges per RFC 1918:
// - 10.0.0.0/8 (10.0.0.0 to 10.255.255.255)
// - 172.16.0.0/12 (172.16.0.0 to 172.31.255.255)
// - 192.168.0.0/16 (192.168.0.0 to 192.168.255.255)
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}

	first, second := ipv4[0], ipv4[1]

	// Check for 127.0.0.0/8 (localhost) or RFC 1918 private network ranges
	return first == 127 || // localhost
		first == 10 || // 10.0.0.0/8
		(first == 172 && second >= 16 && second <= 31) || // 172.16.0.0/12
		(first == 192 && second == 168) // 192.168.0.0/16
}

// isBlockedByDNS checks if a hostname resolves to blocked IPs
// Returns true if any resolved IP is in blocked ranges (127.x.x.x or 192.168.x.x)
func isBlockedByDNS(hostname string) (bool, []net.IP, error) {
	if hostname == "" {
		return false, nil, fmt.Errorf("empty hostname")
	}

	// Resolve hostname to IP addresses
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return false, nil, fmt.Errorf("DNS lookup failed: %w", err)
	}

	// Check if any IP is blocked
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return true, ips, nil
		}
	}

	return false, ips, nil
}

// checkAndRemoveBlockedForwarders checks all forwarders and immediately removes
// any that resolve to localhost (127.x.x.x) or private IPs per RFC 1918
// (10.x.x.x, 172.16.x.x-172.31.x.x, 192.168.x.x)
func (fm *ForwarderManager) checkAndRemoveBlockedForwarders() {
	if len(fm.Forwarders) == 0 {
		return
	}

	// Make a copy of forwarders to iterate over (since disableForwarder modifies the list)
	fm.forwardersMu.RLock()
	forwardersCopy := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwardersCopy, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	blockedCount := 0

	for _, forwarder := range forwardersCopy {
		// Extract hostname from URI
		u, err := url.Parse(forwarder.Uri)
		if err != nil {
			// If we can't parse the URI, skip DNS check (let other error handling deal with it)
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping DNS check for forwarder %s: invalid URI: %v\n", forwarder.GetName(), err)
			}
			continue
		}

		hostname := u.Hostname()
		if hostname == "" {
			// No hostname to check
			continue
		}

		// Check if hostname resolves to blocked IPs
		blocked, resolvedIPs, err := isBlockedByDNS(hostname)
		if err != nil {
			// DNS lookup failed - keep the forwarder (let it fail during announce)
			if fm.Config.Debug {
				DebugLogFwd.Printf("DNS lookup failed for forwarder %s (%s): %v\n", forwarder.GetName(), hostname, err)
			}
			continue
		}

		if blocked {
			// This forwarder resolves to blocked IPs - remove it immediately
			blockedCount++
			ipStrs := make([]string, len(resolvedIPs))
			for i, ip := range resolvedIPs {
				ipStrs[i] = ip.String()
			}
			reason := fmt.Sprintf("blocked by DNS: resolves to localhost/private IPs (%v)", ipStrs)
			fm.disableForwarder(forwarder.GetName(), reason)
			InfoLogFwd.Printf("Removed forwarder %s (%s): %s\n", forwarder.GetName(), hostname, reason)
		}
	}

	if blockedCount > 0 {
		InfoLogFwd.Printf("Removed %d forwarder(s) blocked by DNS (localhost/private IPs)\n", blockedCount)
	}
}

// extractMainDomain extracts the main domain from a hostname
// e.g., "tracker.example.com" -> "example.com", "sub.tracker.example.com" -> "example.com"
func extractMainDomain(hostname string) string {
	if hostname == "" {
		return ""
	}
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return hostname
	}
	// Return last two parts (e.g., "example.com")
	return strings.Join(parts[len(parts)-2:], ".")
}

// forwarderScore calculates a priority score for a forwarder
// Higher score = higher priority. Priority order:
// 1. UDP protocol (1000 points)
// 2. Has /announce path (100 points)
// 3. Port 6969 (50 points)
// 4. Has "tracker." subdomain (25 points)
// 5. Longer URL (1 point per character)
func forwarderScore(forwarder CoreCommon.Forward) int {
	score := 0
	u, err := url.Parse(forwarder.Uri)
	if err != nil {
		return 0
	}

	// 1. UDP protocol (highest priority)
	if u.Scheme == protocolUDP {
		score += 1000
	}

	// 2. Has /announce path
	if strings.HasSuffix(u.Path, "/announce") {
		score += 100
	}

	// 3. Port 6969
	port := u.Port()
	if port == "6969" {
		score += 50
	}

	// 4. Has "tracker." subdomain
	hostname := u.Hostname()
	if strings.HasPrefix(hostname, "tracker.") {
		score += 25
	}

	// 5. Longer URL (tiebreaker)
	score += len(forwarder.Uri)

	return score
}

// removeDuplicateForwardersByDomain removes duplicate forwarders based on main domain
// For forwarders with the same main domain, keeps the one with highest priority:
// UDP > /announce path > port 6969 > tracker. subdomain > longer URL
func (fm *ForwarderManager) removeDuplicateForwardersByDomain() {
	if len(fm.Forwarders) == 0 {
		return
	}

	// Group forwarders by main domain only (regardless of protocol or port)
	domainGroups := make(map[string][]CoreCommon.Forward)
	for _, forwarder := range fm.Forwarders {
		u, err := url.Parse(forwarder.Uri)
		if err != nil {
			// Skip invalid URIs (will be caught by validation later)
			continue
		}
		hostname := u.Hostname()
		if hostname == "" {
			continue
		}
		mainDomain := extractMainDomain(hostname)
		if mainDomain == "" {
			continue
		}
		domainGroups[mainDomain] = append(domainGroups[mainDomain], forwarder)
	}

	// For each domain group, keep only the highest scoring forwarder
	removedCount := 0
	toKeep := make(map[string]bool)       // Track forwarders to keep by URI (more reliable than name)
	removedNames := make(map[string]bool) // Track names of removed forwarders for stats cleanup

	for mainDomain, forwarders := range domainGroups {
		if len(forwarders) <= 1 {
			// Mark single forwarder to keep
			if len(forwarders) == 1 {
				toKeep[forwarders[0].Uri] = true
			}
			continue // No duplicates for this domain
		}

		// Find the forwarder with highest score
		bestForwarder := forwarders[0]
		bestScore := forwarderScore(bestForwarder)

		for i := 1; i < len(forwarders); i++ {
			score := forwarderScore(forwarders[i])
			if score > bestScore {
				bestForwarder = forwarders[i]
				bestScore = score
			}
		}

		// Mark the best forwarder to keep, mark others for removal
		toKeep[bestForwarder.Uri] = true
		for _, forwarder := range forwarders {
			if forwarder.Uri != bestForwarder.Uri {
				removedNames[forwarder.GetName()] = true
			}
		}
		removedCount += len(forwarders) - 1
		if fm.Config.Debug {
			DebugLogFwd.Printf("Domain %s: keeping forwarder %s (score: %d), removing %d duplicate(s)\n",
				mainDomain, bestForwarder.GetName(), bestScore, len(forwarders)-1)
		}
	}

	if removedCount == 0 {
		return
	}

	// Remove forwarders that are not marked to keep
	fm.forwardersMu.Lock()
	newForwarders := make([]CoreCommon.Forward, 0, len(fm.Forwarders))
	for _, forwarder := range fm.Forwarders {
		if toKeep[forwarder.Uri] {
			newForwarders = append(newForwarders, forwarder)
		}
	}
	fm.Forwarders = newForwarders
	fm.forwardersMu.Unlock()

	// Clean up stats for removed forwarders
	fm.statsMu.Lock()
	for name := range removedNames {
		delete(fm.stats, name)
	}
	fm.statsMu.Unlock()

	InfoLogFwd.Printf("Removed %d duplicate forwarder(s) by main domain\n", removedCount)
}

// validateAndRemoveInvalidForwarders checks all forwarders and removes those with invalid configuration
// Currently validates: UDP forwarders must have an explicit port in the URL
func (fm *ForwarderManager) validateAndRemoveInvalidForwarders() {
	if len(fm.Forwarders) == 0 {
		return
	}

	// Make a copy of forwarders to iterate over (since disableForwarder modifies the list)
	fm.forwardersMu.RLock()
	forwardersCopy := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwardersCopy, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	invalidCount := 0

	for _, forwarder := range forwardersCopy {
		// Extract hostname and port from URI
		u, err := url.Parse(forwarder.Uri)
		if err != nil {
			// If we can't parse the URI, skip validation (let other error handling deal with it)
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping validation for forwarder %s: invalid URI: %v\n", forwarder.GetName(), err)
			}
			continue
		}

		// Check if UDP forwarder has explicit port
		if u.Scheme == protocolUDP {
			hostname := u.Hostname()
			port := u.Port()
			if port == "" {
				// UDP forwarder without explicit port is invalid
				invalidCount++
				reason := fmt.Sprintf("invalid UDP URL: missing port (hostname: %s)", hostname)
				fm.disableForwarder(forwarder.GetName(), reason)
				InfoLogFwd.Printf("Removed forwarder %s (%s): %s\n", forwarder.GetName(), forwarder.Uri, reason)
			}
		}
	}

	if invalidCount > 0 {
		InfoLogFwd.Printf("Removed %d forwarder(s) with invalid configuration\n", invalidCount)
	}
}

// Queue management

// QueueEligibleAnnounces enqueues announce jobs for forwarders that are ready
func (fm *ForwarderManager) QueueEligibleAnnounces(infoHash common.InfoHash, request tracker.Request) {
	// Copy forwarders slice while holding lock to avoid race with disableForwarder
	fm.forwardersMu.RLock()
	forwarders := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwarders, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	if len(forwarders) == 0 {
		return
	}

	// Shuffle forwarders to distribute load evenly (prevents earlier forwarders
	// from being favored during throttling)
	rand.Shuffle(len(forwarders), func(i, j int) {
		forwarders[i], forwarders[j] = forwarders[j], forwarders[i]
	})

	limit := fm.maxForwardersPerAnnounce
	fill := fm.queueFillPct()
	if fill >= fm.queueThrottleThresh && fm.queueThrottleTopN > 0 {
		limit = fm.queueThrottleTopN
		if fm.shouldLogQueueThrottle(fill) {
			InfoLogFwd.Printf("Queue at %d%% (>= %d%%), throttling to %d forwarders\n",
				fill, fm.queueThrottleThresh, limit)
		}
	} else {
		fm.resetQueueThrottleLogState()
	}

	now := time.Now()
	queued := 0

	type eligibleForwarder struct {
		forwarder     CoreCommon.Forward
		forwarderName string
		order         int
		overdue       time.Duration
		hasEntry      bool
	}

	eligibleNew := make([]eligibleForwarder, 0)
	eligibleDue := make([]eligibleForwarder, 0)

	for idx, forwarder := range forwarders {
		forwarderName := forwarder.GetName()

		if fm.isSuspended(forwarderName) {
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping suspended forwarder %s for %x\n", forwarderName, infoHash)
			}
			continue
		}

		if fm.isJobPending(infoHash, forwarderName, request.PeerID) {
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping duplicate job for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
			}
			continue
		}

		hasEntry, nextAnnounce := fm.Storage.GetAnnounceState(infoHash, forwarderName)
		if hasEntry && nextAnnounce.After(now) {
			continue
		}

		candidate := eligibleForwarder{
			forwarder:     forwarder,
			forwarderName: forwarderName,
			order:         idx,
			hasEntry:      hasEntry,
		}

		if !hasEntry {
			eligibleNew = append(eligibleNew, candidate)
			continue
		}

		if !nextAnnounce.IsZero() {
			candidate.overdue = now.Sub(nextAnnounce)
			if candidate.overdue < 0 {
				candidate.overdue = 0
			}
		}
		eligibleDue = append(eligibleDue, candidate)
	}

	sort.SliceStable(eligibleDue, func(i, j int) bool {
		if eligibleDue[i].overdue == eligibleDue[j].overdue {
			return eligibleDue[i].order < eligibleDue[j].order
		}
		return eligibleDue[i].overdue > eligibleDue[j].overdue
	})

	enqueue := func(candidate eligibleForwarder) bool {
		if queued >= limit {
			return false
		}

		job := AnnounceJob{
			InfoHash:      infoHash,
			ForwarderName: candidate.forwarderName,
			PeerID:        request.PeerID,
			Forwarder:     candidate.forwarder,
			Request:       request,
		}

		fm.markJobPending(infoHash, candidate.forwarderName, request.PeerID)

		select {
		case fm.jobQueue <- job:
			queued++
			if fm.Config.Debug {
				DebugLogFwd.Printf("Queued announce for %x to %s (peer %x)\n", infoHash, candidate.forwarderName, request.PeerID)
			}
			return true
		default:
			fm.unmarkJobPending(infoHash, candidate.forwarderName, request.PeerID)
			atomic.AddUint64(&fm.droppedFullCount, 1)
			if fm.Prometheus != nil {
				fm.Prometheus.DroppedFull.Inc()
			}
			InfoLogFwd.Printf("Job queue full, stopping announce queueing for %x", infoHash)
			return false
		}
	}

	for _, candidate := range eligibleNew {
		if queued >= limit {
			break
		}
		if !enqueue(candidate) {
			return
		}
	}

	for _, candidate := range eligibleDue {
		if queued >= limit {
			break
		}
		if !enqueue(candidate) {
			return
		}
	}

	if fm.Config.Debug && queued > 0 {
		DebugLogFwd.Printf("Queued %d announces for %x\n", queued, infoHash)
	}
}

func (fm *ForwarderManager) shouldLogQueueThrottle(fillPct int) bool {
	fm.queueThrottleLogMu.Lock()
	defer fm.queueThrottleLogMu.Unlock()

	if fm.lastThrottleLogFillPct == fillPct {
		return false
	}
	fm.lastThrottleLogFillPct = fillPct
	return true
}

func (fm *ForwarderManager) resetQueueThrottleLogState() {
	fm.queueThrottleLogMu.Lock()
	fm.lastThrottleLogFillPct = -1
	fm.queueThrottleLogMu.Unlock()
}

func (fm *ForwarderManager) shouldRateLimitInitial() bool {
	if fm.queueRateLimitThresh <= 0 {
		return false
	}
	return fm.queueFillPct() >= fm.queueRateLimitThresh
}

// Scheduler

func (fm *ForwarderManager) schedulerRoutine() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-fm.stopChan:
			return
		case <-ticker.C:
			now := time.Now()
			fm.scheduledJobsMu.Lock()
			readyJobs := make([]AnnounceJob, 0)
			timesToRemove := make([]time.Time, 0)

			for scheduledTime, jobs := range fm.scheduledJobs {
				if !now.Before(scheduledTime) {
					readyJobs = append(readyJobs, jobs...)
					timesToRemove = append(timesToRemove, scheduledTime)
				}
			}

			for _, t := range timesToRemove {
				delete(fm.scheduledJobs, t)
			}
			fm.scheduledJobsMu.Unlock()

			for _, job := range readyJobs {
				fm.processScheduledReadyJob(job, now)
			}
		}
	}
}

func (fm *ForwarderManager) processScheduledReadyJob(job AnnounceJob, now time.Time) {
	if suspendedUntil, suspended := fm.getSuspendedUntil(job.ForwarderName, now); suspended {
		rescheduleAt := suspendedUntil
		if !rescheduleAt.After(now) {
			rescheduleAt = now.Add(1 * time.Second)
		}
		job.ScheduledTime = rescheduleAt

		fm.scheduledJobsMu.Lock()
		fm.scheduledJobs[rescheduleAt] = append(fm.scheduledJobs[rescheduleAt], job)
		fm.scheduledJobsMu.Unlock()

		InfoLogFwd.Printf("Forwarder %s suspended, rescheduled job for %x at %s\n",
			job.ForwarderName, job.InfoHash, rescheduleAt.Format(time.RFC3339))
		return
	}

	select {
	case fm.jobQueue <- job:
		if fm.Config.Debug {
			DebugLogFwd.Printf("Scheduled job ready, queued for %x to %s\n", job.InfoHash, job.ForwarderName)
		}
	default:
		fm.scheduledJobsMu.Lock()
		newTime := now.Add(10 * time.Second)
		job.ScheduledTime = newTime
		fm.scheduledJobs[newTime] = append(fm.scheduledJobs[newTime], job)
		fm.scheduledJobsMu.Unlock()
		InfoLogFwd.Printf("Queue full, rescheduled job for %x to %s\n", job.InfoHash, job.ForwarderName)
	}
}

func (fm *ForwarderManager) handleRetryError(job AnnounceJob, failureReason string, retryIn interface{}, trackerURL string) bool {
	if retryIn == nil {
		return false
	}

	if retryInStr, ok := retryIn.(string); ok && retryInStr == "never" {
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Tracker %s returned permanent failure (retry in: never): %s\n", trackerURL, failureReason)
		}
		fm.disableForwarder(job.ForwarderName, fmt.Sprintf("Permanent failure: %s", failureReason))
		return true
	}

	retryMinutes := 0
	switch v := retryIn.(type) {
	case int:
		retryMinutes = v
	case int64:
		retryMinutes = int(v)
	case string:
		if parsed, err := strconv.Atoi(v); err == nil {
			retryMinutes = parsed
		} else {
			if fm.Config.Debug {
				ErrorLogFwd.Printf("Invalid retry in value: %v\n", v)
			}
			return false
		}
	default:
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Unexpected retry in type: %T\n", v)
		}
		return false
	}

	if retryMinutes <= 0 {
		return false
	}

	if retryMinutes >= 10 {
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Tracker %s requested retry in %d minutes (>= 10), treating as completed\n", trackerURL, retryMinutes)
		}
		return true
	}

	scheduledTime := time.Now().Add(time.Duration(retryMinutes) * time.Minute)
	retryJob := AnnounceJob{
		InfoHash:      job.InfoHash,
		ForwarderName: job.ForwarderName,
		PeerID:        job.PeerID,
		Forwarder:     job.Forwarder,
		Request:       job.Request,
		ScheduledTime: scheduledTime,
	}

	fm.markJobPending(retryJob.InfoHash, retryJob.ForwarderName, retryJob.PeerID)
	if fm.scheduleJob(retryJob) {
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Scheduled retry for %x to %s in %d minutes\n", job.InfoHash, job.ForwarderName, retryMinutes)
		}
	} else {
		fm.unmarkJobPending(retryJob.InfoHash, retryJob.ForwarderName, retryJob.PeerID)
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Duplicate retry job already scheduled for %x to %s\n", job.InfoHash, job.ForwarderName)
		}
	}

	return true
}

func (fm *ForwarderManager) scheduleJob(job AnnounceJob) bool {
	fm.scheduledJobsMu.Lock()
	defer fm.scheduledJobsMu.Unlock()

	for _, jobs := range fm.scheduledJobs {
		for _, existingJob := range jobs {
			if existingJob.InfoHash == job.InfoHash && existingJob.ForwarderName == job.ForwarderName {
				return false
			}
		}
	}

	fm.scheduledJobs[job.ScheduledTime] = append(fm.scheduledJobs[job.ScheduledTime], job)
	return true
}
