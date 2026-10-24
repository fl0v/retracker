// forwarderAnnounce.go - Unified announce execution logic for forwarders.
// Routes to HTTP or UDP protocol handlers, processes results, handles retries.
// Key functions: executeAnnounce(), doHTTPAnnounce(), doUDPAnnounce(), handleAnnounceResult()
// Also handles event forwarding (ForwardStoppedEvent, ForwardCompletedEvent).
// HTTP announces and events share the manager's client and bounded body reads.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	protocolUDP          = "udp"
	protocolHTTP         = "http"
	protocolHTTPS        = "https"
	forwarderNameLabel   = "name"
	forwarderStatusLabel = "status"
)

var (
	cgnatPrefix    = netip.MustParsePrefix("100.64.0.0/10")
	docV4Prefix1   = netip.MustParsePrefix("192.0.2.0/24")
	docV4Prefix2   = netip.MustParsePrefix("198.51.100.0/24")
	docV4Prefix3   = netip.MustParsePrefix("203.0.113.0/24")
	benchmarkV4Net = netip.MustParsePrefix("198.18.0.0/15")
	docV6Prefix    = netip.MustParsePrefix("2001:db8::/32")
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

func buildFailureResult(duration time.Duration, err error) AnnounceResult {
	classified := classifyForwarderError(err)
	if classified == nil {
		classified = &ForwarderError{
			Type:    ForwarderErrorRetryable,
			Message: "unknown forwarder error",
			Cause:   err,
		}
	}

	return AnnounceResult{
		Success:      false,
		Duration:     duration,
		Error:        classified,
		NonRetryable: classified.IsPermanent(),
		Suspend:      classified.ShouldSuspend(),
	}
}

func filterRoutableForwarderPeers(peers []common.Peer) ([]common.Peer, int) {
	if len(peers) == 0 {
		return peers, 0
	}

	filtered := make([]common.Peer, 0, len(peers))
	dropped := 0
	for _, peer := range peers {
		if isRoutablePeerAddress(peer.IP) {
			filtered = append(filtered, peer)
			continue
		}
		dropped++
	}
	return filtered, dropped
}

func isRoutablePeerAddress(address common.Address) bool {
	addr, err := netip.ParseAddr(address.String())
	if err != nil || !addr.IsValid() {
		return false
	}

	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsPrivate() {
		return false
	}

	if addr.Is4() {
		if cgnatPrefix.Contains(addr) || docV4Prefix1.Contains(addr) ||
			docV4Prefix2.Contains(addr) || docV4Prefix3.Contains(addr) ||
			benchmarkV4Net.Contains(addr) {
			return false
		}

		octets := addr.As4()
		if octets[0] == 0 || octets[0] >= 240 {
			return false
		}
	}

	if addr.Is6() && docV6Prefix.Contains(addr) {
		return false
	}

	return true
}

// executeAnnounce routes to the appropriate protocol handler
func (fm *ForwarderManager) executeAnnounce(job AnnounceJob) {
	if job.canceled() || fm.isStopping() {
		return
	}
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

	if protocol == protocolUDP {
		result = fm.doUDPAnnounce(job)
	} else {
		result = fm.doHTTPAnnounce(job)
	}

	fm.handleAnnounceResult(job, result)
}

// logAnnounceResult logs the outcome of an announce attempt in a unified format
func (fm *ForwarderManager) logAnnounceResult(protocol, trackerURL string, hash string, result AnnounceResult) {
	// Extract host:port from URL
	u, err := url.Parse(trackerURL)
	if err != nil {
		// Fallback to full URL if parsing fails
		if result.Success {
			secs := result.Duration.Seconds()
			fmt.Printf("%s %s %s\n    --> (%d bytes, %.3fs, interval=%d, peers=%d)\n",
				hash, protocol, trackerURL, result.ResponseSize, secs, result.Interval, len(result.Peers))
		} else {
			WarnLogFwd.Printf("%s %s %s\n    --> (error: %s)\n",
				hash, protocol, trackerURL, result.Error.Error())
		}
		return
	}

	// Format host:port
	hostPort := u.Host
	// Check if port is already included using u.Port() to correctly handle IPv6 addresses
	if u.Port() == "" {
		// Add default port if not specified
		switch u.Scheme {
		case protocolHTTPS:
			hostPort += ":443"
		case protocolHTTP:
			hostPort += ":80"
		case protocolUDP:
			// UDP URLs must have an explicit port - if missing, use hostname only
			// (this should not happen if validation is working, but handle gracefully)
			hostPort = u.Hostname()
		}
	}

	// Log based on result
	if result.Success {
		secs := result.Duration.Seconds()
		fmt.Printf("%s %s %s\n    --> (%d bytes, %.3fs, interval=%d, peers=%d)\n",
			hash, protocol, hostPort, result.ResponseSize, secs, result.Interval, len(result.Peers))
	} else {
		WarnLogFwd.Printf("%s %s %s\n    --> (error: %s)\n",
			hash, protocol, hostPort, result.Error.Error())
	}
}

// logRetryableAnnounceResult logs retryable announce failures with failure-count context in one warning.
func (fm *ForwarderManager) logRetryableAnnounceResult(protocol, trackerURL, hash string, result AnnounceResult, failureCount, threshold int) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		if threshold > 0 {
			WarnLogFwd.Printf("%s %s %s [%d/%d]\n    --> (error: %s)\n",
				hash, protocol, trackerURL, failureCount, threshold, result.Error.Error())
		} else {
			WarnLogFwd.Printf("%s %s %s [%d]\n    --> (error: %s)\n",
				hash, protocol, trackerURL, failureCount, result.Error.Error())
		}
		return
	}

	hostPort := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case protocolHTTPS:
			hostPort += ":443"
		case protocolHTTP:
			hostPort += ":80"
		case protocolUDP:
			hostPort = u.Hostname()
		}
	}

	if threshold > 0 {
		WarnLogFwd.Printf("%s %s %s [%d/%d]\n    --> (error: %s)\n",
			hash, protocol, hostPort, failureCount, threshold, result.Error.Error())
	} else {
		WarnLogFwd.Printf("%s %s %s [%d]\n    --> (error: %s)\n",
			hash, protocol, hostPort, failureCount, result.Error.Error())
	}
}

