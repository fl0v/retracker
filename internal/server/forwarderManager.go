// forwarderManager.go - Core forwarder orchestration and worker pool management.
// Manages job queue, worker scaling, rate limiting, throttling, and forwarder lifecycle.
// Handles job scheduling, deduplication, and forwarder enable/disable/suspend logic.
// Pending markers belong to individual jobs; peer sessions invalidate stale work.
// Stop cancels network work and joins workers before releasing queued jobs.
// Owns a shared HTTP client unless one is injected through a constructor option.
package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
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

var ErrForwarderAlreadyExists = errors.New("forwarder already exists")

// ForwarderManager manages forwarding announces to external trackers
type ForwarderManager struct {
	observability.Logging
	Config             *config.Config
	Storage            *ForwarderStorage
	MainStorage        *Storage
	Forwarders         []CoreCommon.Forward
	Workers            int
	jobQueue           chan AnnounceJob
	stopChan           <-chan struct{}
	tasks              *backgroundTasks
	startOnce          sync.Once
	stopOnce           sync.Once
	queueMu            sync.RWMutex
	Prometheus         *observability.Prometheus
	TempStorage        *TempStorage
	pendingJobs        map[string]*announceJobState
	pendingMu          sync.Mutex
	stats              map[string]*ForwarderStats
	statsMu            sync.RWMutex
	udpForwarder       *UDPForwarder
	httpClient         *http.Client
	closeIdleHTTP      func()
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
func NewForwarderManager(cfg *config.Config, storage *ForwarderStorage, mainStorage *Storage, prom *observability.Prometheus, tempStorage *TempStorage, options ...ForwarderOption) *ForwarderManager {
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
	if queueRateLimitThresh < 0 {
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

	var settings forwarderOptions
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}

	logging := observability.NewLogging(cfg.Debug, settings.logger)
	fm := &ForwarderManager{
		Logging:                  logging,
		Config:                   cfg,
		Storage:                  storage,
		MainStorage:              mainStorage,
		Forwarders:               cfg.Forwards,
		Workers:                  cfg.ForwarderWorkers,
		jobQueue:                 make(chan AnnounceJob, queueSize),
		tasks:                    newBackgroundTasks(),
		Prometheus:               prom,
		TempStorage:              tempStorage,
		pendingJobs:              make(map[string]*announceJobState),
		stats:                    make(map[string]*ForwarderStats),
		udpForwarder:             NewUDPForwarder(cfg.Debug, cfg.ForwardTimeout, cfg.ForwarderRetryAttempts, cfg.ForwarderRetryBaseMs, logging.Log()),
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
	if mainStorage != nil {
		mainStorage.requestsMu.Lock()
		mainStorage.forwarderStorage = storage
		mainStorage.requestsMu.Unlock()
	}
	if cfg.Age > 0 {
		storage.mu.Lock()
		storage.maxPeerAge = time.Duration(cfg.Age * float64(time.Minute))
		storage.mu.Unlock()
	}
	fm.stopChan = fm.tasks.ctx.Done()
	fm.httpClient = settings.httpClient
	if fm.httpClient == nil {
		fm.httpClient, fm.closeIdleHTTP = newForwarderHTTPClient(maxWorkers)
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
	fm.queueMu.RLock()
	defer fm.queueMu.RUnlock()
	fm.startOnce.Do(func() {
		if fm.isStopping() {
			return
		}
		for i := 0; i < fm.Workers; i++ {
			fm.tasks.run(fm.worker)
		}
		if fm.Prometheus != nil {
			fm.Prometheus.WorkerCount.Set(float64(fm.workerCount))
		}
		fm.tasks.run(fm.scaleWorkers)
		fm.tasks.run(fm.schedulerRoutine)

		if fm.Config.StatsInterval > 0 {
			fm.tasks.run(fm.statsRoutine)
		}
	})
}

func (fm *ForwarderManager) Stop() {
	fm.stopOnce.Do(func() {
		fm.queueMu.Lock()
		fm.tasks.stop()
		fm.queueMu.Unlock()
		if fm.udpForwarder != nil {
			fm.udpForwarder.Stop()
		}
		fm.tasks.wg.Wait()
		if fm.closeIdleHTTP != nil {
			fm.closeIdleHTTP()
		}
		fm.pendingMu.Lock()
		clear(fm.pendingJobs)
		fm.pendingMu.Unlock()
		fm.scheduledJobsMu.Lock()
		clear(fm.scheduledJobs)
		fm.scheduledJobsMu.Unlock()
		fm.workerCountMu.Lock()
		fm.workerCount = 0
		fm.workerCountMu.Unlock()
		if fm.Prometheus != nil {
			fm.Prometheus.WorkerCount.Set(0)
		}
		// The queue is never closed: concurrent producers cannot send to a closed channel.
		for {
			select {
			case <-fm.jobQueue:
			default:
				return
			}
		}
	})
}

func (fm *ForwarderManager) isStopping() bool {
	select {
	case <-fm.stopChan:
		return true
	default:
		return false
	}
}

func (fm *ForwarderManager) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(fm.Config.ForwardTimeout)*time.Second)
	if fm.tasks == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(fm.tasks.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// AddRuntimeForwarder adds a new forwarder in memory at runtime.
// The forwarder is not persisted and is lost on restart.
func (fm *ForwarderManager) AddRuntimeForwarder(forwarderURL string) (CoreCommon.Forward, error) {
	fm.queueMu.RLock()
	defer fm.queueMu.RUnlock()
	if fm.isStopping() {
		return CoreCommon.Forward{}, fmt.Errorf("forwarder manager is stopped")
	}
	uri := strings.TrimSpace(forwarderURL)
	if uri == "" {
		return CoreCommon.Forward{}, fmt.Errorf("missing forwarder url")
	}

	u, err := url.Parse(uri)
	if err != nil {
		return CoreCommon.Forward{}, fmt.Errorf("invalid forwarder url: %w", err)
	}
	if u.Scheme != protocolHTTP && u.Scheme != protocolHTTPS && u.Scheme != protocolUDP {
		return CoreCommon.Forward{}, fmt.Errorf("unsupported forwarder protocol: %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return CoreCommon.Forward{}, fmt.Errorf("invalid forwarder url: missing hostname")
	}
	if u.Scheme == protocolUDP && u.Port() == "" {
		return CoreCommon.Forward{}, fmt.Errorf("invalid UDP URL: missing port (hostname: %s)", u.Hostname())
	}

	forwarder := CoreCommon.Forward{Uri: uri}
	name := forwarder.GetName()

	fm.forwardersMu.RLock()
	for _, existing := range fm.Forwarders {
		if strings.EqualFold(existing.Uri, uri) || existing.GetName() == name {
			fm.forwardersMu.RUnlock()
			return CoreCommon.Forward{}, fmt.Errorf("%w: %s", ErrForwarderAlreadyExists, existing.GetName())
		}
	}
	fm.forwardersMu.RUnlock()

	fm.pendingMu.Lock()
	delete(fm.disabledForwarders, name)
	delete(fm.failCounts, name)
	fm.pendingMu.Unlock()

	fm.suspendedMu.Lock()
	delete(fm.suspendedForwarders, name)
	fm.suspendedMu.Unlock()

	fm.forwardersMu.Lock()
	for _, existing := range fm.Forwarders {
		if strings.EqualFold(existing.Uri, uri) || existing.GetName() == name {
			fm.forwardersMu.Unlock()
			return CoreCommon.Forward{}, fmt.Errorf("%w: %s", ErrForwarderAlreadyExists, existing.GetName())
		}
	}
	fm.Forwarders = append(fm.Forwarders, forwarder)
	fm.forwardersMu.Unlock()

	fm.statsMu.Lock()
	if _, ok := fm.stats[name]; !ok {
		fm.stats[name] = &ForwarderStats{SampleCount: 0}
	}
	fm.statsMu.Unlock()

	fm.Log().Info(fmt.Sprintf("Added runtime forwarder %s (%s)", name, uri))
	return forwarder, nil
}

// Job management

func (fm *ForwarderManager) jobKey(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) string {
	return fmt.Sprintf("%x:%s:%x", infoHash, forwarderName, peerID)
}

func (fm *ForwarderManager) isJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) bool {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	_, pending := fm.pendingJobs[fm.jobKey(infoHash, forwarderName, peerID)]
	return pending
}

func (fm *ForwarderManager) markJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	fm.pendingJobs[fm.jobKey(infoHash, forwarderName, peerID)] = &announceJobState{}
}

func (fm *ForwarderManager) unmarkJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	delete(fm.pendingJobs, fm.jobKey(infoHash, forwarderName, peerID))
}

func (fm *ForwarderManager) finishJob(job AnnounceJob) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	key := fm.jobKey(job.InfoHash, job.ForwarderName, job.PeerID)
	if fm.pendingJobs[key] == job.state {
		delete(fm.pendingJobs, key)
	}
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
				remaining := fm.workerCount
				fm.workerCountMu.Unlock()
				fm.Log().Info(fmt.Sprintf("Worker scaling down, remaining: %d", remaining))
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(remaining))
				}
				return
			}
			fm.workerCountMu.Unlock()
		case job, ok := <-fm.jobQueue:
			if !ok {
				return
			}
			if job.canceled() {
				fm.finishJob(job)
				continue
			}
			if !job.ScheduledTime.IsZero() && time.Now().Before(job.ScheduledTime) {
				if !fm.scheduleJob(job) {
					fm.finishJob(job)
				}
				continue
			}
			fm.executeAnnounce(job)
			fm.finishJob(job)
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
				fm.tasks.run(fm.worker)
				fm.Log().Info(fmt.Sprintf("Scaling workers up to %d (queue %d%%)", newCount, fillPct))
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(newCount))
				}
			case fillPct < 40 && currentCount > baseWorkers:
				select {
				case fm.scaleDownChan <- struct{}{}:
					fm.Log().Info(fmt.Sprintf("Signaling worker to scale down (queue %d%%, workers: %d)", fillPct, currentCount))
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

func (fm *ForwarderManager) getFailureCount(forwarderName string) int {
	fm.pendingMu.Lock()
	count := fm.failCounts[forwarderName]
	fm.pendingMu.Unlock()
	return count
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

	fm.Log().Info(fmt.Sprintf("Disabled forwarder %s: %s (failure count: %d)", forwarderName, reason, failureCount))
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
	fm.Log().Info(fmt.Sprintf("Suspended forwarder %s for %v (reason: %s)", forwarderName, duration, truncatedReason))
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
				fm.Log().Debug(fmt.Sprintf("Skipping DNS check for forwarder %s: invalid URI: %v", forwarder.GetName(), err))
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
				fm.Log().Debug(fmt.Sprintf("DNS lookup failed for forwarder %s (%s): %v", forwarder.GetName(), hostname, err))
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
			fm.Log().Info(fmt.Sprintf("Removed forwarder %s (%s): %s", forwarder.GetName(), hostname, reason))
		}
	}

	if blockedCount > 0 {
		fm.Log().Info(fmt.Sprintf("Removed %d forwarder(s) blocked by DNS (localhost/private IPs)", blockedCount))
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
			fm.Log().Debug(fmt.Sprintf("Domain %s: keeping forwarder %s (score: %d), removing %d duplicate(s)",
				mainDomain, bestForwarder.GetName(), bestScore, len(forwarders)-1))
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

	fm.Log().Info(fmt.Sprintf("Removed %d duplicate forwarder(s) by main domain", removedCount))
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
				fm.Log().Debug(fmt.Sprintf("Skipping validation for forwarder %s: invalid URI: %v", forwarder.GetName(), err))
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
				fm.Log().Info(fmt.Sprintf("Removed forwarder %s (%s): %s", forwarder.GetName(), forwarder.Uri, reason))
			}
		}
	}

	if invalidCount > 0 {
		fm.Log().Info(fmt.Sprintf("Removed %d forwarder(s) with invalid configuration", invalidCount))
	}
}

