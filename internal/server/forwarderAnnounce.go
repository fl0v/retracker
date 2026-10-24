// forwarderAnnounce.go - Unified announce execution logic for forwarders.
// Routes to HTTP or UDP protocol handlers, processes results, handles retries.
// Key functions: executeAnnounce(), doHTTPAnnounce(), doUDPAnnounce(), handleAnnounceResult()
// Also handles event forwarding (ForwardStoppedEvent, ForwardCompletedEvent).
// HTTP announces and events share the manager's client and bounded body reads.
// Cache publication is session-guarded; logging and bookkeeping never hold the peer lock.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
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
	Deferred     bool // Retry scheduling or permanent rejection already handled.
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
			fm.Log().Debug(fmt.Sprintf("Forwarder %s is disabled; dropping job for %x", forwardName, job.InfoHash))
		}
		return
	}
	if fm.isSuspended(forwardName) {
		if fm.Config.Debug {
			fm.Log().Debug(fmt.Sprintf("Forwarder %s is suspended; dropping job for %x", forwardName, job.InfoHash))
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

// logAnnounceResult records protocol-independent outcome fields.
func (fm *ForwarderManager) logAnnounceResult(protocol, trackerURL, hash string, result AnnounceResult) {
	fields := []any{
		"protocol", protocol, "tracker", trackerURL, "info_hash", hash,
		"response_bytes", result.ResponseSize, "duration", result.Duration,
		"interval", result.Interval, "peers", len(result.Peers),
	}
	if result.Success {
		fm.Log().Info("Forwarder announce succeeded", fields...)
	} else {
		fm.Log().Warn("Forwarder announce failed", append(fields, "error", result.Error)...)
	}
}

// logRetryableAnnounceResult includes the failure count and disable threshold.
func (fm *ForwarderManager) logRetryableAnnounceResult(protocol, trackerURL, hash string, result AnnounceResult, failureCount, threshold int) {
	fm.Log().Warn("Forwarder announce will retry", "protocol", protocol, "tracker", trackerURL,
		"info_hash", hash, "duration", result.Duration, "error", result.Error,
		"failure_count", failureCount, "failure_threshold", threshold)
}

// handleAnnounceResult processes the result of an announce attempt
func (fm *ForwarderManager) handleAnnounceResult(job AnnounceJob, result AnnounceResult) {
	if result.Deferred || job.canceled() || fm.isStopping() {
		return
	}
	forwardName := job.Forwarder.GetName()
	trackerURL := job.Forwarder.Uri
	hash := fmt.Sprintf("%x", job.InfoHash)

	if result.Success {
		routablePeers, dropped := filterRoutableForwarderPeers(result.Peers)
		if dropped > 0 {
			fm.Log().Info(fmt.Sprintf("Filtered %d unroutable peer(s) from forwarder %s response\n", dropped, forwardName))
		}
		if len(result.Peers) > 0 && len(routablePeers) == 0 {
			result = buildFailureResult(result.Duration, fmt.Errorf("forwarder returned only unroutable peers (%d total)", len(result.Peers)))
		} else {
			result.Peers = routablePeers
		}
	}

	if result.Success {
		// Update storage with peers
		if !fm.updatePeerCache(job, func() { fm.Storage.UpdatePeers(job.InfoHash, forwardName, result.Peers, result.Interval) }) {
			return
		}
		if len(result.Peers) > 0 {
			fm.resetFailure(forwardName)
		}
		fm.recordStats(forwardName, result.Duration, result.Interval)

		if fm.Prometheus != nil && job.Forwarder.GetProtocol() == protocolUDP {
			fm.Prometheus.ForwarderStatus.WithLabelValues(forwardName, "200").Inc()
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
		if job.canceled() || fm.isStopping() {
			return
		}

		suspendFor := time.Duration(fm.Config.ForwarderSuspendSeconds) * time.Second
		if suspendFor <= 0 {
			suspendFor = 300 * time.Second
		}
		fm.suspendForwarderWithReason(forwardName, suspendFor, result.Error.Error())
		fm.Log().Info(fmt.Sprintf("Suspending forwarder %s for %v\n", forwardName, suspendFor))
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
		fm.Log().Error(fmt.Sprintf("  Duration: %v\n", result.Duration))
	}

	if job.canceled() || fm.isStopping() {
		return
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
	if !fm.isDisabled(forwardName) {
		fm.updatePeerCache(job, func() { fm.Storage.UpdatePeers(job.InfoHash, forwardName, nil, 60) })
	}
}

// updatePeerCache serializes only cache mutation with stopped/expired sessions.
// Callbacks must only mutate the cache; filtering, logging, metrics and other
// bookkeeping stay outside this global peer lock.
func (fm *ForwarderManager) updatePeerCache(job AnnounceJob, update func()) bool {
	if job.state != nil && job.state.peerContext != nil && fm.MainStorage != nil {
		fm.MainStorage.requestsMu.Lock()
		defer fm.MainStorage.requestsMu.Unlock()
	}
	if job.canceled() || fm.isStopping() {
		return false
	}
	update()
	return true
}

// doUDPAnnounce performs a UDP tracker announce
func (fm *ForwarderManager) doUDPAnnounce(job AnnounceJob) AnnounceResult {
	startTime := time.Now()
	forward := job.Forwarder

	if fm.Config.Debug && forward.Ip != "" {
		fm.Log().Debug(fmt.Sprintf("  Using IP: %s\n", forward.Ip))
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
		fm.Log().Debug(fmt.Sprintf("  Request URI: %s\n", uri))
		if forward.Ip != "" {
			fm.Log().Debug(fmt.Sprintf("  Using IP: %s\n", forward.Ip))
		}
		if forward.Host != "" {
			fm.Log().Debug(fmt.Sprintf("  Host header: %s\n", forward.Host))
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
		if result.Success || result.Deferred {
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
			fm.Log().Error(fmt.Sprintf("HTTP %d %s (retryable)\n", response.StatusCode, response.Status))
		}
		return result
	}

	payload, err := readHTTPAnnounceResponse(response)
	if err != nil {
		result := buildFailureResult(duration, fmt.Errorf("failed to read response: %w", err))
		if fm.Config.Debug && !isTimeoutErr(err) && !result.NonRetryable {
			fm.Log().Error(fmt.Sprintf("Failed to read response (%s): %v\n",
				map[bool]string{true: "non-retryable", false: "retryable"}[result.NonRetryable], err))
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
			return AnnounceResult{Deferred: true, Duration: duration}
		}
		if fm.Config.Debug {
			fm.Log().Error(fmt.Sprintf("Tracker failure response: %s\n", bitResponse.FailureReason))
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
	uri, err := url.Parse(forward.Uri)
	if err != nil {
		return forward.Uri
	} // Request construction reports malformed URLs.
	query := uri.Query()
	query.Set("info_hash", string(request.InfoHash))
	query.Set("peer_id", string(request.PeerID))
	query.Set("port", strconv.Itoa(request.Port))
	query.Set("uploaded", strconv.FormatUint(request.Uploaded, 10))
	query.Set("downloaded", strconv.FormatUint(request.Downloaded, 10))
	query.Set("left", strconv.FormatUint(request.Left, 10))
	query.Del("event")
	if event != "" {
		query.Set("event", event)
	}
	if forward.Ip != "" {
		query.Set("ip", forward.Ip)
		query.Set("ipv4", forward.Ip)
	}
	uri.RawQuery = query.Encode()
	return uri.String()
}

// logParseError logs details about a parse failure
func (fm *ForwarderManager) logParseError(payload []byte, hash, uri string, err error) {
	fm.Log().Error(fmt.Sprintf("Failed to parse response: %v\n", err))

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
		fm.Log().Error(fmt.Sprintf("  Raw response data: %s\n", payloadStr))
	} else {
		fm.Log().Error(fmt.Sprintf("  Response is binary (%d bytes)\n", len(payload)))
	}

	tempFilename := fm.TempStorage.SaveBencodeFromForwarder(payload, hash, uri)
	if tempFilename != "" {
		fm.Log().Error(fmt.Sprintf("  Response saved to: %s\n", tempFilename))
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
			fm.Log().Debug(fmt.Sprintf("Timeout waiting for %s event forwards to complete for %x\n", request.Event, infoHash))
		}
	}
}

// executeEventAnnounce executes a stopped/completed event announce
func (fm *ForwarderManager) executeEventAnnounce(forwarder CoreCommon.Forward, infoHash common.InfoHash, peerID common.PeerID, request tracker.Request) {
	hash := fmt.Sprintf("%x", infoHash)
	trackerURL := forwarder.Uri
	protocol := forwarder.GetProtocol()

	if fm.Config.Debug {
		fm.Log().Debug(fmt.Sprintf("Forwarding %s %s event for %s (peer %x) to %s\n",
			protocol, request.Event, hash, peerID, trackerURL))
	}

	var err error
	if protocol == protocolUDP {
		_, _, err = fm.udpForwarder.Announce(forwarder, request)
	} else {
		err = fm.doHTTPEventRequest(forwarder, request)
	}

	if err != nil {
		fm.Log().Error(fmt.Sprintf("Error forwarding %s event for %s to %s: %s\n",
			request.Event, hash, trackerURL, err.Error()))
		return
	}

	if fm.Config.Debug {
		fm.Log().Debug(fmt.Sprintf("Successfully forwarded %s event for %s to %s\n",
			request.Event, hash, trackerURL))
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
	fm.cancelPendingJobs(infoHash, peerID, nil)
}

func (fm *ForwarderManager) cancelPendingJobs(infoHash common.InfoHash, peerID common.PeerID, session context.Context) {
	fm.pendingMu.Lock()

	expectedPrefix := fmt.Sprintf("%x:", infoHash)
	expectedSuffix := fmt.Sprintf(":%x", peerID)

	keysToRemove := make([]string, 0)
	for key, state := range fm.pendingJobs {
		if session != nil && (state == nil || state.peerContext != session) {
			continue
		}
		if len(key) > len(expectedPrefix)+len(expectedSuffix) {
			if key[:len(expectedPrefix)] == expectedPrefix && key[len(key)-len(expectedSuffix):] == expectedSuffix {
				keysToRemove = append(keysToRemove, key)
			}
		}
	}

	for _, key := range keysToRemove {
		delete(fm.pendingJobs, key)
	}
	fm.scheduledJobsMu.Lock()
	for scheduledAt, jobs := range fm.scheduledJobs {
		kept := jobs[:0]
		for _, job := range jobs {
			if job.InfoHash != infoHash || job.PeerID != peerID || (session != nil && job.context() != session) {
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
	fm.scheduledJobsMu.Unlock()
	fm.pendingMu.Unlock()
	if len(keysToRemove) > 0 && fm.Config.Debug {
		fm.Log().Debug("Canceled pending forwarding jobs", "peer_id", fmt.Sprintf("%x", peerID), "count", len(keysToRemove))
	}
}
