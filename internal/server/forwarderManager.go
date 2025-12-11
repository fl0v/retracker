package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const (
	EventStopped   = "stopped"
	EventCompleted = "completed"
)

var (
	DebugLogFwd = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogFwd = log.New(os.Stderr, `error#`, log.Lshortfile)
)

// ForwarderStats tracks statistics per forwarder (aggregated across all hashes).
// Note: This is different from ForwarderStorage which tracks intervals per-hash, per-forwarder.
// LastInterval here is the most recent interval reported by this forwarder for ANY hash.
type ForwarderStats struct {
	AvgResponseTime time.Duration // EMA of response times (across all hashes for this forwarder)
	LastInterval    int           // Last reported interval from this forwarder (most recent across all hashes)
	SampleCount     int           // Total samples seen (across all hashes for this forwarder)
	mu              sync.RWMutex
}

const (
	emaAlpha             = 0.02 // Smoothing factor equivalent to ~100 sample window (unused now, kept for reference)
	emaAlphaResponseTime = 0.15 // Higher weight for response time EMA - gives 15% weight to latest sample for faster adaptation
)

type DisabledForwarder struct {
	Reason       string // Truncated to maxReasonLength for memory efficiency
	DisabledAt   int64  // Unix timestamp (more compact than time.Time)
	FailureCount int
}

const maxReasonLength = 200 // Limit reason string length to save memory

type ForwarderManager struct {
	Config             *config.Config
	Storage            *ForwarderStorage
	MainStorage        *Storage // Reference to main storage for client statistics
	Forwarders         []CoreCommon.Forward
	Workers            int
	jobQueue           chan AnnounceJob
	stopChan           chan struct{}
	Prometheus         *observability.Prometheus
	TempStorage        *TempStorage
	requestCache       map[string]tracker.Request // cache of last request per info_hash for re-announcing
	cacheMu            sync.RWMutex
	pendingJobs        map[string]bool // track pending jobs: "infoHash:forwarderName" -> true
	pendingMu          sync.Mutex
	stats              map[string]*ForwarderStats // forwarderName -> stats
	statsMu            sync.RWMutex
	udpForwarder       *UDPForwarder // UDP forwarder client for UDP trackers
	failCounts         map[string]int
	disabledForwarders map[string]*DisabledForwarder
	forwardersMu       sync.RWMutex // protects Forwarders slice

	// Queue/worker control
	maxWorkers            int
	queueScaleThreshold   int
	queueRateLimitThresh  int
	queueThrottleThresh   int
	queueThrottleTopN     int
	workerCount           int
	workerCountMu         sync.Mutex    // protects workerCount during scaling
	scaleDownChan         chan struct{} // channel to signal workers to scale down
	rateLimiter           *tokenBucket
	droppedFullCount      uint64
	rateLimitedCount      uint64
	throttledForwardCount uint64

	suspendedMu         sync.Mutex
	suspendedForwarders map[string]time.Time

	// Scheduled jobs
	scheduledJobs   map[time.Time][]AnnounceJob // scheduled jobs by execution time
	scheduledJobsMu sync.RWMutex
}

type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	rate     int // tokens per second
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
	ratePerSec := cfg.RateLimitInitialPerSec
	if ratePerSec <= 0 {
		ratePerSec = 100
	}
	rateBurst := cfg.RateLimitInitialBurst
	if rateBurst <= 0 {
		rateBurst = 200
	}

	fm := &ForwarderManager{
		Config:               cfg,
		Storage:              storage,
		MainStorage:          mainStorage,
		Forwarders:           cfg.Forwards,
		Workers:              cfg.ForwarderWorkers,
		jobQueue:             make(chan AnnounceJob, queueSize),
		stopChan:             make(chan struct{}),
		Prometheus:           prom,
		TempStorage:          tempStorage,
		requestCache:         make(map[string]tracker.Request),
		pendingJobs:          make(map[string]bool),
		stats:                make(map[string]*ForwarderStats),
		udpForwarder:         NewUDPForwarder(cfg.Debug, cfg.ForwardTimeout, cfg.ForwarderRetryAttempts, cfg.ForwarderRetryBaseMs),
		failCounts:           make(map[string]int),
		disabledForwarders:   make(map[string]*DisabledForwarder),
		maxWorkers:           maxWorkers,
		queueScaleThreshold:  queueScaleThresh,
		queueRateLimitThresh: queueRateLimitThresh,
		queueThrottleThresh:  queueThrottleThresh,
		queueThrottleTopN:    queueThrottleTopN,
		workerCount:          cfg.ForwarderWorkers,
		rateLimiter: &tokenBucket{
			rate:   ratePerSec,
			burst:  rateBurst,
			tokens: rateBurst,
		},
		suspendedForwarders: make(map[string]time.Time),
		scaleDownChan:       make(chan struct{}, 100), // buffered channel for scale-down signals
		scheduledJobs:       make(map[time.Time][]AnnounceJob),
	}
	// Initialize stats for each forwarder
	for _, forwarder := range cfg.Forwards {
		fm.stats[forwarder.GetName()] = &ForwarderStats{
			SampleCount: 0,
		}
	}
	return fm
}

func (fm *ForwarderManager) Start() {
	// Start worker pool
	for i := 0; i < fm.Workers; i++ {
		go fm.worker()
	}
	if fm.Prometheus != nil {
		fm.Prometheus.WorkerCount.Set(float64(fm.workerCount))
	}
	go fm.scaleWorkers()
	go fm.schedulerRoutine()

	// Start statistics routine
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

// jobKey generates a unique key for a job (infoHash:forwarderName:peerID)
func (fm *ForwarderManager) jobKey(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) string {
	return fmt.Sprintf("%x:%s:%x", infoHash, forwarderName, peerID)
}

// isJobPending checks if a job is already in the queue
func (fm *ForwarderManager) isJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) bool {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	key := fm.jobKey(infoHash, forwarderName, peerID)
	return fm.pendingJobs[key]
}

