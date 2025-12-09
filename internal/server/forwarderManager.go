package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	EventStopped   = "stopped"
	EventCompleted = "completed"
)

var (
	DebugLogFwd = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogFwd = log.New(os.Stderr, `error#`, log.Lshortfile)
)

type ForwarderStats struct {
	ResponseTimes []time.Duration
	Intervals     []int
	mu            sync.RWMutex
}

type ForwarderManager struct {
	Config       *config.Config
	Storage      *ForwarderStorage
	MainStorage  *Storage // Reference to main storage for client statistics
	Forwarders   []CoreCommon.Forward
	Workers      int
	jobQueue     chan AnnounceJob
	stopChan     chan struct{}
	Prometheus   *observability.Prometheus
	TempStorage  *TempStorage
	requestCache map[string]tracker.Request // cache of last request per info_hash for re-announcing
	cacheMu      sync.RWMutex
	pendingJobs  map[string]bool // track pending jobs: "infoHash:forwarderName" -> true
	pendingMu    sync.Mutex
	stats        map[string]*ForwarderStats // forwarderName -> stats
	statsMu      sync.RWMutex
	udpForwarder *UDPForwarder // UDP forwarder client for UDP trackers
	failCounts   map[string]int
	disabled     map[string]struct{}
}

func NewForwarderManager(cfg *config.Config, storage *ForwarderStorage, mainStorage *Storage, prom *observability.Prometheus, tempStorage *TempStorage) *ForwarderManager {
	queueSize := cfg.ForwarderQueueSize
	if queueSize <= 0 {
		queueSize = 1000
	}
	fm := &ForwarderManager{
		Config:       cfg,
		Storage:      storage,
		MainStorage:  mainStorage,
		Forwarders:   cfg.Forwards,
		Workers:      cfg.ForwarderWorkers,
		jobQueue:     make(chan AnnounceJob, queueSize),
		stopChan:     make(chan struct{}),
		Prometheus:   prom,
		TempStorage:  tempStorage,
		requestCache: make(map[string]tracker.Request),
		pendingJobs:  make(map[string]bool),
		stats:        make(map[string]*ForwarderStats),
		udpForwarder: NewUDPForwarder(cfg.Debug, cfg.ForwardTimeout, cfg.ForwarderRetryAttempts, cfg.ForwarderRetryBaseMs),
		failCounts:   make(map[string]int),
		disabled:     make(map[string]struct{}),
	}
	// Initialize stats for each forwarder
	for _, forwarder := range cfg.Forwards {
		fm.stats[forwarder.GetName()] = &ForwarderStats{
			ResponseTimes: make([]time.Duration, 0),
			Intervals:     make([]int, 0),
		}
	}
	return fm
}

func (fm *ForwarderManager) Start() {
	// Start worker pool
	for i := 0; i < fm.Workers; i++ {
		go fm.worker()
	}
	// No scheduler - re-announcing is client-driven

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
	for {
		select {
		case <-fm.stopChan:
			return
		case job, ok := <-fm.jobQueue:
			if !ok {
				return
			}
			fm.executeAnnounce(job)
			// Unmark job as pending when done
			fm.unmarkJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
		}
	}
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
		fm.disableForwarder(forwarderName)
	}
}

func (fm *ForwarderManager) resetFailure(forwarderName string) {
	fm.pendingMu.Lock()
	delete(fm.failCounts, forwarderName)
	fm.pendingMu.Unlock()
}

func (fm *ForwarderManager) disableForwarder(forwarderName string) {
	fm.pendingMu.Lock()
	fm.disabled[forwarderName] = struct{}{}
	delete(fm.failCounts, forwarderName)
	fm.pendingMu.Unlock()

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
		DebugLogFwd.Printf("Disabled forwarder %s after repeated failures", forwarderName)
	}
}

