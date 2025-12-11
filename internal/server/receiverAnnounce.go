package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

var (
	DebugLogAnnounce = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogAnnounce = log.New(os.Stderr, `error#`, log.Lshortfile)
	remoteAddrRegexp = regexp.MustCompile(`(.*):\d+$`)
)

type ReceiverAnnounce struct {
	Config           *config.Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Prometheus       *observability.Prometheus
	TempStorage      *TempStorage
}

func (ra *ReceiverAnnounce) HTTPHandler(w http.ResponseWriter, r *http.Request) {
	if ra.Prometheus != nil {
		ra.Prometheus.Requests.Inc()
	}
	xrealip := r.Header.Get(`X-Real-IP`)
	DebugLogAnnounce.Printf("%s %s %s '%s' '%s'\n", r.Method, r.RemoteAddr, xrealip, r.RequestURI, r.UserAgent())
	remoteAddr := ra.getRemoteAddr(r, xrealip)
	remotePort := r.URL.Query().Get(`port`)
	infoHash := r.URL.Query().Get(`info_hash`)
	if ra.Config.Debug {
		DebugLogAnnounce.Printf("hash: '%x', remote addr: %s:%s", infoHash, remoteAddr, remotePort)
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
		ErrorLogAnnounce.Printf(
			"announce failure hash=%x peer=%x ip=%s port=%s event=%s numwant=%s compact=%s no_peer_id=%s ua=%q err=%s",
			infoHash,
			r.URL.Query().Get(`peer_id`),
			remoteAddr,
			remotePort,
			r.URL.Query().Get(`event`),
			r.URL.Query().Get(`numwant`),
			compactFlag,
			noPeerIDFlag,
			r.UserAgent(),
			failure,
		)
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
	compacted := false
	if r.URL.Query().Get(`compact`) == `1` {
		compacted = true
	}
	w.Header().Set(`Content-Type`, `text/plain; charset=utf-8`)
	d, err := response.Bencode(compacted)
	if err != nil {
		ErrorLogAnnounce.Println(err.Error())
		return
	}
	fmt.Fprint(w, d)
	/*
		if ra.Config.Debug {
			DebugLog.Printf("Bencode: %s\n", d)
		}
	*/
}

func (ra *ReceiverAnnounce) getRemoteAddr(r *http.Request, xrealip string) string {
	if ra.Config.XRealIP && xrealip != `` {
		return xrealip
	}
	return ra.parseRemoteAddr(r.RemoteAddr, `127.0.0.1`)
}

func (ra *ReceiverAnnounce) parseRemoteAddr(in, def string) string {
	address := def
	if match := remoteAddrRegexp.FindStringSubmatch(in); len(match) == 2 {
		address = match[1]
	}
	return address
}

func (ra *ReceiverAnnounce) ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant, event, userAgent, compactFlag, noPeerIDFlag string) (*Response.Response, string) {
	request, err := tracker.MakeRequest(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
		event, userAgent, compactFlag, noPeerIDFlag, DebugLog)
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
	// Clamp interval to ensure minimum is AnnounceInterval
	response.Interval = ra.clampInterval(response.Interval)
	response.MinInterval = ra.Config.AnnounceInterval

	seeders, leechers := ra.countLocalPeers(request.InfoHash)
	response.Complete = seeders
	response.Incomplete = leechers

	if trackerID := ra.Config.TrackerID; trackerID != "" {
		response.TrackerID = trackerID
	}

	response.Peers = ra.filterPeers(response.Peers, request.Peer(), request.NumWant)

	return &response, ""
}

// handleStoppedEvent processes a stopped event: forwards to forwarders, cancels pending jobs, and deletes the peer
func (ra *ReceiverAnnounce) handleStoppedEvent(request *tracker.Request, response *Response.Response) {
	// Delete peer from storage
	ra.Storage.Delete(*request)

	// Clean up forwarder storage when no local peers remain
	if ra.ForwarderStorage != nil {
		localPeers := ra.Storage.GetPeers(request.InfoHash)
		if len(localPeers) == 0 {
			ra.ForwarderStorage.Cleanup(request.InfoHash)
		}
	}

	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)
}

// handleCompletedEvent processes a completed event: updates storage, forwards event, but continues normal flow
func (ra *ReceiverAnnounce) handleCompletedEvent(request *tracker.Request, response *Response.Response) {
	// Update storage with completed status
	ra.Storage.Update(*request)

	// Get peers for response
	response.Peers = ra.getPeersForResponse(request.InfoHash)

	// Use general interval setting from config
	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)
}

// handleRegularAnnounce processes regular announces (started event or empty event)
func (ra *ReceiverAnnounce) handleRegularAnnounce(request *tracker.Request, response *Response.Response) {
	// Update storage
	ra.Storage.Update(*request)

	// Get peers from both local and forwarder storage
	response.Peers = ra.getPeersForResponse(request.InfoHash)

	// Check if this is first announce for this info_hash
	if ra.isFirstAnnounce(request.InfoHash) {
		ra.handleFirstAnnounce(request, response)
	} else {
		ra.handleSubsequentAnnounce(request, response)
	}
}

// handleFirstAnnounce handles the first announce for an info_hash
func (ra *ReceiverAnnounce) handleFirstAnnounce(request *tracker.Request, response *Response.Response) {
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
		ra.ForwarderManager.CacheRequest(request.InfoHash, *request)
		ra.ForwarderManager.QueueEligibleAnnounces(request.InfoHash, *request)
	} else if len(ra.Config.Forwards) > 0 {
		ErrorLogAnnounce.Printf("Forwarders configured (%d) but forwarder manager is nil; cannot forward initial announce for %x", len(ra.Config.Forwards), request.InfoHash)
	}
}

// handleSubsequentAnnounce handles subsequent announces for an info_hash
func (ra *ReceiverAnnounce) handleSubsequentAnnounce(request *tracker.Request, response *Response.Response) {
	// Peers already collected in handleRegularAnnounce via getPeersForResponse()
	// No need to collect again here

	// Use general interval setting from config
	response.Interval = ra.clampInterval(ra.Config.AnnounceInterval)

	// For regular announces, forward immediately if due or not yet contacted
	if ra.ForwarderManager != nil {
		ra.ForwarderManager.CacheRequest(request.InfoHash, *request)
		ra.ForwarderManager.QueueEligibleAnnounces(request.InfoHash, *request)
	}
}

// getPeersForResponse collects peers from both local storage and forwarder storage
func (ra *ReceiverAnnounce) getPeersForResponse(infoHash common.InfoHash) []common.Peer {
	peers := ra.Storage.GetPeers(infoHash)

	if ra.ForwarderStorage != nil {
		forwarderPeers := ra.ForwarderStorage.GetAllPeers(infoHash)
		peers = append(peers, forwarderPeers...)
	}

	return peers
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

func (ra *ReceiverAnnounce) countLocalPeers(infoHash common.InfoHash) (int, int) {
	if ra.Storage == nil {
		return 0, 0
	}

	seeders := 0
	leechers := 0

	ra.Storage.requestsMu.Lock()
	if requestInfoHash, found := ra.Storage.Requests[infoHash]; found {
		for _, peerRequest := range requestInfoHash {
			if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
				seeders++
			} else {
				leechers++
			}
		}
	}
	ra.Storage.requestsMu.Unlock()

	return seeders, leechers
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

func NewReceiverAnnounce(cfg *config.Config, storage *Storage, forwarderStorage *ForwarderStorage, forwarderManager *ForwarderManager) *ReceiverAnnounce {
	announce := ReceiverAnnounce{
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: forwarderManager,
	}
	return &announce
}