// markJobPending marks a job as pending
func (fm *ForwarderManager) markJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	key := fm.jobKey(infoHash, forwarderName, peerID)
	fm.pendingJobs[key] = true
}

// unmarkJobPending removes job from pending list
func (fm *ForwarderManager) unmarkJobPending(infoHash common.InfoHash, forwarderName string, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()
	key := fm.jobKey(infoHash, forwarderName, peerID)
	delete(fm.pendingJobs, key)
}

func (fm *ForwarderManager) worker() {
	baseWorkers := fm.Workers
	for {
		select {
		case <-fm.stopChan:
			return
		case <-fm.scaleDownChan:
			// Check if this worker is beyond base workers and should exit
			fm.workerCountMu.Lock()
			currentCount := fm.workerCount
			shouldExit := currentCount > baseWorkers
			if shouldExit {
				fm.workerCount--
				fm.workerCountMu.Unlock()
				if fm.Config.Debug {
					DebugLogFwd.Printf("Worker scaling down, remaining: %d\n", fm.workerCount)
				}
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(fm.workerCount))
				}
				return
			}
			fm.workerCountMu.Unlock()
			// Not an extra worker, continue
		case job, ok := <-fm.jobQueue:
			if !ok {
				return
			}
			// Check if job is scheduled for future execution
			if !job.ScheduledTime.IsZero() && time.Now().Before(job.ScheduledTime) {
				// Job not ready yet, reschedule it
				if !fm.scheduleJob(job) {
					// Duplicate job already scheduled, unmark as pending to avoid losing it
					fm.unmarkJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
					if fm.Config.Debug {
						DebugLogFwd.Printf("Rescheduled job for %x to %s already exists, unmarking as pending\n",
							job.InfoHash, job.ForwarderName)
					}
				}
				continue
			}
			fm.executeAnnounce(job)
			// Unmark job as pending when done
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

			shouldScaleUp := fillPct >= fm.queueScaleThreshold && currentCount < fm.maxWorkers
			shouldScaleDown := fillPct < 40 && currentCount > baseWorkers

			switch {
			case shouldScaleUp:
				// Scale up: simple cool-down: only scale once per second tick
				fm.workerCount++
				newCount := fm.workerCount
				fm.workerCountMu.Unlock()
				go fm.worker()
				if fm.Config.Debug {
					DebugLogFwd.Printf("Scaling workers up to %d (queue %d%%)\n", newCount, fillPct)
				}
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(newCount))
				}
			case shouldScaleDown:
				// Scale down: queue below 40% and we have more than base workers
				// Signal one worker to exit
				select {
				case fm.scaleDownChan <- struct{}{}:
					if fm.Config.Debug {
						DebugLogFwd.Printf("Signaling worker to scale down (queue %d%%, workers: %d)\n", fillPct, currentCount)
					}
				default:
					// Channel full, skip this tick
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
	capacity := cap(fm.jobQueue)
	if capacity == 0 {
		return false
	}
	return len(fm.jobQueue) >= capacity
}

func (fm *ForwarderManager) updatePrometheusQueue(stats *observability.Stats) {
	fm.Prometheus.QueueDepth.Set(float64(stats.QueueDepth))
	fm.Prometheus.QueueCapacity.Set(float64(stats.QueueCapacity))
	fm.Prometheus.QueueFillPct.Set(float64(stats.QueueFillPct))
	fm.Prometheus.WorkerCount.Set(float64(stats.ActiveWorkers))
	// counters are cumulative; gauges already set
}

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
	// Get failure count before deleting
	failureCount := fm.failCounts[forwarderName]
	delete(fm.failCounts, forwarderName)
	fm.pendingMu.Unlock()

	// Find the forwarder in Forwarders slice
	fm.forwardersMu.Lock()
	var forwarderIndex int = -1
	for i, f := range fm.Forwarders {
		if f.GetName() == forwarderName {
			forwarderIndex = i
			break
		}
	}

	// Store in disabledForwarders map and remove from Forwarders slice
	if forwarderIndex >= 0 {
		// Truncate reason to save memory
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

		// Remove from Forwarders slice
		fm.Forwarders = append(fm.Forwarders[:forwarderIndex], fm.Forwarders[forwarderIndex+1:]...)
	}
	fm.forwardersMu.Unlock()

	// Remove from stats map
	fm.statsMu.Lock()
	delete(fm.stats, forwarderName)
	fm.statsMu.Unlock()

	// Drop from storage
	fm.Storage.mu.Lock()
	for infoHash := range fm.Storage.Entries {
		delete(fm.Storage.Entries[infoHash], forwarderName)
		if len(fm.Storage.Entries[infoHash]) == 0 {
			delete(fm.Storage.Entries, infoHash)
		}
	}
	fm.Storage.mu.Unlock()

	if fm.Config.Debug {
		DebugLogFwd.Printf("Disabled forwarder %s: %s (failure count: %d)", forwarderName, reason, failureCount)
	}
}

func (fm *ForwarderManager) isDisabled(forwarderName string) bool {
	fm.pendingMu.Lock()
	_, disabled := fm.disabledForwarders[forwarderName]
	fm.pendingMu.Unlock()
	return disabled
}

func (fm *ForwarderManager) isSuspended(forwarderName string) bool {
	fm.suspendedMu.Lock()
	defer fm.suspendedMu.Unlock()
	until, ok := fm.suspendedForwarders[forwarderName]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(fm.suspendedForwarders, forwarderName)
		return false
	}
	return true
}

