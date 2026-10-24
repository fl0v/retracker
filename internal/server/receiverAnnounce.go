// receiverAnnounce.go - HTTP announce request handler.
// Processes BitTorrent announce requests, manages peer storage updates,
// triggers forwarder announces, and builds responses with aggregated peers.
// Key function: getPeersForResponse() collects peers and counts seeders/leechers.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

type ReceiverAnnounce struct {
	observability.Logging
	Config           *config.Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Prometheus       *observability.Prometheus
	TempStorage      *TempStorage
	peerFamily       int           // Zero for HTTP; UDP selects 4 or 6 before limiting peers.
	forwarderMu      *sync.RWMutex // Shared with Core for runtime initialization.
}

func (ra *ReceiverAnnounce) HTTPHandler(w http.ResponseWriter, r *http.Request) {
	if ra.Prometheus != nil {
		ra.Prometheus.Requests.Inc()
	}
	xrealip := r.Header.Get(`X-Real-IP`)
	ra.Log().Debug("HTTP announce", "method", r.Method, "remote_addr", r.RemoteAddr, "x_real_ip", xrealip, "uri", r.RequestURI, "user_agent", r.UserAgent())
	remoteAddr := ra.getRemoteAddr(r, xrealip)
	remotePort := r.URL.Query().Get(`port`)
	infoHash := r.URL.Query().Get(`info_hash`)
	if ra.Config.Debug {
		ra.Log().Debug(fmt.Sprintf("hash: '%x', remote addr: %s:%s", infoHash, remoteAddr, remotePort))
	}
	compactFlag := r.URL.Query().Get(`compact`)
	noPeerIDFlag := r.URL.Query().Get(`no_peer_id`)
	response, failure := ra.ProcessAnnounce(
		remoteAddr,
		infoHash,
		r.URL.Query().Get(`peer_id`),
		remotePort,
		r.URL.Query().Get(`uploaded`),
		r.URL.Query().Get(`downloaded`),
		r.URL.Query().Get(`left`),
		r.URL.Query().Get(`ip`),
		r.URL.Query().Get(`numwant`),
		r.URL.Query().Get(`event`),
		r.UserAgent(),
		compactFlag,
		noPeerIDFlag,
	)
	if failure != `` {
		ra.Log().Error("Announce failed", "info_hash", fmt.Sprintf("%x", infoHash), "peer_id", fmt.Sprintf("%x", r.URL.Query().Get("peer_id")), "ip", remoteAddr, "port", remotePort, "event", r.URL.Query().Get("event"), "numwant", r.URL.Query().Get("numwant"), "compact", compactFlag, "no_peer_id", noPeerIDFlag, "user_agent", r.UserAgent(), "error", failure)
		w.Header().Set(`Content-Type`, `text/plain; charset=utf-8`)
		w.WriteHeader(http.StatusBadRequest)
		errResp := Response.NewFailure(failure)
		if encoded, err := errResp.Bencode(false); err == nil {
			fmt.Fprint(w, encoded)
		}
		return
	}
	if response == nil {
		w.Header().Set(`Content-Type`, `text/plain; charset=utf-8`)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `d14:failure reason24:internal tracker errore`)
		return
	}
	compacted := r.URL.Query().Get(`compact`) == `1`
	w.Header().Set(`Content-Type`, `text/plain; charset=utf-8`)
	d, err := response.Bencode(compacted)
	if err != nil {
		ra.Log().Error("Encode announce response", "error", err)
		return
	}
	fmt.Fprint(w, d)
}

func (ra *ReceiverAnnounce) getRemoteAddr(r *http.Request, xrealip string) string {
	if ra.Config.XRealIP && xrealip != `` {
		return xrealip
	}
	return ra.parseRemoteAddr(r.RemoteAddr, `127.0.0.1`)
}

func (ra *ReceiverAnnounce) parseRemoteAddr(in, def string) string {
	address, _, err := net.SplitHostPort(in)
	if err != nil {
		return def
	}
	return address
}

func (ra *ReceiverAnnounce) ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant, event, userAgent, compactFlag, noPeerIDFlag string) (*Response.Response, string) {
	if ra.forwarderMu != nil {
		ra.forwarderMu.RLock()
		defer ra.forwarderMu.RUnlock()
	}
	request, err := tracker.MakeRequest(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
		event, userAgent, compactFlag, noPeerIDFlag, slog.NewLogLogger(ra.Log().Handler(), slog.LevelWarn))
	if err != nil {
		return nil, err.Error()
	}

	response := Response.Response{}

	switch request.Event {
	case EventStopped:
		ra.handleStoppedEvent(request, &response)
	case EventCompleted:
		ra.handleCompletedEvent(request, &response)
	default:
		ra.handleRegularAnnounce(request, &response)
	}

	if response.Interval == 0 {
		response.Interval = ra.Config.AnnounceInterval
	}
	if response.FailureReason == "" {
		response.Interval = ra.clampInterval(response.Interval)
		response.MinInterval = ra.Config.AnnounceInterval
	} else {
		response.MinInterval = response.Interval
	}

	// Get peers, seeders, and leechers from both local and forwarder storage
	seeders, leechers, peers := ra.getPeersForResponse(request.InfoHash)
	response.Complete = seeders
	response.Incomplete = leechers
	response.Peers = peers

	if trackerID := ra.Config.TrackerID; trackerID != "" {
		response.TrackerID = trackerID
	}

	response.Peers = ra.filterPeers(response.Peers, request.Peer(), request.NumWant)

	return &response, ""
}

