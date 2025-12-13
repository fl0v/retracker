// forwarderAnnounce.go - Unified announce execution logic for forwarders.
// Routes to HTTP or UDP protocol handlers, processes results, handles retries.
// Key functions: executeAnnounce(), doHTTPAnnounce(), doUDPAnnounce(), handleAnnounceResult()
// Also handles event forwarding (ForwardStoppedEvent, ForwardCompletedEvent).
package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"

	"github.com/prometheus/client_golang/prometheus"
)

// AnnounceResult represents the outcome of an announce attempt
type AnnounceResult struct {
	Success      bool
	Peers        []common.Peer
	Interval     int
	ResponseSize int
	Duration     time.Duration
	Error        error
	NonRetryable bool // If true, error is non-retryable (disable forwarder)
	Suspend      bool // If true, forwarder should be suspended (e.g., 429)
}

// executeAnnounce routes to the appropriate protocol handler
func (fm *ForwarderManager) executeAnnounce(job AnnounceJob) {
	forward := job.Forwarder
	forwardName := forward.GetName()

	if fm.isDisabled(forwardName) {
		if fm.Config.Debug {
			DebugLogFwd.Printf("Forwarder %s is disabled; dropping job for %x", forwardName, job.InfoHash)
		}
		return
	}
	if fm.isSuspended(forwardName) {
		if fm.Config.Debug {
			DebugLogFwd.Printf("Forwarder %s is suspended; dropping job for %x", forwardName, job.InfoHash)
		}
		return
	}

	protocol := forward.GetProtocol()
	var result AnnounceResult

	if protocol == "udp" {
		result = fm.doUDPAnnounce(job)
	} else {
		result = fm.doHTTPAnnounce(job)
	}

	fm.handleAnnounceResult(job, result)
}

// handleAnnounceResult processes the result of an announce attempt
func (fm *ForwarderManager) handleAnnounceResult(job AnnounceJob, result AnnounceResult) {
	forwardName := job.Forwarder.GetName()
	trackerURL := job.Forwarder.Uri

	if result.Success {
		// Update storage with peers
		fm.Storage.UpdatePeers(job.InfoHash, forwardName, result.Peers, result.Interval)
		fm.resetFailure(forwardName)
		fm.recordStats(forwardName, result.Duration, result.Interval)

		if fm.Prometheus != nil {
			fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `200`}).Inc()
		}

		secs := result.Duration.Seconds()
		protocol := job.Forwarder.GetProtocol()
		fmt.Printf("%s response %x from %s \n\t(%d bytes, %.3fs, interval=%d, peers=%d)\n",
			protocol, job.InfoHash, trackerURL, result.ResponseSize, secs, result.Interval, len(result.Peers))
		return
	}

	// Handle failure
	hash := fmt.Sprintf("%x", job.InfoHash)

	if result.Suspend {
		suspendFor := time.Duration(fm.Config.ForwarderSuspendSeconds) * time.Second
		if suspendFor <= 0 {
			suspendFor = 300 * time.Second
		}
		fm.suspendForwarder(forwardName, suspendFor)
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Suspending forwarder %s for %v\n", forwardName, suspendFor)
		}
		return
	}

	ErrorLogFwd.Printf("Announce error %s to %s: %s\n", hash, trackerURL, result.Error.Error())
	if fm.Config.Debug {
		ErrorLogFwd.Printf("  Duration: %v\n", result.Duration)
	}

	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `error`}).Inc()
	}

	if result.NonRetryable {
		fm.disableForwarder(forwardName, result.Error.Error())
	} else {
		fm.registerFailure(forwardName)
	}

	// Mark as attempted with default interval to avoid immediate retry
	fm.Storage.UpdatePeers(job.InfoHash, forwardName, []common.Peer{}, 60)
}

// doUDPAnnounce performs a UDP tracker announce
func (fm *ForwarderManager) doUDPAnnounce(job AnnounceJob) AnnounceResult {
	startTime := time.Now()
	forward := job.Forwarder
	hash := fmt.Sprintf("%x", job.InfoHash)

	fmt.Printf("UDP announce %s to %s\n", hash, forward.Uri)
	if fm.Config.Debug && forward.Ip != "" {
		DebugLogFwd.Printf("  Using IP: %s\n", forward.Ip)
	}

	bitResponse, respBytes, err := fm.udpForwarder.Announce(forward, job.Request)
	duration := time.Since(startTime)

	if err != nil {
		return AnnounceResult{
			Success:      false,
			Duration:     duration,
			Error:        err,
			NonRetryable: isNonRetryableError(err),
		}
	}

	return AnnounceResult{
		Success:      true,
		Peers:        bitResponse.Peers,
		Interval:     bitResponse.Interval,
		ResponseSize: respBytes,
		Duration:     duration,
	}
}