func (fm *ForwarderManager) suspendForwarder(forwarderName string, duration time.Duration) {
	fm.suspendedMu.Lock()
	fm.suspendedForwarders[forwarderName] = time.Now().Add(duration)
	fm.suspendedMu.Unlock()
	if fm.Config.Debug {
		DebugLogFwd.Printf("Suspended forwarder %s for %v\n", forwarderName, duration)
	}
}

func shouldSuspendForwarder(statusCode int, err error) bool {
	// Suspend on HTTP 429; can extend with other overload signals if needed
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	// Placeholder for future error-based signals
	_ = err
	return false
}

func (fm *ForwarderManager) executeAnnounce(job AnnounceJob) {
	forward := job.Forwarder

	if fm.isDisabled(forward.GetName()) {
		if fm.Config.Debug {
			DebugLogFwd.Printf("Forwarder %s is disabled; dropping job for %x", forward.GetName(), job.InfoHash)
		}
		return
	}
	if fm.isSuspended(forward.GetName()) {
		if fm.Config.Debug {
			DebugLogFwd.Printf("Forwarder %s is suspended; dropping job for %x", forward.GetName(), job.InfoHash)
		}
		return
	}

	// Detect protocol and route to appropriate handler
	protocol := forward.GetProtocol()
	if protocol == "udp" {
		fm.executeUDPAnnounce(job)
	} else {
		fm.executeHTTPAnnounce(job)
	}
}

// executeUDPAnnounce handles UDP tracker announces
func (fm *ForwarderManager) executeUDPAnnounce(job AnnounceJob) {
	startTime := time.Now()
	forward := job.Forwarder
	request := job.Request
	hash := fmt.Sprintf("%x", job.InfoHash)
	forwardName := forward.GetName()
	trackerURL := forward.Uri

	// Normal mode: log hash and tracker URL
	fmt.Printf("UDP announce %s to %s\n", hash, trackerURL)
	if fm.Config.Debug {
		if forward.Ip != `` {
			DebugLogFwd.Printf("  Using IP: %s\n", forward.Ip)
		}
	}

	// Use UDP forwarder
	bitResponse, respBytes, err := fm.udpForwarder.Announce(forward, request)
	duration := time.Since(startTime)

	if err != nil {
		ErrorLogFwd.Printf("UDP annouce error %s to %s: %s\n", hash, trackerURL, err.Error())
		if fm.Config.Debug {
			ErrorLogFwd.Printf("  Duration: %v\n", duration)
		}
		if fm.Prometheus != nil {
			fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `error`}).Inc()
		}
		// Check if error is non-retryable (invalid hostname, port not open, etc.)
		if isNonRetryableError(err) {
			fm.disableForwarder(forwardName, "UDP error (non-retryable): "+err.Error())
		} else {
			// For retryable errors (timeout, parse failures, etc.), register failure
			// This will disable only after consecutive failures reach threshold
			fm.registerFailure(forwardName)
		}
		// Mark as attempted with default interval to avoid immediate retry
		fm.Storage.UpdatePeers(job.InfoHash, forwardName, []common.Peer{}, 60)
		return
	}

	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `200`}).Inc()
	}

	// Update storage with peers and interval from response
	fm.Storage.UpdatePeers(job.InfoHash, forwardName, bitResponse.Peers, bitResponse.Interval)

	// Record statistics
	fm.recordStats(forwardName, duration, bitResponse.Interval)

	secs := duration.Seconds()
	fmt.Printf("UDP response from %s (%d bytes, %.3fs, interval=%d, peers=%d)\n", trackerURL, respBytes, secs, bitResponse.Interval, len(bitResponse.Peers))
}

