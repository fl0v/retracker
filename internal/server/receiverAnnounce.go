package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"

	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

var (
	DebugLogAnnounce = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogAnnounce = log.New(os.Stderr, `error#`, log.Lshortfile)
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
	response := ra.ProcessAnnounce(
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
	)
	compacted := false
	if r.URL.Query().Get(`compact`) == `1` {
		compacted = true
	}
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
	r := regexp.MustCompile(`(.*):\d+$`)
	if match := r.FindStringSubmatch(in); len(match) == 2 {
		address = match[1]
	}
	return address
}

func (ra *ReceiverAnnounce) ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant, event string) *Response.Response {
	request, err := tracker.MakeRequest(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
		event, DebugLog)
	if err != nil {
		return nil
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

	return &response
}

// handleStoppedEvent processes a stopped event: forwards to forwarders, cancels pending jobs, and deletes the peer
func (ra *ReceiverAnnounce) handleStoppedEvent(request *tracker.Request, response *Response.Response) {
	// Forward stopped event to forwarders immediately
	if ra.ForwarderManager != nil {
		ra.ForwarderManager.ForwardStoppedEvent(request.InfoHash, request.PeerID, *request)
	}

	// Delete peer from storage
	ra.Storage.Delete(*request)

	// Clean up forwarder storage when no local peers remain
	if ra.ForwarderStorage != nil {
		localPeers := ra.Storage.GetPeers(request.InfoHash)
		if len(localPeers) == 0 {
			ra.ForwarderStorage.Cleanup(request.InfoHash)
		}
	}

	response.Interval = ra.Config.AnnounceResponseInterval
}

// handleCompletedEvent processes a completed event: updates storage, forwards event, but continues normal flow
func (ra *ReceiverAnnounce) handleCompletedEvent(request *tracker.Request, response *Response.Response) {
	// Update storage with completed status
	ra.Storage.Update(*request)

	// Get peers for response
	response.Peers = ra.getPeersForResponse(request.InfoHash)

	// Calculate interval
	response.Interval = ra.calculateInterval(request.InfoHash)

	// Forward completed event (one-time notification, but don't cancel jobs)
	if ra.ForwarderManager != nil {
		ra.ForwarderManager.ForwardCompletedEvent(request.InfoHash, request.PeerID, *request)
		// Continue normal announce scheduling
		ra.ForwarderManager.CacheRequest(request.InfoHash, *request)
		ra.ForwarderManager.CheckAndReannounce(request.InfoHash, *request, response.Interval)
	}
}

// handleRegularAnnounce processes regular announces (started event or empty event)
func (ra *ReceiverAnnounce) handleRegularAnnounce(request *tracker.Request, response *Response.Response) {
	// Update storage
	ra.Storage.Update(*request)

	// Get local peers
	response.Peers = ra.Storage.GetPeers(request.InfoHash)

	// Check if this is first announce for this info_hash
	if ra.isFirstAnnounce(request.InfoHash) {
		ra.handleFirstAnnounce(request, response)
	} else {
		ra.handleSubsequentAnnounce(request, response)
	}
}

// handleFirstAnnounce handles the first announce for an info_hash
func (ra *ReceiverAnnounce) handleFirstAnnounce(request *tracker.Request, response *Response.Response) {
	// First announce: return default shorter interval and trigger parallel forwarder announces
	response.Interval = 15 // Default shorter interval for first announce

	// Get cached forwarder peers (should be empty on first announce)
	if ra.ForwarderStorage != nil {
		forwarderPeers := ra.ForwarderStorage.GetAllPeers(request.InfoHash)
		response.Peers = append(response.Peers, forwarderPeers...)
	}

	// Trigger parallel decoupled announces to all forwarders
	if ra.ForwarderManager != nil {
		ra.ForwarderManager.CacheRequest(request.InfoHash, *request)
		ra.ForwarderManager.TriggerInitialAnnounce(request.InfoHash, *request)
	}
}

// handleSubsequentAnnounce handles subsequent announces for an info_hash
func (ra *ReceiverAnnounce) handleSubsequentAnnounce(request *tracker.Request, response *Response.Response) {
	// Get cached forwarder peers
	if ra.ForwarderStorage != nil {
		forwarderPeers := ra.ForwarderStorage.GetAllPeers(request.InfoHash)
		response.Peers = append(response.Peers, forwarderPeers...)

		// Calculate interval
		response.Interval = ra.calculateInterval(request.InfoHash)

		// Check if we need to re-announce based on interval comparison
		if ra.ForwarderManager != nil {
			ra.ForwarderManager.CacheRequest(request.InfoHash, *request)
			ra.ForwarderManager.CheckAndReannounce(request.InfoHash, *request, response.Interval)
		}
	} else {
		response.Interval = ra.Config.AnnounceResponseInterval
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

// calculateInterval calculates the appropriate interval for the response
func (ra *ReceiverAnnounce) calculateInterval(infoHash common.InfoHash) int {
	if ra.ForwarderStorage != nil {
		avgInterval := ra.ForwarderStorage.GetAverageInterval(infoHash)
		if avgInterval > 0 {
			return avgInterval
		}
	}
	// No forwarders responded yet, use default
	return ra.Config.AnnounceResponseInterval
}

// isFirstAnnounce checks if this is the first announce for the given info_hash
func (ra *ReceiverAnnounce) isFirstAnnounce(infoHash common.InfoHash) bool {
	if ra.ForwarderStorage != nil {
		return !ra.ForwarderStorage.HasInfoHash(infoHash)
	}
	return false
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