// Queue management

// QueueEligibleAnnounces enqueues announce jobs for forwarders that are ready
func (fm *ForwarderManager) QueueEligibleAnnounces(infoHash common.InfoHash, request tracker.Request) {
	var peerContext context.Context
	if fm.MainStorage != nil {
		peerContext = fm.MainStorage.peerContext(infoHash, request.PeerID)
		if peerContext == nil {
			return
		}
	}
	fm.queueEligibleAnnounces(infoHash, request, peerContext)
}

func (fm *ForwarderManager) queueEligibleAnnounces(infoHash common.InfoHash, request tracker.Request, peerContext context.Context) {
	fm.queueMu.RLock()
	defer fm.queueMu.RUnlock()
	if fm.isStopping() {
		return
	}
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
			fm.Log().Info(fmt.Sprintf("Queue at %d%% (>= %d%%), throttling to %d forwarders",
				fill, fm.queueThrottleThresh, limit))
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
				fm.Log().Debug(fmt.Sprintf("Skipping suspended forwarder %s for %x", forwarderName, infoHash))
			}
			continue
		}

		if fm.isJobPending(infoHash, forwarderName, request.PeerID) {
			if fm.Config.Debug {
				fm.Log().Debug(fmt.Sprintf("Skipping duplicate job for %x to %s (peer %x)", infoHash, forwarderName, request.PeerID))
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
			state:         &announceJobState{peerContext: peerContext},
		}

		fm.pendingMu.Lock()
		key := fm.jobKey(infoHash, candidate.forwarderName, request.PeerID)
		if _, pending := fm.pendingJobs[key]; pending || job.canceled() {
			fm.pendingMu.Unlock()
			return true
		}
		fm.pendingJobs[key] = job.state
		fm.pendingMu.Unlock()

		select {
		case fm.jobQueue <- job:
			queued++
			if fm.Config.Debug {
				fm.Log().Debug(fmt.Sprintf("Queued announce for %x to %s (peer %x)", infoHash, candidate.forwarderName, request.PeerID))
			}
			return true
		default:
			fm.finishJob(job)
			atomic.AddUint64(&fm.droppedFullCount, 1)
			if fm.Prometheus != nil {
				fm.Prometheus.DroppedFull.Inc()
			}
			fm.Log().Info(fmt.Sprintf("Job queue full, stopping announce queueing for %x", infoHash))
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
		fm.Log().Debug(fmt.Sprintf("Queued %d announces for %x", queued, infoHash))
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
			fm.Storage.Prune(now)
			fm.scheduledJobsMu.Lock()
			readyJobs := make([]AnnounceJob, 0)
			timesToRemove := make([]time.Time, 0)

			var canceledJobs []AnnounceJob
			for scheduledTime, jobs := range fm.scheduledJobs {
				kept := jobs[:0]
				for _, job := range jobs {
					switch {
					case job.canceled():
						canceledJobs = append(canceledJobs, job)
					case !now.Before(scheduledTime):
						readyJobs = append(readyJobs, job)
					default:
						kept = append(kept, job)
					}
				}
				clear(jobs[len(kept):])
				if len(kept) == 0 {
					timesToRemove = append(timesToRemove, scheduledTime)
				} else {
					fm.scheduledJobs[scheduledTime] = kept
				}
			}

			for _, t := range timesToRemove {
				delete(fm.scheduledJobs, t)
			}
			fm.scheduledJobsMu.Unlock()

			for _, job := range canceledJobs {
				fm.finishJob(job)
			}
			for _, job := range readyJobs {
				fm.processScheduledReadyJob(job, now)
			}
		}
	}
}