// executeHTTPAnnounce handles HTTP/HTTPS tracker announces (original logic)
func (fm *ForwarderManager) executeHTTPAnnounce(job AnnounceJob) {
	startTime := time.Now()
	forward := job.Forwarder
	request := job.Request
	hash := fmt.Sprintf("%x", job.InfoHash)
	forwardName := forward.GetName()
	trackerURL := forward.Uri

	uri := fmt.Sprintf("%s?info_hash=%s&peer_id=%s&port=%d&uploaded=%d&downloaded=%d&left=%d",
		forward.Uri, url.QueryEscape(string(request.InfoHash)),
		url.QueryEscape(string(request.PeerID)), request.Port, request.Uploaded, request.Downloaded, request.Left)
	if forward.Ip != `` {
		uri = fmt.Sprintf("%s&ip=%s&ipv4=%s", uri, forward.Ip, forward.Ip)
	}

	fmt.Printf("HTTP announce %s to %s\n", hash, trackerURL)
	if fm.Config.Debug {
		DebugLogFwd.Printf("  Request URI: %s\n", uri)
		if forward.Ip != `` {
			DebugLogFwd.Printf("  Using IP: %s\n", forward.Ip)
		}
		if forward.Host != `` {
			DebugLogFwd.Printf("  Host header: %s\n", forward.Host)
		}
	}

	attempts := fm.Config.ForwarderRetryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := time.Duration(fm.Config.ForwarderRetryBaseMs) * time.Millisecond
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	var lastErr error
	for retry := 0; retry < attempts; retry++ {
		if retry > 0 {
			time.Sleep(backoff * time.Duration(1<<retry))
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(fm.Config.ForwardTimeout))
		rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		if forward.Host != `` {
			rqst.Host = forward.Host
		}

		client := http.Client{}
		response, err := client.Do(rqst)
		duration := time.Since(startTime)
		if err != nil {
			lastErr = err
			cancel()
			// Check if error is non-retryable (invalid hostname, port not open, etc.)
			if isNonRetryableError(err) {
				if fm.Config.Debug {
					ErrorLogFwd.Printf("HTTP request error (non-retryable): %v\n", err)
				}
				fm.disableForwarder(forwardName, "HTTP request error: "+err.Error())
				return
			}
			// For timeout or other retryable errors, continue retry loop
			if fm.Config.Debug && !isTimeoutErr(err) {
				ErrorLogFwd.Printf("HTTP request error (retryable): %v\n", err)
			}
			continue
		}

		// From here on, ensure body is closed
		body := response.Body
		defer body.Close()

		if fm.Prometheus != nil {
			fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: fmt.Sprintf("%d", response.StatusCode)}).Inc()
		}

		if response.StatusCode != http.StatusOK {
			// Read and discard body before closing (required to reuse connection)
			_, _ = io.ReadAll(body)
			cancel()
			// Special handling for 429 Too Many Requests -> suspend forwarder
			if shouldSuspendForwarder(response.StatusCode, nil) {
				suspendFor := time.Duration(fm.Config.ForwarderSuspendSeconds) * time.Second
				if suspendFor <= 0 {
					suspendFor = 300 * time.Second
				}
				fm.suspendForwarder(forwardName, suspendFor)
				if fm.Config.Debug {
					ErrorLogFwd.Printf("HTTP %d from %s; suspending for %v\n", response.StatusCode, trackerURL, suspendFor)
				}
				return
			}
			// Check if status code indicates tracker rejection (non-retryable)
			if isTrackerRejection(response.StatusCode) {
				if fm.Config.Debug {
					ErrorLogFwd.Printf("HTTP %d %s (tracker rejection)\n", response.StatusCode, response.Status)
				}
				fm.disableForwarder(forwardName, fmt.Sprintf("HTTP status %d (tracker rejection)", response.StatusCode))
				return
			}
			// Other status codes (500, 502, 503, etc.) are retryable
			lastErr = fmt.Errorf("HTTP status %d: %s", response.StatusCode, response.Status)
			if fm.Config.Debug {
				ErrorLogFwd.Printf("HTTP %d %s (retryable, will retry)\n", response.StatusCode, response.Status)
			}
			continue
		}

		payload, err := io.ReadAll(body)
		cancel()
		if err != nil {
			readErr := fmt.Errorf("failed to read response: %w", err)
			lastErr = readErr
			// Check if error is non-retryable
			if isNonRetryableError(err) {
				if fm.Config.Debug {
					ErrorLogFwd.Printf("Failed to read response (non-retryable): %v\n", err)
				}
				fm.disableForwarder(forwardName, "failed to read response: "+err.Error())
				return
			}
			// For timeout or other retryable errors, continue retry loop
			if fm.Config.Debug && !isTimeoutErr(err) {
				ErrorLogFwd.Printf("Failed to read response (retryable): %v\n", err)
			}
			continue
		}

		tempFilename := ``
		if fm.Config.Debug {
			tempFilename = fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
		}

		bitResponse, err := Response.Load(payload)
		if err != nil {
			parseErr := fmt.Errorf("failed to parse response: %w", err)
			lastErr = parseErr
			// Parse errors are retryable (could be transient corruption)
			if fm.Config.Debug {
				ErrorLogFwd.Printf("Failed to parse response (retryable): %v\n", err)
				isText := true
				if len(payload) > 0 {
					printableCount := 0
					for _, b := range payload {
						if b >= 32 && b < 127 || b == '\n' || b == '\r' || b == '\t' {
							printableCount++
						}
					}
					isText = float64(printableCount)/float64(len(payload)) >= 0.8
				}
				if isText {
					payloadStr := string(payload)
					if len(payloadStr) > 500 {
						payloadStr = payloadStr[:500] + "... (truncated)"
					}
					ErrorLogFwd.Printf("  Raw response data: %s\n", payloadStr)
				} else {
					ErrorLogFwd.Printf("  Response is binary (%d bytes)\n", len(payload))
				}
				if tempFilename == `` {
					tempFilename = fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
				}
				if tempFilename != `` {
					ErrorLogFwd.Printf("  Response saved to: %s\n", tempFilename)
				}
			}
			// Continue retry loop for parse errors
			continue
		}

		// Check for failure with "retry in" (BEP 31: Failure Retry Extension)
		if bitResponse.FailureReason != "" {
			if handled := fm.handleRetryError(job, bitResponse.FailureReason, bitResponse.RetryIn, trackerURL); handled {
				return
			}
			// If not handled by retry logic, treat as regular failure
			lastErr = fmt.Errorf("tracker failure: %s", bitResponse.FailureReason)
			if fm.Config.Debug {
				ErrorLogFwd.Printf("Tracker failure response: %s\n", bitResponse.FailureReason)
			}
			continue
		}

		// Success
		fm.Storage.UpdatePeers(job.InfoHash, forwardName, bitResponse.Peers, bitResponse.Interval)
		fm.resetFailure(forwardName)
		fm.recordStats(forwardName, duration, bitResponse.Interval)
		secs := duration.Seconds()
		fmt.Printf("HTTP response from %s (%d bytes, %.3fs, interval=%d, peers=%d)\n", trackerURL, len(payload), secs, bitResponse.Interval, len(bitResponse.Peers))
		return
	}

	// All retries failed
	duration := time.Since(startTime)
	ErrorLogFwd.Printf("HTTP response error %s to %s: %v (%.3fs)\n", hash, trackerURL, lastErr.Error(), duration.Seconds())
	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `error`}).Inc()
	}
	fm.Storage.UpdatePeers(job.InfoHash, forwardName, []common.Peer{}, 60)

	// Check if error is non-retryable - disable immediately
	if lastErr != nil && isNonRetryableError(lastErr) {
		fm.disableForwarder(forwardName, "all retries failed (non-retryable): "+lastErr.Error())
	} else {
		// For retryable errors (timeout, parse failures, etc.), register failure
		// This will disable only after consecutive failures reach threshold
		fm.registerFailure(forwardName)
	}
}