// handleStoppedEvent removes the peer, cancels its jobs, and cleans an unused cache.
func (ra *ReceiverAnnounce) handleStoppedEvent(request *tracker.Request, response *Response.Response) {
	// Serialize session cancellation and cache cleanup with result publication.
	ra.Storage.requestsMu.Lock()
	session := ra.Storage.peerSessions[peerKey{request.InfoHash, request.PeerID}].ctx
	ra.Storage.deleteLocked(request.InfoHash, request.PeerID)
	if ra.ForwarderStorage != nil && len(ra.Storage.Requests[request.InfoHash]) == 0 {
		ra.ForwarderStorage.Cleanup(request.InfoHash)
	}
	ra.Storage.requestsMu.Unlock()
	// Match the canceled session so a concurrent restart keeps its new jobs.
	if ra.ForwarderManager != nil && session != nil {
		ra.ForwarderManager.cancelPendingJobs(request.InfoHash, request.PeerID, session)
	}

	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)
}

// handleCompletedEvent updates storage and continues the normal response flow.
func (ra *ReceiverAnnounce) handleCompletedEvent(request *tracker.Request, response *Response.Response) {
	// Update storage with completed status
	ra.Storage.Update(*request)

	// Get peers for response
	_, _, peers := ra.getPeersForResponse(request.InfoHash)
	response.Peers = peers

	// Use general interval setting from config
	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)
}

// handleRegularAnnounce processes regular announces (started event or empty event)
func (ra *ReceiverAnnounce) handleRegularAnnounce(request *tracker.Request, response *Response.Response) {
	// Update storage
	peerContext := ra.Storage.Update(*request)

	// Check if this is first announce for this info_hash
	if ra.isFirstAnnounce(request.InfoHash) {
		ra.handleFirstAnnounce(request, response, peerContext)
	} else {
		ra.handleSubsequentAnnounce(request, response, peerContext)
	}
}

// handleFirstAnnounce handles the first announce for an info_hash
func (ra *ReceiverAnnounce) handleFirstAnnounce(request *tracker.Request, response *Response.Response, peerContext context.Context) {
	// First announce: return configured interval and trigger parallel forwarder announces
	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)

	// Peers already collected in handleRegularAnnounce via getPeersForResponse()
	// No need to collect again here

	// Trigger parallel decoupled announces to all forwarders
	if ra.ForwarderManager != nil {
		if ra.ForwarderManager.shouldRateLimitInitial() && !ra.ForwarderManager.rateLimiter.allow() {
			atomic.AddUint64(&ra.ForwarderManager.rateLimitedCount, 1)
			if ra.ForwarderManager.Prometheus != nil {
				ra.ForwarderManager.Prometheus.RateLimited.Inc()
			}
			// If rate limiting is enabled, use retry period (bypasses clampInterval minimum)
			retrySeconds := ra.Config.RetryPeriod
			if retrySeconds <= 0 {
				retrySeconds = 300 // Default to 5 minutes if not configured
			}
			response.FailureReason = fmt.Sprintf("tracker busy, retry in %d seconds", retrySeconds)
			response.Interval = retrySeconds
			return
		}
		// Check if queue is full before attempting to queue jobs
		if ra.ForwarderManager.isQueueFull() {
			atomic.AddUint64(&ra.ForwarderManager.droppedFullCount, 1)
			if ra.ForwarderManager.Prometheus != nil {
				ra.ForwarderManager.Prometheus.DroppedFull.Inc()
			}
			// Queue is full, reject with retry using configured interval (default 30 minutes)
			response.FailureReason = "tracker queue full, retry in 30 minutes"
			response.Interval = ra.Config.AnnounceInterval
			return
		}
		ra.ForwarderManager.queueEligibleAnnounces(request.InfoHash, *request, peerContext)
	} else if len(ra.Config.Forwards) > 0 {
		ra.Log().Error(fmt.Sprintf("Forwarders configured (%d) but forwarder manager is nil; cannot forward initial announce for %x", len(ra.Config.Forwards), request.InfoHash))
	}
}