// doHTTPAnnounce performs an HTTP/HTTPS tracker announce with retries
func (fm *ForwarderManager) doHTTPAnnounce(job AnnounceJob) AnnounceResult {
	startTime := time.Now()
	forward := job.Forwarder
	request := job.Request
	hash := fmt.Sprintf("%x", job.InfoHash)
	trackerURL := forward.Uri

	uri := fm.buildAnnounceURI(forward, request, "")

	fmt.Printf("HTTP announce %s to %s\n", hash, trackerURL)
	if fm.Config.Debug {
		DebugLogFwd.Printf("  Request URI: %s\n", uri)
		if forward.Ip != "" {
			DebugLogFwd.Printf("  Using IP: %s\n", forward.Ip)
		}
		if forward.Host != "" {
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

		result := fm.doSingleHTTPRequest(job, uri, startTime)

		// Handle terminal conditions
		if result.Success {
			return result
		}
		if result.NonRetryable || result.Suspend {
			return result
		}

		lastErr = result.Error
	}

	// All retries failed
	return AnnounceResult{
		Success:      false,
		Duration:     time.Since(startTime),
		Error:        lastErr,
		NonRetryable: lastErr != nil && isNonRetryableError(lastErr),
	}
}

// doSingleHTTPRequest performs a single HTTP request attempt
func (fm *ForwarderManager) doSingleHTTPRequest(job AnnounceJob, uri string, startTime time.Time) AnnounceResult {
	forward := job.Forwarder
	forwardName := forward.GetName()
	hash := fmt.Sprintf("%x", job.InfoHash)
	trackerURL := forward.Uri

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(fm.Config.ForwardTimeout))
	defer cancel()

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return AnnounceResult{Error: err}
	}

	if forward.Host != "" {
		rqst.Host = forward.Host
	}

	client := http.Client{}
	response, err := client.Do(rqst)
	duration := time.Since(startTime)

	if err != nil {
		nonRetryable := isNonRetryableError(err)
		if fm.Config.Debug && !isTimeoutErr(err) {
			ErrorLogFwd.Printf("HTTP request error (%s): %v\n",
				map[bool]string{true: "non-retryable", false: "retryable"}[nonRetryable], err)
		}
		return AnnounceResult{
			Duration:     duration,
			Error:        err,
			NonRetryable: nonRetryable,
		}
	}

	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{
			`name`:   forwardName,
			`status`: fmt.Sprintf("%d", response.StatusCode),
		}).Inc()
	}

	if response.StatusCode != http.StatusOK {
		_, _ = io.ReadAll(response.Body)
		response.Body.Close()

		if shouldSuspendForwarder(response.StatusCode, nil) {
			return AnnounceResult{
				Duration: duration,
				Error:    fmt.Errorf("HTTP status %d", response.StatusCode),
				Suspend:  true,
			}
		}

		if isTrackerRejection(response.StatusCode) {
			return AnnounceResult{
				Duration:     duration,
				Error:        fmt.Errorf("HTTP status %d (tracker rejection)", response.StatusCode),
				NonRetryable: true,
			}
		}

		if fm.Config.Debug {
			ErrorLogFwd.Printf("HTTP %d %s (retryable)\n", response.StatusCode, response.Status)
		}
		return AnnounceResult{
			Duration: duration,
			Error:    fmt.Errorf("HTTP status %d: %s", response.StatusCode, response.Status),
		}
	}

	payload, err := io.ReadAll(response.Body)
	response.Body.Close()

	if err != nil {
		nonRetryable := isNonRetryableError(err)
		if fm.Config.Debug && !isTimeoutErr(err) {
			ErrorLogFwd.Printf("Failed to read response (%s): %v\n",
				map[bool]string{true: "non-retryable", false: "retryable"}[nonRetryable], err)
		}
		return AnnounceResult{
			Duration:     duration,
			Error:        fmt.Errorf("failed to read response: %w", err),
			NonRetryable: nonRetryable,
		}
	}

	if fm.Config.Debug {
		fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
	}

	bitResponse, err := Response.Load(payload)
	if err != nil {
		if fm.Config.Debug {
			fm.logParseError(payload, hash, uri, err)
		}
		return AnnounceResult{
			Duration: duration,
			Error:    fmt.Errorf("failed to parse response: %w", err),
		}
	}

	// Check for failure with "retry in" (BEP 31)
	if bitResponse.FailureReason != "" {
		if handled := fm.handleRetryError(job, bitResponse.FailureReason, bitResponse.RetryIn, trackerURL); handled {
			// Handled by retry scheduling - return success to stop further retries
			return AnnounceResult{Success: true, Duration: duration}
		}
		if fm.Config.Debug {
			ErrorLogFwd.Printf("Tracker failure response: %s\n", bitResponse.FailureReason)
		}
		return AnnounceResult{
			Duration: duration,
			Error:    fmt.Errorf("tracker failure: %s", bitResponse.FailureReason),
		}
	}

	return AnnounceResult{
		Success:      true,
		Peers:        bitResponse.Peers,
		Interval:     bitResponse.Interval,
		ResponseSize: len(payload),
		Duration:     duration,
	}
}