func (fm *ForwarderManager) isDisabled(forwarderName string) bool {
	fm.pendingMu.Lock()
	_, disabled := fm.disabled[forwarderName]
	fm.pendingMu.Unlock()
	return disabled
}

func (fm *ForwarderManager) executeAnnounce(job AnnounceJob) {
	forward := job.Forwarder

	if fm.isDisabled(forward.GetName()) {
		if fm.Config.Debug {
			DebugLogFwd.Printf("Forwarder %s is disabled; dropping job for %x", forward.GetName(), job.InfoHash)
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
		if isTimeoutErr(err) {
			fm.registerFailure(forwardName)
		} else {
			fm.disableForwarder(forwardName)
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
			if !isTimeoutErr(err) {
				if fm.Config.Debug {
					ErrorLogFwd.Printf("HTTP request error: %v\n", err)
				}
				fm.disableForwarder(forwardName)
				return
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
			if fm.Config.Debug {
				ErrorLogFwd.Printf("HTTP %d %s\n", response.StatusCode, response.Status)
			}
			cancel()
			fm.disableForwarder(forwardName)
			return
		}

		payload, err := io.ReadAll(body)
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			if !isTimeoutErr(err) {
				if fm.Config.Debug {
					ErrorLogFwd.Printf("Failed to read response: %v\n", err)
				}
				fm.disableForwarder(forwardName)
				return
			}
			continue
		}

		tempFilename := ``
		if fm.Config.Debug {
			tempFilename = fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
		}

		bitResponse, err := Response.Load(payload)
		if err != nil {
			if fm.Config.Debug {
				ErrorLogFwd.Printf("Failed to parse response: %v\n", err)
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
			fm.disableForwarder(forwardName)
			return
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
	if isTimeoutErr(lastErr) {
		fm.registerFailure(forwardName)
	} else {
		fm.disableForwarder(forwardName)
	}
}

func (fm *ForwarderManager) CacheRequest(infoHash common.InfoHash, request tracker.Request) {
	fm.cacheMu.Lock()
	defer fm.cacheMu.Unlock()
	fm.requestCache[string(infoHash)] = request
}

func (fm *ForwarderManager) TriggerInitialAnnounce(infoHash common.InfoHash, request tracker.Request) {
	if len(fm.Forwarders) == 0 {
		ErrorLogFwd.Printf("No forwarders available; skipping initial announce for %x", infoHash)
		return
	}
	if fm.Config.Debug {
		DebugLogFwd.Printf("Triggering initial announce for %x to %d forwarder(s)", infoHash, len(fm.Forwarders))
	}

	// Trigger parallel decoupled announces to all forwarders
	for _, forwarder := range fm.Forwarders {
		forwarderName := forwarder.GetName()

		if fm.isDisabled(forwarderName) {
			continue
		}

		// Check if job is already pending
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

		// Mark as pending before queuing
		fm.markJobPending(infoHash, forwarderName, request.PeerID)

		select {
		case fm.jobQueue <- job:
			if fm.Config.Debug {
				DebugLogFwd.Printf("Queued initial announce for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
			}
		default:
			// Queue full, unmark and skip
			fm.unmarkJobPending(infoHash, forwarderName, request.PeerID)
			ErrorLogFwd.Printf("Job queue full, skipping initial announce for %x to %s", infoHash, forwarderName)
		}
	}
}

func (fm *ForwarderManager) CheckAndReannounce(infoHash common.InfoHash, request tracker.Request, clientInterval int) {
	// Collect jobs to queue (outside of lock)
	jobsToQueue := make([]AnnounceJob, 0)
	nextAnnounces := make(map[string]time.Time) // forwarderName -> nextAnnounce time

	fm.Storage.mu.RLock()
	if forwarders, ok := fm.Storage.Entries[infoHash]; ok {
		for forwarderName, entry := range forwarders {
			if fm.isDisabled(forwarderName) {
				continue
			}
			// Skip if no interval yet (forwarder hasn't responded)
			if entry.Interval <= 0 {
				continue
			}

			// Check if client interval is bigger than forwarder interval -> re-announce immediately
			if clientInterval > entry.Interval {
				// Re-announce immediately
				if fm.isJobPending(infoHash, forwarderName, request.PeerID) {
					if fm.Config.Debug {
						DebugLogFwd.Printf("Re-announce job already pending for %x to %s (peer %x)\n", infoHash, forwarderName, request.PeerID)
					}
					continue
				}

				// Find the forwarder
				var forwarder CoreCommon.Forward
				for _, f := range fm.Forwarders {
					if f.GetName() == forwarderName {
						forwarder = f
						break
					}
				}
				if forwarder.Uri == "" {
					continue
				}

				jobsToQueue = append(jobsToQueue, AnnounceJob{
					InfoHash:      infoHash,
					ForwarderName: forwarderName,
					PeerID:        request.PeerID,
					Forwarder:     forwarder,
					Request:       request,
				})
			} else if clientInterval < entry.Interval {
				// Client interval is smaller -> schedule one re-announce following forwarder indication
				now := time.Now()
				nextAnnounce := now.Add(time.Duration(entry.Interval) * time.Second)

				// Only schedule if not already pending and it's time
				if !fm.isJobPending(infoHash, forwarderName, request.PeerID) && (now.After(entry.NextAnnounce) || now.Equal(entry.NextAnnounce)) {
					// Find the forwarder
					var forwarder CoreCommon.Forward
					for _, f := range fm.Forwarders {
						if f.GetName() == forwarderName {
							forwarder = f
							break
						}
					}
					if forwarder.Uri == "" {
						continue
					}

					jobsToQueue = append(jobsToQueue, AnnounceJob{
						InfoHash:      infoHash,
						ForwarderName: forwarderName,
						PeerID:        request.PeerID,
						Forwarder:     forwarder,
						Request:       request,
					})
					nextAnnounces[forwarderName] = nextAnnounce
				}
			}
		}
	}
	fm.Storage.mu.RUnlock()

	// Queue jobs and update NextAnnounce times
	for _, job := range jobsToQueue {
		fm.markJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
		select {
		case fm.jobQueue <- job:
			if fm.Config.Debug {
				hash := fmt.Sprintf("%x", job.InfoHash)
				if nextAnnounce, ok := nextAnnounces[job.ForwarderName]; ok {
					DebugLogFwd.Printf("Queued scheduled re-announce for %s to %s (following forwarder interval)\n",
						hash, job.ForwarderName)
					// Update NextAnnounce time
					fm.Storage.mu.Lock()
					if f, ok := fm.Storage.Entries[job.InfoHash]; ok {
						if e, ok := f[job.ForwarderName]; ok {
							e.NextAnnounce = nextAnnounce
							f[job.ForwarderName] = e
						}
					}
					fm.Storage.mu.Unlock()
				} else {
					// Immediate re-announce
					fm.Storage.mu.RLock()
					entry := fm.Storage.Entries[job.InfoHash][job.ForwarderName]
					fm.Storage.mu.RUnlock()
					DebugLogFwd.Printf("Queued immediate re-announce for %s to %s (client interval %d > forwarder interval %d)\n",
						hash, job.ForwarderName, clientInterval, entry.Interval)
				}
			}
		default:
			fm.unmarkJobPending(job.InfoHash, job.ForwarderName, job.PeerID)
			if fm.Config.Debug {
				hash := fmt.Sprintf("%x", job.InfoHash)
				DebugLogFwd.Printf("Job queue full, skipping re-announce for %s to %s\n", hash, job.ForwarderName)
			}
		}
	}
}

// ForwardStoppedEvent forwards a stopped event to all forwarders without scheduling future announces
// Sends to all forwarders in parallel (immediately, not queued) to avoid blocking the handler
func (fm *ForwarderManager) ForwardStoppedEvent(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	// Cancel any pending jobs for this peer first
	fm.CancelPendingJobs(infoHash, peerID)

	// Forward stopped event to all forwarders immediately in parallel
	var wg sync.WaitGroup
	for _, forwarder := range fm.Forwarders {
		forwarderName := forwarder.GetName()

		if fm.isDisabled(forwarderName) {
			continue
		}

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
	var wg sync.WaitGroup
	for _, forwarder := range fm.Forwarders {
		forwarderName := forwarder.GetName()

		if fm.isDisabled(forwarderName) {
			continue
		}

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
			ResponseTimes: make([]time.Duration, 0),
			Intervals:     make([]int, 0),
		}
		fm.stats[forwarderName] = stats
	}

	stats.mu.Lock()
	// Keep last 100 response times and intervals
	stats.ResponseTimes = append(stats.ResponseTimes, responseTime)
	if len(stats.ResponseTimes) > 100 {
		stats.ResponseTimes = stats.ResponseTimes[1:]
	}
	stats.Intervals = append(stats.Intervals, interval)
	if len(stats.Intervals) > 100 {
		stats.Intervals = stats.Intervals[1:]
	}
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

func (fm *ForwarderManager) printStats() {
	now := time.Now()

	// Count scheduled announcements
	fm.pendingMu.Lock()
	pendingCount := len(fm.pendingJobs)
	fm.pendingMu.Unlock()

	// Get scheduled announcements with time to execution
	fm.Storage.mu.RLock()
	scheduledAnnounces := make([]struct {
		InfoHash      string
		ForwarderName string
		TimeToExec    time.Duration
	}, 0)

	trackedHashSet := make(map[string]struct{})
	// Collect per-hash IP statistics
	type hashStats struct {
		LocalUnique     int
		ForwarderUnique int
		TotalUnique     int
	}
	hashPeerStats := make(map[string]hashStats)

	for infoHash, forwarders := range fm.Storage.Entries {
		seenLocal := make(map[string]struct{})
		seenForwarder := make(map[string]struct{})
		trackedHashSet[fmt.Sprintf("%x", infoHash)] = struct{}{}

		// Count local peers (unique by IP)
		if fm.MainStorage != nil {
			localPeers := fm.MainStorage.GetPeers(infoHash)
			for _, peer := range localPeers {
				ip := string(peer.IP)
				if ip == "" {
					continue
				}
				seenLocal[ip] = struct{}{}
			}
		}

		// Count forwarder peers (unique by IP)
		for _, entry := range forwarders {
			for _, peer := range entry.Peers {
				ip := string(peer.IP)
				if ip == "" {
					continue
				}
				seenForwarder[ip] = struct{}{}
			}
		}

		// Build totals
		totalSeen := make(map[string]struct{})
		for ip := range seenLocal {
			totalSeen[ip] = struct{}{}
		}
		for ip := range seenForwarder {
			totalSeen[ip] = struct{}{}
		}

		hashKey := fmt.Sprintf("%x", infoHash)
		hashPeerStats[hashKey] = hashStats{
			LocalUnique:     len(seenLocal),
			ForwarderUnique: len(seenForwarder),
			TotalUnique:     len(totalSeen),
		}

		for forwarderName, entry := range forwarders {
			if entry.NextAnnounce.After(now) {
				timeToExec := entry.NextAnnounce.Sub(now)
				scheduledAnnounces = append(scheduledAnnounces, struct {
					InfoHash      string
					ForwarderName string
					TimeToExec    time.Duration
				}{
					InfoHash:      fmt.Sprintf("%x", infoHash),
					ForwarderName: forwarderName,
					TimeToExec:    timeToExec,
				})
			}
		}
	}
	fm.Storage.mu.RUnlock()

	// Include hashes tracked only in main storage (even if no forwarder entries)
	if fm.MainStorage != nil {
		fm.MainStorage.requestsMu.Lock()
		for infoHash := range fm.MainStorage.Requests {
			trackedHashSet[fmt.Sprintf("%x", infoHash)] = struct{}{}
		}
		fm.MainStorage.requestsMu.Unlock()
	}

	trackedHashes := len(trackedHashSet)

	// Get forwarder statistics
	fm.statsMu.RLock()
	forwarderStats := make(map[string]struct {
		AvgResponseTime time.Duration
		AvgInterval     int
		Count           int
	})

	for forwarderName, stats := range fm.stats {
		stats.mu.RLock()
		if len(stats.ResponseTimes) > 0 {
			var totalTime time.Duration
			for _, rt := range stats.ResponseTimes {
				totalTime += rt
			}
			avgResponseTime := totalTime / time.Duration(len(stats.ResponseTimes))

			var totalInterval int
			for _, interval := range stats.Intervals {
				totalInterval += interval
			}
			avgInterval := 0
			if len(stats.Intervals) > 0 {
				avgInterval = totalInterval / len(stats.Intervals)
			}

			forwarderStats[forwarderName] = struct {
				AvgResponseTime time.Duration
				AvgInterval     int
				Count           int
			}{
				AvgResponseTime: avgResponseTime,
				AvgInterval:     avgInterval,
				Count:           len(stats.ResponseTimes),
			}
		}
		stats.mu.RUnlock()
	}
	fm.statsMu.RUnlock()

	// Print statistics
	fmt.Printf("\n=== Statistics ===\n")
	fmt.Printf("Scheduled announcements: %d\n", pendingCount)

	if len(scheduledAnnounces) > 0 {
		sort.Slice(scheduledAnnounces, func(i, j int) bool {
			return scheduledAnnounces[i].TimeToExec < scheduledAnnounces[j].TimeToExec
		})
		limit := len(scheduledAnnounces)
		if limit > 10 {
			limit = 10
		}
		fmt.Printf("Scheduled announces (next %d):\n", limit)
		for _, sa := range scheduledAnnounces[:limit] {
			hashDisplay := sa.InfoHash
			if len(hashDisplay) > 16 {
				hashDisplay = hashDisplay[:16] + "..."
			}
			fmt.Printf("  %s -> %s: %v\n", hashDisplay, sa.ForwarderName, sa.TimeToExec.Round(time.Second))
		}
	}

	fmt.Printf("Tracked hashes: %d\n", trackedHashes)
	disabledCount := len(fm.disabled)
	activeCount := len(fm.Forwarders) - disabledCount
	if activeCount < 0 {
		activeCount = 0
	}
	fmt.Printf("Disabled forwarders: %d\n", disabledCount)
	fmt.Printf("Active forwarders: %d\n", activeCount)

	for _, forwarder := range fm.Forwarders {
		forwarderName := forwarder.GetName()
		if stats, ok := forwarderStats[forwarderName]; ok {
			fmt.Printf("  %s: avg response time %v, avg interval %ds (from %d samples)\n",
				forwarderName, stats.AvgResponseTime.Round(time.Millisecond), stats.AvgInterval, stats.Count)
		} else {
			fmt.Printf("  %s: no statistics yet\n", forwarderName)
		}
	}

	// Print per-hash unique IP counts
	if len(hashPeerStats) > 0 {
		fmt.Printf("Hash unique IPs (local/forwarders/total):\n")
		for hash, stats := range hashPeerStats {
			hashDisplay := hash
			if len(hashDisplay) > 16 {
				hashDisplay = hashDisplay[:16] + "..."
			}
			fmt.Printf("  %s: %d/%d/%d\n", hashDisplay, stats.LocalUnique, stats.ForwarderUnique, stats.TotalUnique)
		}
	}

	// Print client statistics
	if fm.MainStorage != nil {
		fm.MainStorage.printClientStatsInline(now)
	}

	fmt.Printf("==================\n\n")
}
