// forwarderManager.go - Core forwarder orchestration and worker pool management.
// Manages job queue, worker scaling, rate limiting, throttling, and forwarder lifecycle.
// Handles job scheduling, deduplication, and forwarder enable/disable/suspend logic.
package server

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
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
	DebugLogFwd = log.New(os.Stdout, `debug#`, log.Lshortfile)
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
	workerCount              int
	workerCountMu            sync.Mutex
	scaleDownChan            chan struct{}
	rateLimiter              *tokenBucket
	droppedFullCount         uint64
	rateLimitedCount         uint64

	suspendedMu         sync.Mutex
	suspendedForwarders map[string]time.Time

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
		workerCount:              cfg.ForwarderWorkers,
		rateLimiter: &tokenBucket{
			rate:   ratePerSec,
			burst:  rateBurst,
			tokens: rateBurst,
		},
		suspendedForwarders: make(map[string]time.Time),
		scaleDownChan:       make(chan struct{}, 100),
		scheduledJobs:       make(map[time.Time][]AnnounceJob),
	}

	for _, forwarder := range cfg.Forwards {
		fm.stats[forwarder.GetName()] = &ForwarderStats{SampleCount: 0}
	}
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
				if fm.Config.Debug {
					DebugLogFwd.Printf("Worker scaling down, remaining: %d\n", fm.workerCount)
				}
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
				if fm.Config.Debug {
					DebugLogFwd.Printf("Scaling workers up to %d (queue %d%%)\n", newCount, fillPct)
				}
				if fm.Prometheus != nil {
					fm.Prometheus.WorkerCount.Set(float64(newCount))
				}
			case fillPct < 40 && currentCount > baseWorkers:
				select {
				case fm.scaleDownChan <- struct{}{}:
					if fm.Config.Debug {
						DebugLogFwd.Printf("Signaling worker to scale down (queue %d%%, workers: %d)\n", fillPct, currentCount)
					}
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
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	_ = err
	return false
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
		if fm.Config.Debug {
			DebugLogFwd.Printf("Queue at %d%% (>= %d%%), throttling to %d forwarders\n",
				fill, fm.queueThrottleThresh, limit)
		}
	}

	now := time.Now()
	queued := 0

forwarderLoop:
	for _, forwarder := range forwarders {
		if queued >= limit {
			break
		}

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
			queued++
			if fm.Config.Debug {
				DebugLogFwd.Printf("Queued announce for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
			}
		default:
			fm.unmarkJobPending(infoHash, forwarderName, request.PeerID)
			atomic.AddUint64(&fm.droppedFullCount, 1)
			if fm.Prometheus != nil {
				fm.Prometheus.DroppedFull.Inc()
			}
			ErrorLogFwd.Printf("Job queue full, stopping announce queueing for %x", infoHash)
			break forwarderLoop
		}
	}

	if fm.Config.Debug && queued > 0 {
		DebugLogFwd.Printf("Queued %d announces for %x\n", queued, infoHash)
	}
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
				select {
				case fm.jobQueue <- job:
					if fm.Config.Debug {
						DebugLogFwd.Printf("Scheduled job ready, queued for %x to %s\n", job.InfoHash, job.ForwarderName)
					}
				default:
					fm.scheduledJobsMu.Lock()
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