func (fm *ForwarderManager) processScheduledReadyJob(job AnnounceJob, now time.Time) {
	fm.queueMu.RLock()
	defer fm.queueMu.RUnlock()
	if fm.isStopping() {
		fm.finishJob(job)
		return
	}
	if job.canceled() {
		fm.finishJob(job)
		return
	}
	if suspendedUntil, suspended := fm.getSuspendedUntil(job.ForwarderName, now); suspended {
		rescheduleAt := suspendedUntil
		if !rescheduleAt.After(now) {
			rescheduleAt = now.Add(1 * time.Second)
		}
		job.ScheduledTime = rescheduleAt

		if !fm.scheduleJob(job) {
			fm.finishJob(job)
			return
		}

		fm.Log().Info(fmt.Sprintf("Forwarder %s suspended, rescheduled job for %x at %s",
			job.ForwarderName, job.InfoHash, rescheduleAt.Format(time.RFC3339)))
		return
	}

	select {
	case fm.jobQueue <- job:
		if fm.Config.Debug {
			fm.Log().Debug(fmt.Sprintf("Scheduled job ready, queued for %x to %s", job.InfoHash, job.ForwarderName))
		}
	default:
		newTime := now.Add(10 * time.Second)
		job.ScheduledTime = newTime
		if !fm.scheduleJob(job) {
			fm.finishJob(job)
			return
		}
		fm.Log().Info(fmt.Sprintf("Queue full, rescheduled job for %x to %s", job.InfoHash, job.ForwarderName))
	}
}