// handleSubsequentAnnounce handles subsequent announces for an info_hash
func (ra *ReceiverAnnounce) handleSubsequentAnnounce(request *tracker.Request, response *Response.Response, peerContext context.Context) {
	// Peers already collected in handleRegularAnnounce via getPeersForResponse()
	// No need to collect again here

	// Use general interval setting from config
	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)

	// For regular announces, forward immediately if due or not yet contacted
	if ra.ForwarderManager != nil {
		ra.ForwarderManager.queueEligibleAnnounces(request.InfoHash, *request, peerContext)
	}
}

// getPeersForResponse collects peers from both local storage and forwarder storage
// and counts seeders and leechers. Returns (seeders, leechers, peers)
func (ra *ReceiverAnnounce) getPeersForResponse(infoHash common.InfoHash) (int, int, []common.Peer) {
	seeders := 0
	leechers := 0
	seenIPs := make(map[string]struct{})

	// Get local and forwarder peers first to estimate capacity
	var localPeers []common.Peer
	var forwarderPeers []common.Peer

	if ra.Storage != nil {
		localPeers = ra.Storage.GetPeers(infoHash)
	}
	if ra.ForwarderStorage != nil {
		forwarderPeers = ra.ForwarderStorage.GetAllPeers(infoHash)
	}

	// Pre-allocate slice with estimated capacity to avoid reallocations
	estimatedCapacity := len(localPeers) + len(forwarderPeers)
	peers := make([]common.Peer, 0, estimatedCapacity)

	// Collect and count local peers (unique by IP)
	if ra.Storage != nil {
		peers = append(peers, localPeers...)

		ra.Storage.requestsMu.Lock()
		if requestInfoHash, found := ra.Storage.Requests[infoHash]; found {
			for _, peerRequest := range requestInfoHash {
				ipStr := string(peerRequest.Peer().IP)
				if ipStr != "" {
					if _, exists := seenIPs[ipStr]; !exists {
						seenIPs[ipStr] = struct{}{}
						if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
							seeders++
						} else {
							leechers++
						}
					}
				}
			}
		}
		ra.Storage.requestsMu.Unlock()
	}

	// Collect and count forwarder peers (unique by IP, counted as seeders since we lack state)
	if ra.ForwarderStorage != nil {
		peers = append(peers, forwarderPeers...)

		for _, peer := range forwarderPeers {
			ipStr := string(peer.IP)
			if ipStr != "" {
				if _, exists := seenIPs[ipStr]; !exists {
					seenIPs[ipStr] = struct{}{}
					seeders++ // Forwarder peers are counted as seeders (we don't have state info)
				}
			}
		}
	}

	return seeders, leechers, peers
}

// isFirstAnnounce checks if this is the first announce for the given info_hash
func (ra *ReceiverAnnounce) isFirstAnnounce(infoHash common.InfoHash) bool {
	if ra.ForwarderStorage != nil {
		return !ra.ForwarderStorage.HasInfoHash(infoHash)
	}
	return false
}

func (ra *ReceiverAnnounce) clampInterval(interval int) int {
	if interval <= 0 {
		return ra.Config.AnnounceInterval
	}
	// Ensure minimum is AnnounceInterval (default 30 minutes)
	if interval < ra.Config.AnnounceInterval {
		return ra.Config.AnnounceInterval
	}
	// No maximum clamping - allow longer intervals from forwarders
	return interval
}

func (ra *ReceiverAnnounce) filterPeers(peers []common.Peer, requester common.Peer, numWant uint64) []common.Peer {
	if len(peers) == 0 {
		return peers
	}

	maxPeers := numWant
	if maxPeers == 0 {
		maxPeers = tracker.DefaultNumWant
	}

	seen := make(map[string]struct{}, len(peers))
	filtered := make([]common.Peer, 0, len(peers))

	for _, peer := range peers {
		if peer.PeerID == requester.PeerID {
			continue
		}
		if peer.IP == requester.IP && peer.Port == requester.Port {
			continue
		}

		if ra.peerFamily != 0 {
			ip := net.ParseIP(string(peer.IP))
			if ip == nil || (ip.To4() != nil) != (ra.peerFamily == 4) {
				continue
			}
		}
		key := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		filtered = append(filtered, peer)
		if uint64(len(filtered)) >= maxPeers {
			break
		}
	}

	return filtered
}

func NewReceiverAnnounce(cfg *config.Config, storage *Storage, forwarderStorage *ForwarderStorage, forwarderManager *ForwarderManager, loggers ...*slog.Logger) *ReceiverAnnounce {
	announce := ReceiverAnnounce{
		Logging:          observability.NewLogging(cfg.Debug, loggers...),
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: forwarderManager,
	}
	return &announce
}