func (fm *ForwarderManager) CacheRequest(infoHash common.InfoHash, request tracker.Request) {
	fm.cacheMu.Lock()
	defer fm.cacheMu.Unlock()
	fm.requestCache[string(infoHash)] = request
}

func (fm *ForwarderManager) selectForwardersByQueue(all []CoreCommon.Forward) []CoreCommon.Forward {
	fill := fm.queueFillPct()
	if fill < fm.queueThrottleThresh || fm.queueThrottleTopN <= 0 {
		return all
	}

	limit := fm.queueThrottleTopN
	if limit > len(all) {
		limit = len(all)
	}
	if limit >= len(all) {
		return all
	}

	// Randomly select N trackers from all available to distribute load
	// Create a copy of the forwarders slice to avoid modifying the original
	selected := make([]CoreCommon.Forward, len(all))
	copy(selected, all)

	// Shuffle using Fisher-Yates algorithm
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := len(selected) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		selected[i], selected[j] = selected[j], selected[i]
	}

	// Take the first N after shuffling
	result := make([]CoreCommon.Forward, 0, limit)
	for i := 0; i < limit; i++ {
		result = append(result, selected[i])
	}

	skipped := uint64(len(all) - limit)
	atomic.AddUint64(&fm.throttledForwardCount, skipped)
	if fm.Prometheus != nil {
		fm.Prometheus.Throttled.Add(float64(skipped))
	}

	return result
}

// QueueEligibleAnnounces enqueues announce jobs for forwarders that have never
// seen the hash or are past their NextAnnounce. Throttling is applied when the
// queue is under pressure.
func (fm *ForwarderManager) QueueEligibleAnnounces(infoHash common.InfoHash, request tracker.Request) {
	fm.forwardersMu.RLock()
	allForwarders := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(allForwarders, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	if len(allForwarders) == 0 {
		ErrorLogFwd.Printf("No forwarders available; skipping announce for %x", infoHash)
		return
	}

	forwarders := fm.selectForwardersByQueue(allForwarders)
	now := time.Now()

	for _, forwarder := range forwarders {
		forwarderName := forwarder.GetName()

		if fm.isSuspended(forwarderName) {
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping suspended forwarder %s for %x\n", forwarderName, infoHash)
			}
			continue
		}
		if !fm.Storage.ShouldAnnounceNow(infoHash, forwarderName, now) {
			continue
		}
		if fm.isJobPending(infoHash, forwarderName, request.PeerID) {
			if fm.Config.Debug {
				DebugLogFwd.Printf("Skipping duplicate job for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
			}
			continue
		}

		job := AnnounceJob{
			InfoHash:      infoHash,
			ForwarderName: forwarderName,
			PeerID:        request.PeerID,
			Forwarder:     forwarder,
			Request:       request,
		}

		fm.markJobPending(infoHash, forwarderName, request.PeerID)

		select {
		case fm.jobQueue <- job:
			if fm.Config.Debug {
				DebugLogFwd.Printf("Queued announce for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
			}
		default:
			// Queue full, unmark and skip
			fm.unmarkJobPending(infoHash, forwarderName, request.PeerID)
			atomic.AddUint64(&fm.droppedFullCount, 1)
			if fm.Prometheus != nil {
				fm.Prometheus.DroppedFull.Inc()
			}
			ErrorLogFwd.Printf("Job queue full, skipping announce for %x to %s", infoHash, forwarderName)
		}
	}
}

func (fm *ForwarderManager) shouldRateLimitInitial() bool {
	if fm.queueRateLimitThresh <= 0 {
		return false
	}
	return fm.queueFillPct() >= fm.queueRateLimitThresh
}

// ForwardStoppedEvent forwards a stopped event to all forwarders without scheduling future announces
// Sends to all forwarders in parallel (immediately, not queued) to avoid blocking the handler
func (fm *ForwarderManager) ForwardStoppedEvent(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	// Cancel any pending jobs for this peer first
	fm.CancelPendingJobs(infoHash, peerID)

	// Forward stopped event to all forwarders immediately in parallel
	fm.forwardersMu.RLock()
	forwarders := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwarders, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	var wg sync.WaitGroup
	for _, forwarder := range forwarders {
		forwarderName := forwarder.GetName()

		// Create a stopped event request
		stoppedRequest := request
		stoppedRequest.Event = EventStopped

		// Execute immediately in parallel (don't queue, send synchronously for stopped events)
		wg.Add(1)
		go func(f CoreCommon.Forward, fn string, req tracker.Request) {
			defer wg.Done()
			fm.executeStoppedAnnounce(f, fn, infoHash, peerID, req)
		}(forwarder, forwarderName, stoppedRequest)
	}
	// Wait for all forwarders to complete (with timeout to avoid blocking indefinitely)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// All forwarders completed
	case <-time.After(time.Second * time.Duration(fm.Config.ForwardTimeout)):
		// Timeout reached, but we don't block the handler - forwarders continue in background
		if fm.Config.Debug {
			DebugLogFwd.Printf("Timeout waiting for stopped event forwards to complete for %x\n", infoHash)
		}
	}
}

// ForwardCompletedEvent forwards a completed event to all forwarders (one-time notification, no scheduling change)
// Sends to all forwarders in parallel (immediately, not queued) to avoid blocking the handler
func (fm *ForwarderManager) ForwardCompletedEvent(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	// Forward completed event to all forwarders
	// Note: We don't cancel jobs because client continues as seeder
	fm.forwardersMu.RLock()
	forwarders := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwarders, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	var wg sync.WaitGroup
	for _, forwarder := range forwarders {
		forwarderName := forwarder.GetName()

		// Create a completed event request
		completedRequest := request
		completedRequest.Event = EventCompleted

		// Execute immediately in parallel (don't queue, send synchronously for completed events)
		wg.Add(1)
		go func(f CoreCommon.Forward, fn string, req tracker.Request) {
			defer wg.Done()
			fm.executeStoppedAnnounce(f, fn, infoHash, peerID, req)
		}(forwarder, forwarderName, completedRequest)
	}
	// Wait for all forwarders to complete (with timeout to avoid blocking indefinitely)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// All forwarders completed
	case <-time.After(time.Second * time.Duration(fm.Config.ForwardTimeout)):
		// Timeout reached, but we don't block the handler - forwarders continue in background
		if fm.Config.Debug {
			DebugLogFwd.Printf("Timeout waiting for completed event forwards to complete for %x\n", infoHash)
		}
	}
}