// handleAnnounceResult processes the result of an announce attempt
func (fm *ForwarderManager) handleAnnounceResult(job AnnounceJob, result AnnounceResult) {
	if job.state != nil && job.state.peerContext != nil && fm.MainStorage != nil {
		fm.MainStorage.requestsMu.Lock()
		defer fm.MainStorage.requestsMu.Unlock()
	}
	if job.canceled() || fm.isStopping() {
		return
	}
	forwardName := job.Forwarder.GetName()
	trackerURL := job.Forwarder.Uri
	hash := fmt.Sprintf("%x", job.InfoHash)

	if result.Success {
		routablePeers, dropped := filterRoutableForwarderPeers(result.Peers)
		if dropped > 0 {
			InfoLogFwd.Printf("Filtered %d unroutable peer(s) from forwarder %s response\n", dropped, forwardName)
		}
		if len(result.Peers) > 0 && len(routablePeers) == 0 {
			result = buildFailureResult(result.Duration, fmt.Errorf("forwarder returned only unroutable peers (%d total)", len(result.Peers)))
		} else {
			result.Peers = routablePeers
		}
	}

	if result.Success {
		// Update storage with peers
		fm.Storage.UpdatePeers(job.InfoHash, forwardName, result.Peers, result.Interval)
		if len(result.Peers) > 0 {
			fm.resetFailure(forwardName)
		}
		fm.recordStats(forwardName, result.Duration, result.Interval)

		if fm.Prometheus != nil {
			fm.Prometheus.ForwarderStatus.With(prometheus.Labels{forwarderNameLabel: forwardName, forwarderStatusLabel: `200`}).Inc()
		}

		if fm.Config.Debug {
			protocol := job.Forwarder.GetProtocol()
			fm.logAnnounceResult(protocol, trackerURL, hash, result)
		}
		return
	}

	// Handle failure
	if result.Suspend {
		protocol := job.Forwarder.GetProtocol()
		fm.logAnnounceResult(protocol, trackerURL, hash, result)

		suspendFor := time.Duration(fm.Config.ForwarderSuspendSeconds) * time.Second
		if suspendFor <= 0 {
			suspendFor = 300 * time.Second
		}
		fm.suspendForwarderWithReason(forwardName, suspendFor, result.Error.Error())
		InfoLogFwd.Printf("Suspending forwarder %s for %v\n", forwardName, suspendFor)
		return
	}

	protocol := job.Forwarder.GetProtocol()
	if result.NonRetryable {
		fm.logAnnounceResult(protocol, trackerURL, hash, result)
	} else {
		currentFailures := fm.getFailureCount(forwardName)
		nextFailures := currentFailures + 1
		threshold := fm.Config.ForwarderFailThreshold
		fm.logRetryableAnnounceResult(protocol, trackerURL, hash, result, nextFailures, threshold)
	}
	if fm.Config.Debug {
		ErrorLogFwd.Printf("  Duration: %v\n", result.Duration)
	}

	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{forwarderNameLabel: forwardName, forwarderStatusLabel: `error`}).Inc()
	}

	if result.NonRetryable {
		fm.disableForwarder(forwardName, disableReasonForError(result.Error))
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

	if fm.Config.Debug && forward.Ip != "" {
		DebugLogFwd.Printf("  Using IP: %s\n", forward.Ip)
	}

	bitResponse, respBytes, err := fm.udpForwarder.AnnounceContext(job.context(), forward, job.Request)
	duration := time.Since(startTime)

	if err != nil {
		return buildFailureResult(duration, err)
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

	uri := fm.buildAnnounceURI(forward, request, "")

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
		if fm.isStopping() {
			return buildFailureResult(time.Since(startTime), context.Canceled)
		}
		if job.canceled() {
			return buildFailureResult(time.Since(startTime), job.context().Err())
		}
		if retry > 0 {
			timer := time.NewTimer(backoff * time.Duration(1<<retry))
			select {
			case <-timer.C:
			case <-fm.stopChan:
				timer.Stop()
				return buildFailureResult(time.Since(startTime), context.Canceled)
			case <-job.context().Done():
				timer.Stop()
				return buildFailureResult(time.Since(startTime), job.context().Err())
			}
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
	if lastErr == nil {
		lastErr = fmt.Errorf("forwarder announce failed after %d attempt(s)", attempts)
	}
	return buildFailureResult(time.Since(startTime), lastErr)
}

// doSingleHTTPRequest performs a single HTTP request attempt
func (fm *ForwarderManager) doSingleHTTPRequest(job AnnounceJob, uri string, startTime time.Time) AnnounceResult {
	forward := job.Forwarder
	forwardName := forward.GetName()
	hash := fmt.Sprintf("%x", job.InfoHash)
	trackerURL := forward.Uri

	ctx, cancel := fm.requestContext(job.context())
	defer cancel()

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return buildFailureResult(time.Since(startTime), err)
	}

	if forward.Host != "" {
		rqst.Host = forward.Host
	}

	response, err := fm.httpClient.Do(rqst)
	duration := time.Since(startTime)

	if err != nil {
		return buildFailureResult(duration, err)
	}
	defer response.Body.Close()

	if fm.Prometheus != nil {
		fm.Prometheus.ForwarderStatus.With(prometheus.Labels{
			forwarderNameLabel:   forwardName,
			forwarderStatusLabel: fmt.Sprintf("%d", response.StatusCode),
		}).Inc()
	}

	if response.StatusCode != http.StatusOK {
		discardHTTPResponseBody(response.Body)

		statusErr := fmt.Errorf("HTTP status %d: %s", response.StatusCode, response.Status)
		if isTrackerRejection(response.StatusCode) {
			statusErr = fmt.Errorf("HTTP status %d (tracker rejection)", response.StatusCode)
		}

		result := buildFailureResult(duration, statusErr)
		if fm.Config.Debug && !result.NonRetryable && !result.Suspend {
			ErrorLogFwd.Printf("HTTP %d %s (retryable)\n", response.StatusCode, response.Status)
		}
		return result
	}

	payload, err := readHTTPAnnounceResponse(response)
	if err != nil {
		result := buildFailureResult(duration, fmt.Errorf("failed to read response: %w", err))
		if fm.Config.Debug && !isTimeoutErr(err) && !result.NonRetryable {
			ErrorLogFwd.Printf("Failed to read response (%s): %v\n",
				map[bool]string{true: "non-retryable", false: "retryable"}[result.NonRetryable], err)
		}
		return result
	}

	if fm.Config.Debug {
		fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
	}

	bitResponse, err := Response.Load(payload)
	if err != nil {
		if fm.Config.Debug {
			fm.logParseError(payload, hash, uri, err)
		}
		return buildFailureResult(duration, fmt.Errorf("failed to parse response: %w", err))
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
		return buildFailureResult(duration, fmt.Errorf("tracker failure: %s", bitResponse.FailureReason))
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
	if !fm.tasks.run(func() {
		for _, forwarder := range forwarders {
			f := forwarder
			fm.tasks.run(func() { fm.executeEventAnnounce(f, infoHash, peerID, request) })
		}
		close(done)
	}) {
		return
	}

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
	if protocol == protocolUDP {
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

	ctx, cancel := fm.requestContext(context.Background())
	defer cancel()

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}

	if forwarder.Host != "" {
		rqst.Host = forwarder.Host
	}

	response, err := fm.httpClient.Do(rqst)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	discardHTTPResponseBody(response.Body)

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", response.StatusCode, response.Status)
	}

	return nil
}

// CancelPendingJobs removes pending markers and scheduled retries for a peer.
// Storage ends the peer session, invalidating queued and in-flight jobs too.
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

	fm.scheduledJobsMu.Lock()
	defer fm.scheduledJobsMu.Unlock()
	for scheduledAt, jobs := range fm.scheduledJobs {
		kept := jobs[:0]
		for _, job := range jobs {
			if job.InfoHash != infoHash || job.PeerID != peerID {
				kept = append(kept, job)
			}
		}
		clear(jobs[len(kept):])
		if len(kept) == 0 {
			delete(fm.scheduledJobs, scheduledAt)
		} else {
			fm.scheduledJobs[scheduledAt] = kept
		}
	}
}