// buildAnnounceURI constructs the announce URL
func (fm *ForwarderManager) buildAnnounceURI(forward CoreCommon.Forward, request tracker.Request, event string) string {
	uri := fmt.Sprintf("%s?info_hash=%s&peer_id=%s&port=%d&uploaded=%d&downloaded=%d&left=%d",
		forward.Uri, url.QueryEscape(string(request.InfoHash)),
		url.QueryEscape(string(request.PeerID)), request.Port, request.Uploaded, request.Downloaded, request.Left)

	if event != "" {
		uri = fmt.Sprintf("%s&event=%s", uri, url.QueryEscape(event))
	}
	if forward.Ip != "" {
		uri = fmt.Sprintf("%s&ip=%s&ipv4=%s", uri, forward.Ip, forward.Ip)
	}
	return uri
}

// logParseError logs details about a parse failure
func (fm *ForwarderManager) logParseError(payload []byte, hash, uri string, err error) {
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

	tempFilename := fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
	if tempFilename != "" {
		ErrorLogFwd.Printf("  Response saved to: %s\n", tempFilename)
	}
}

// ForwardStoppedEvent forwards a stopped event to all forwarders
func (fm *ForwarderManager) ForwardStoppedEvent(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	fm.CancelPendingJobs(infoHash, peerID)
	request.Event = EventStopped
	fm.forwardEventToAll(infoHash, peerID, request)
}

// ForwardCompletedEvent forwards a completed event to all forwarders
func (fm *ForwarderManager) ForwardCompletedEvent(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	request.Event = EventCompleted
	fm.forwardEventToAll(infoHash, peerID, request)
}

// forwardEventToAll sends an event to all forwarders in parallel
func (fm *ForwarderManager) forwardEventToAll(infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	fm.forwardersMu.RLock()
	forwarders := make([]CoreCommon.Forward, len(fm.Forwarders))
	copy(forwarders, fm.Forwarders)
	fm.forwardersMu.RUnlock()

	done := make(chan struct{})
	go func() {
		for _, forwarder := range forwarders {
			f := forwarder
			go fm.executeEventAnnounce(f, infoHash, peerID, request)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second * time.Duration(fm.Config.ForwardTimeout)):
		if fm.Config.Debug {
			DebugLogFwd.Printf("Timeout waiting for %s event forwards to complete for %x\n", request.Event, infoHash)
		}
	}
}

// executeEventAnnounce executes a stopped/completed event announce
func (fm *ForwarderManager) executeEventAnnounce(forwarder CoreCommon.Forward, infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	hash := fmt.Sprintf("%x", infoHash)
	trackerURL := forwarder.Uri
	protocol := forwarder.GetProtocol()

	if fm.Config.Debug {
		DebugLogFwd.Printf("Forwarding %s %s event for %s (peer %x) to %s\n",
			protocol, request.Event, hash, peerID, trackerURL)
	}

	var err error
	if protocol == "udp" {
		_, _, err = fm.udpForwarder.Announce(forwarder, request)
	} else {
		err = fm.doHTTPEventRequest(forwarder, request)
	}

	if err != nil {
		ErrorLogFwd.Printf("Error forwarding %s event for %s to %s: %s\n",
			request.Event, hash, trackerURL, err.Error())
		return
	}

	if fm.Config.Debug {
		DebugLogFwd.Printf("Successfully forwarded %s event for %s to %s\n",
			request.Event, hash, trackerURL)
	}
}

// doHTTPEventRequest performs an HTTP request for event forwarding (no retries)
func (fm *ForwarderManager) doHTTPEventRequest(forwarder CoreCommon.Forward, request tracker.Request) error {
	uri := fm.buildAnnounceURI(forwarder, request, request.Event)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(fm.Config.ForwardTimeout))
	defer cancel()

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}

	if forwarder.Host != "" {
		rqst.Host = forwarder.Host
	}

	client := http.Client{}
	response, err := client.Do(rqst)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", response.StatusCode, response.Status)
	}

	_, _ = io.ReadAll(response.Body)
	return nil
}

// CancelPendingJobs cancels any pending announce jobs for a specific peer
func (fm *ForwarderManager) CancelPendingJobs(infoHash common.InfoHash, peerID common.PeerID) {
	fm.pendingMu.Lock()
	defer fm.pendingMu.Unlock()

	expectedPrefix := fmt.Sprintf("%x:", infoHash)
	expectedSuffix := fmt.Sprintf(":%x", peerID)

	keysToRemove := make([]string, 0)
	for key := range fm.pendingJobs {
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