// executeStoppedAnnounce executes a stopped or completed event announce immediately
func (fm *ForwarderManager) executeStoppedAnnounce(forwarder CoreCommon.Forward, _ string, infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	// Detect protocol and route to appropriate handler
	protocol := forwarder.GetProtocol()
	if protocol == "udp" {
		fm.executeUDPStoppedAnnounce(forwarder, infoHash, peerID, request)
	} else {
		fm.executeHTTPStoppedAnnounce(forwarder, infoHash, peerID, request)
	}
}

// executeUDPStoppedAnnounce handles UDP tracker stopped/completed event announces
func (fm *ForwarderManager) executeUDPStoppedAnnounce(forwarder CoreCommon.Forward, infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	hash := fmt.Sprintf("%x", infoHash)
	trackerURL := forwarder.Uri

	if fm.Config.Debug {
		DebugLogFwd.Printf("Forwarding UDP %s event for %s (peer %x) to %s\n", request.Event, hash, peerID, trackerURL)
	}

	// Use UDP forwarder - we don't need to capture the response for stopped/completed events
	_, _, err := fm.udpForwarder.Announce(forwarder, request)
	if err != nil {
		ErrorLogFwd.Printf("Error forwarding UDP %s event for %s to %s: %s\n", request.Event, hash, trackerURL, err.Error())
		return
	}

	if fm.Config.Debug {
		DebugLogFwd.Printf("Successfully forwarded UDP %s event for %s to %s\n", request.Event, hash, trackerURL)
	}
}

// executeHTTPStoppedAnnounce handles HTTP tracker stopped/completed event announces
func (fm *ForwarderManager) executeHTTPStoppedAnnounce(forwarder CoreCommon.Forward, infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	hash := fmt.Sprintf("%x", infoHash)
	trackerURL := forwarder.Uri

	uri := fmt.Sprintf("%s?info_hash=%s&peer_id=%s&port=%d&uploaded=%d&downloaded=%d&left=%d&event=%s",
		forwarder.Uri, url.QueryEscape(string(request.InfoHash)),
		url.QueryEscape(string(request.PeerID)), request.Port, request.Uploaded, request.Downloaded, request.Left,
		url.QueryEscape(request.Event))
	if forwarder.Ip != `` {
		uri = fmt.Sprintf("%s&ip=%s&ipv4=%s", uri, forwarder.Ip, forwarder.Ip)
	}

	if fm.Config.Debug {
		DebugLogFwd.Printf("Forwarding %s event for %s (peer %x) to %s\n", request.Event, hash, peerID, trackerURL)
		DebugLogFwd.Printf("  Request URI: %s\n", uri)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(fm.Config.ForwardTimeout))
	defer cancel()

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		ErrorLogFwd.Printf("Error forwarding %s event for %s to %s: %s\n", request.Event, hash, trackerURL, err.Error())
		return
	}

	if forwarder.Host != `` {
		rqst.Host = forwarder.Host
	}

	client := http.Client{}
	response, err := client.Do(rqst)
	if err != nil {
		ErrorLogFwd.Printf("Error forwarding %s event for %s to %s: %s\n", request.Event, hash, trackerURL, err.Error())
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		ErrorLogFwd.Printf("Error forwarding %s event for %s to %s: HTTP %d %s\n", request.Event, hash, trackerURL, response.StatusCode, response.Status)
		return
	}

	// Read response but don't update storage for stopped/completed events
	_, err = io.ReadAll(response.Body)
	if err != nil {
		ErrorLogFwd.Printf("Error reading response for %s event for %s to %s: %s\n", request.Event, hash, trackerURL, err.Error())
		return
	}

	if fm.Config.Debug {
		DebugLogFwd.Printf("Successfully forwarded %s event for %s to %s\n", request.Event, hash, trackerURL)
	}
}

// CancelPendingJobs cancels any pending announce jobs for a specific peer
func (fm *ForwarderManager) CancelPendingJobs(infoHash common.InfoHash, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()

	// Find and remove all pending jobs for this peer
	keysToRemove := make([]string, 0)
	for key := range fm.pendingJobs {
		// Key format is: "infoHash:forwarderName:peerID"
		// We need to check if it matches our infoHash and peerID
		expectedPrefix := fmt.Sprintf("%x:", infoHash)
		expectedSuffix := fmt.Sprintf(":%x", peerID)
		if len(key) > len(expectedPrefix)+len(expectedSuffix) {
			if key[:len(expectedPrefix)] == expectedPrefix && key[len(key)-len(expectedSuffix):] == expectedSuffix {
				keysToRemove = append(keysToRemove, key)
			}
		}
	}

	for _, key := range keysToRemove {
		delete(fm.pendingJobs, key)
		if fm.Config.Debug {
			DebugLogFwd.Printf("Canceled pending job: %s\n", key)
		}
	}

	if len(keysToRemove) > 0 && fm.Config.Debug {
		DebugLogFwd.Printf("Canceled %d pending job(s) for peer %x\n", len(keysToRemove), peerID)
	}
}