func (fm *ForwarderManager) handleRetryError(job AnnounceJob, failureReason string, retryIn interface{}, trackerURL string) bool {
	if job.canceled() || fm.isStopping() {
		return true
	}
	if retryIn == nil {
		return false
	}

	if retryInStr, ok := retryIn.(string); ok && retryInStr == "never" {
		if fm.Config.Debug {
			fm.Log().Error(fmt.Sprintf("Tracker %s returned permanent failure (retry in: never): %s", trackerURL, failureReason))
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
				fm.Log().Error(fmt.Sprintf("Invalid retry in value: %v", v))
			}
			return false
		}
	default:
		if fm.Config.Debug {
			fm.Log().Error(fmt.Sprintf("Unexpected retry in type: %T", v))
		}
		return false
	}

	if retryMinutes <= 0 {
		return false
	}

	if int64(retryMinutes) > (1<<63-1)/int64(time.Minute) {
		return false
	}
	scheduledTime := time.Now().Add(time.Duration(retryMinutes) * time.Minute)
	if !fm.updatePeerCache(job, func() { scheduledTime = fm.Storage.DeferAnnounce(job.InfoHash, job.ForwarderName, scheduledTime) }) {
		return true
	}
	retryJob := AnnounceJob{
		InfoHash:      job.InfoHash,
		ForwarderName: job.ForwarderName,
		PeerID:        job.PeerID,
		Forwarder:     job.Forwarder,
		Request:       job.Request,
		ScheduledTime: scheduledTime,
		state:         &announceJobState{},
	}
	if job.state != nil {
		retryJob.state.peerContext = job.state.peerContext
	}

	fm.pendingMu.Lock()
	if retryJob.canceled() {
		fm.pendingMu.Unlock()
		return true
	}
	key := fm.jobKey(retryJob.InfoHash, retryJob.ForwarderName, retryJob.PeerID)
	previous := fm.pendingJobs[key]
	fm.pendingJobs[key] = retryJob.state
	scheduled := fm.scheduleJob(retryJob)
	if !scheduled {
		if previous != nil {
			fm.pendingJobs[key] = previous
		} else {
			delete(fm.pendingJobs, key)
		}
	}
	fm.pendingMu.Unlock()
	if scheduled {
		if fm.Config.Debug {
			fm.Log().Error(fmt.Sprintf("Scheduled retry for %x to %s in %d minutes", job.InfoHash, job.ForwarderName, retryMinutes))
		}
	} else {
		if fm.Config.Debug {
			fm.Log().Error(fmt.Sprintf("Duplicate retry job already scheduled for %x to %s", job.InfoHash, job.ForwarderName))
		}
	}

	return true
}

func (fm *ForwarderManager) scheduleJob(job AnnounceJob) bool {
	fm.scheduledJobsMu.Lock()
	defer fm.scheduledJobsMu.Unlock()
	if job.canceled() || fm.isStopping() {
		return false
	}

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