func (fm *ForwarderManager) recordStats(forwarderName string, responseTime time.Duration, interval int) {
	fm.statsMu.Lock()
	defer fm.statsMu.Unlock()

	stats, ok := fm.stats[forwarderName]
	if !ok {
		stats = &ForwarderStats{
			SampleCount: 0,
		}
		fm.stats[forwarderName] = stats
	}

	stats.mu.Lock()
	// Use Exponential Moving Average (EMA) for response time with higher weight on recent values
	if stats.SampleCount == 0 {
		// First sample: set directly
		stats.AvgResponseTime = responseTime
		stats.LastInterval = interval
	} else {
		// EMA update: new_avg = alpha * new_sample + (1-alpha) * old_avg
		// Using higher alpha (0.15) gives 15% weight to latest response time for faster adaptation
		stats.AvgResponseTime = time.Duration(
			float64(responseTime)*emaAlphaResponseTime + float64(stats.AvgResponseTime)*(1-emaAlphaResponseTime),
		)
		// Store last reported interval (not averaged)
		stats.LastInterval = interval
	}
	stats.SampleCount++
	stats.mu.Unlock()
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

// schedulerRoutine periodically checks for scheduled jobs ready to execute
func (fm *ForwarderManager) schedulerRoutine() {
	ticker := time.NewTicker(5 * time.Second) // Check every 5 seconds
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
				if now.After(scheduledTime) || now.Equal(scheduledTime) {
					readyJobs = append(readyJobs, jobs...)
					timesToRemove = append(timesToRemove, scheduledTime)
				}
			}

			// Remove processed time slots
			for _, t := range timesToRemove {
				delete(fm.scheduledJobs, t)
			}
			fm.scheduledJobsMu.Unlock()

			// Queue ready jobs
			for _, job := range readyJobs {
				select {
				case fm.jobQueue <- job:
					if fm.Config.Debug {
						DebugLogFwd.Printf("Scheduled job ready, queued for %x to %s\n", job.InfoHash, job.ForwarderName)
					}
				default:
					// Queue full, reschedule
					fm.scheduledJobsMu.Lock()
					// Reschedule for 10 seconds later
					newTime := now.Add(10 * time.Second)
					fm.scheduledJobs[newTime] = append(fm.scheduledJobs[newTime], job)
					fm.scheduledJobsMu.Unlock()
					if fm.Config.Debug {
						DebugLogFwd.Printf("Queue full, rescheduled job for %x to %s\n", job.InfoHash, job.ForwarderName)
					}
				}
			}
		}
	}
}

// handleRetryError handles retry logic from tracker failure responses (BEP 31: Failure Retry Extension)
// Returns true if the error was handled (scheduled retry or disabled), false otherwise
func (fm *ForwarderManager) handleRetryError(job AnnounceJob, failureReason string, retryIn interface{}, trackerURL string) bool {
	if retryIn == nil {
		return false // No "retry in" field, not a retry extension response
	}

	// Check if retryIn is "never"
	if retryInStr, ok := retryIn.(string); ok && retryInStr == "never" {
		// Permanent failure - disable forwarder
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Tracker %s returned permanent failure (retry in: never): %s\n",
				trackerURL, failureReason)
		}
		fm.disableForwarder(job.ForwarderName, fmt.Sprintf("Permanent failure: %s", failureReason))
		return true
	}

	// Parse retryIn as minutes (int)
	retryMinutes := 0
	switch v := retryIn.(type) {
	case int:
		retryMinutes = v
	case int64:
		retryMinutes = int(v)
	case string:
		// Try to parse as integer
		if parsed, err := strconv.Atoi(v); err == nil {
			retryMinutes = parsed
		} else {
			if fm.Config.Debug {
				ErrorLogFwd.Printf("Invalid retry in value: %v (expected int or 'never')\n", v)
			}
			return false
		}
	default:
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Unexpected retry in type: %T (value: %v)\n", v, v)
		}
		return false
	}

	if retryMinutes <= 0 {
		return false // Invalid retry time
	}

	// Only reschedule if retry period is less than 10 minutes
	if retryMinutes >= 10 {
		// Retry period too long, consider announce completed
		// Don't update storage - preserve existing peers from last successful announce
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Tracker %s requested retry in %d minutes (>= 10), treating as completed (failure: %s)\n",
				trackerURL, retryMinutes, failureReason)
		}
		return true
	}

	// Schedule retry at the specified time
	now := time.Now()
	scheduledTime := now.Add(time.Duration(retryMinutes) * time.Minute)

	// Create a new job for retry
	retryJob := AnnounceJob{
		InfoHash:      job.InfoHash,
		ForwarderName: job.ForwarderName,
		PeerID:        job.PeerID,
		Forwarder:     job.Forwarder,
		Request:       job.Request,
		ScheduledTime: scheduledTime,
	}

	// Mark as pending and schedule
	fm.markJobPending(retryJob.InfoHash, retryJob.ForwarderName, retryJob.PeerID)
	if fm.scheduleJob(retryJob) {
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Scheduled retry for %x to %s in %d minutes (failure: %s)\n",
				job.InfoHash, job.ForwarderName, retryMinutes, failureReason)
		}
	} else {
		// Duplicate job already scheduled
		fm.unmarkJobPending(retryJob.InfoHash, retryJob.ForwarderName, retryJob.PeerID)
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Duplicate retry job already scheduled for %x to %s\n",
				job.InfoHash, job.ForwarderName)
		}
	}

	return true
}

// scheduleJob adds a job to the scheduled jobs map
// Returns true if job was scheduled, false if an identical job already exists
func (fm *ForwarderManager) scheduleJob(job AnnounceJob) bool {
	fm.scheduledJobsMu.Lock()
	defer fm.scheduledJobsMu.Unlock()

	// Check if we already have an identical scheduled job (same hash, same forwarder)
	for _, jobs := range fm.scheduledJobs {
		for _, existingJob := range jobs {
			if existingJob.InfoHash == job.InfoHash && existingJob.ForwarderName == job.ForwarderName {
				// Identical job already scheduled
				if fm.Config.Debug {
					DebugLogFwd.Printf("Skipping duplicate scheduled job for %x to %s (already scheduled at %v)\n",
						job.InfoHash, job.ForwarderName, existingJob.ScheduledTime)
				}
				return false
			}
		}
	}

	// No duplicate found, schedule the job
	fm.scheduledJobs[job.ScheduledTime] = append(fm.scheduledJobs[job.ScheduledTime], job)
	return true
}

// forwarderStatsProvider implements observability.StatsDataProvider
type forwarderStatsProvider struct {
	fm  *ForwarderManager
	cfg *config.Config
	now time.Time
}

func (p *forwarderStatsProvider) GetPendingCount() int {
	// pendingJobs tracks all pending jobs (both in queue and scheduled)
	// since markJobPending is called for both immediate and scheduled jobs
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

	// Include hashes tracked only in main storage (even if no forwarder entries)
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
		AvgResponseTime time.Duration
		LastInterval    int
		Count           int
	})

	for forwarderName, stats := range p.fm.stats {
		stats.mu.RLock()
		if stats.SampleCount > 0 {
			forwarderStats[forwarderName] = struct {
				AvgResponseTime time.Duration
				LastInterval    int
				Count           int
			}{
				AvgResponseTime: stats.AvgResponseTime,
				LastInterval:    stats.LastInterval,
				Count:           stats.SampleCount,
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
			result[i] = observability.ForwarderStat{
				Name:            forwarderName,
				Protocol:        protocol,
				AvgResponseTime: stats.AvgResponseTime,
				AvgInterval:     stats.LastInterval,
				SampleCount:     stats.Count,
				HasStats:        true,
			}
		} else {
			result[i] = observability.ForwarderStat{
				Name:     forwarderName,
				Protocol: protocol,
				HasStats: false,
			}
		}
	}

	return result
}

func (p *forwarderStatsProvider) GetHashPeerStats() map[string]observability.HashPeerStat {
	p.fm.Storage.mu.RLock()
	defer p.fm.Storage.mu.RUnlock()

	hashPeerStats := make(map[string]observability.HashPeerStat)

	for infoHash, forwarders := range p.fm.Storage.Entries {
		seenLocal := make(map[common.PeerID]struct{})
		seenForwarder := make(map[common.PeerID]struct{})

		// Count local peers (unique by peer ID)
		if p.fm.MainStorage != nil {
			p.fm.MainStorage.requestsMu.Lock()
			if requests, ok := p.fm.MainStorage.Requests[infoHash]; ok {
				for peerID := range requests {
					seenLocal[peerID] = struct{}{}
				}
			}
			p.fm.MainStorage.requestsMu.Unlock()
		}

		// Count forwarder peers (unique by peer ID)
		for _, entry := range forwarders {
			for _, peer := range entry.Peers {
				if peer.PeerID == "" {
					continue
				}
				seenForwarder[peer.PeerID] = struct{}{}
			}
		}

		// Build totals
		totalSeen := make(map[common.PeerID]struct{})
		for peerID := range seenLocal {
			totalSeen[peerID] = struct{}{}
		}
		for peerID := range seenForwarder {
			totalSeen[peerID] = struct{}{}
		}

		hashKey := fmt.Sprintf("%x", infoHash)
		hashPeerStats[hashKey] = observability.HashPeerStat{
			LocalUnique:     len(seenLocal),
			ForwarderUnique: len(seenForwarder),
			TotalUnique:     len(totalSeen),
		}
	}

	return hashPeerStats
}

func (p *forwarderStatsProvider) GetClientStats() *observability.ClientStats {
	if p.fm.MainStorage == nil {
		return nil
	}

	// Map to track clients: "IP:ClientName" -> {hashCount, lastRequestTime}
	type clientInfo struct {
		hashCount       int
		lastRequest     time.Time
		announcedHashes map[string]bool // Track unique hashes
	}

	clients := make(map[string]*clientInfo)

	p.fm.MainStorage.requestsMu.Lock()
	for hash, requests := range p.fm.MainStorage.Requests {
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
	return atomic.LoadUint64(&p.fm.droppedFullCount), atomic.LoadUint64(&p.fm.rateLimitedCount), atomic.LoadUint64(&p.fm.throttledForwardCount)
}

func (p *forwarderStatsProvider) GetConfig() *config.Config {
	return p.cfg
}

func (fm *ForwarderManager) printStats() {
	now := time.Now()
	collector := observability.NewStatsCollector()
	provider := &forwarderStatsProvider{fm: fm, cfg: fm.Config, now: now}
	stats := collector.CollectStats(provider)
	if fm.Prometheus != nil {
		fm.updatePrometheusQueue(stats)
	}
	text := collector.FormatText(stats)
	fmt.Print(text)
}
