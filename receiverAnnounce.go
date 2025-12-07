package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vvampirius/retracker/bittorrent/common"
	Response "github.com/vvampirius/retracker/bittorrent/response"
	"github.com/vvampirius/retracker/bittorrent/tracker"
	CoreCommon "github.com/vvampirius/retracker/common"
)

type ReceiverAnnounce struct {
	Config      *Config
	Storage     *Storage
	Prometheus  *Prometheus
	TempStorage *TempStorage
}

func (ra *ReceiverAnnounce) httpHandler(w http.ResponseWriter, r *http.Request) {
	if ra.Prometheus != nil {
		ra.Prometheus.Requests.Inc()
	}
	xrealip := r.Header.Get(`X-Real-IP`)
	DebugLog.Printf("%s %s %s '%s' '%s'\n", r.Method, r.RemoteAddr, xrealip, r.RequestURI, r.UserAgent())
	remoteAddr := ra.getRemoteAddr(r, xrealip)
	remotePort := r.URL.Query().Get(`port`)
	infoHash := r.URL.Query().Get(`info_hash`)
	if ra.Config.Debug {
		DebugLog.Printf("hash: '%x', remote addr: %s:%s", infoHash, remoteAddr, remotePort)
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
		ErrorLog.Println(err.Error())
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

func (ra *ReceiverAnnounce) ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
	event string) *Response.Response {
	if request, err := tracker.MakeRequest(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
		event, DebugLog); err == nil {

		response := Response.Response{
			Interval: ra.Config.AnnounceResponseInterval,
		}

		if request.Event != `stopped` {
			ra.Storage.Update(*request)
			response.Peers = ra.Storage.GetPeers(request.InfoHash)
			response.Peers = append(response.Peers, ra.makeForwards(*request)...)
		} else {
			ra.Storage.Delete(*request)
		}

		return &response
	}

	return nil
}

func (ra *ReceiverAnnounce) makeForwards(request tracker.Request) []common.Peer {
	peers := make([]common.Peer, 0)
	forwardsCount := len(ra.Config.Forwards)
	if forwardsCount > 0 {
		hash := fmt.Sprintf("%x", request.InfoHash)
		DebugLog.Printf("Processing %d forwarder(s) for info_hash %s (timeout: %ds)\n", forwardsCount, hash, ra.Config.ForwardTimeout)
		startTime := time.Now()
		ch := make(chan []common.Peer, forwardsCount)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(ra.Config.ForwardTimeout))
		defer cancel()
		for _, v := range ra.Config.Forwards {
			go ra.makeForward(v, request, ch, ctx)
		}
		for i := 0; i < forwardsCount; i++ {
			forwardPeers := <-ch
			peers = append(peers, forwardPeers...)
		}
		duration := time.Since(startTime)
		peers = CoreCommon.PeersUniq(peers)
		DebugLog.Printf("Forwarder processing completed for %s: %d unique peers from %d forwarder(s) in %v\n", hash, len(peers), forwardsCount, duration)
	}
	return peers
}

// isPrintableText checks if data is printable text (not binary)
func isPrintableText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	// Check if it's valid UTF-8 and mostly printable
	if !utf8.Valid(data) {
		return false
	}
	// Check if it contains mostly printable characters
	printableCount := 0
	for _, b := range data {
		if b >= 32 && b < 127 || b == '\n' || b == '\r' || b == '\t' {
			printableCount++
		}
	}
	// Consider it text if at least 80% is printable
	return float64(printableCount)/float64(len(data)) >= 0.8
}

func (ra *ReceiverAnnounce) makeForward(forward CoreCommon.Forward, request tracker.Request, ch chan<- []common.Peer, ctx context.Context) {
	startTime := time.Now()
	peers := make([]common.Peer, 0)
	uri := fmt.Sprintf("%s?info_hash=%s&peer_id=%s&port=%d&uploaded=%d&downloaded=%d&left=%d", forward.Uri, url.QueryEscape(string(request.InfoHash)),
		url.QueryEscape(string(request.PeerID)), request.Port, request.Uploaded, request.Downloaded, request.Left)
	if forward.Ip != `` {
		uri = fmt.Sprintf("%s&ip=%s&ipv4=%s", uri, forward.Ip, forward.Ip) //TODO: check for IPv4
	}
	hash := fmt.Sprintf("%x", request.InfoHash)
	forwardName := forward.GetName()
	trackerURL := forward.Uri

	// Normal mode: log hash and tracker URL (without params)
	fmt.Printf("Forwarding announce %s to %s\n", hash, trackerURL)
	// Debug mode: log actual Request URI
	if ra.Config.Debug {
		DebugLog.Printf("  Request URI: %s\n", uri)
		if forward.Ip != `` {
			DebugLog.Printf("  Using IP: %s\n", forward.Ip)
		}
		if forward.Host != `` {
			DebugLog.Printf("  Host header: %s\n", forward.Host)
		}
	}

	rqst, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		duration := time.Since(startTime)
		// Normal mode: hash, tracker URL, error
		ErrorLog.Printf("Error forwarding %s to %s: %s\n", hash, trackerURL, err.Error())
		// Debug mode: show duration
		if ra.Config.Debug {
			ErrorLog.Printf("  Duration: %v\n", duration)
		}
		ch <- peers
		return
	}
	if forward.Host != `` {
		rqst.Host = forward.Host
	}
	client := http.Client{}
	response, err := client.Do(rqst)
	duration := time.Since(startTime)
	if err != nil {
		// Normal mode: hash, tracker URL, error
		ErrorLog.Printf("Error forwarding %s to %s: %s\n", hash, trackerURL, err.Error())
		// Debug mode: show duration
		if ra.Config.Debug {
			ErrorLog.Printf("  Duration: %v\n", duration)
		}
		if ra.Prometheus != nil {
			ra.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: `error`}).Inc()
		}
		ch <- peers
		return
	}
	defer response.Body.Close()

	if ra.Prometheus != nil {
		ra.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: fmt.Sprintf("%d", response.StatusCode)}).Inc()
	}
	if response.StatusCode != http.StatusOK {
		// Normal mode: hash, tracker URL, error
		ErrorLog.Printf("Error forwarding %s to %s: HTTP %d %s\n", hash, trackerURL, response.StatusCode, response.Status)
		// Debug mode: show duration
		if ra.Config.Debug {
			ErrorLog.Printf("  Duration: %v\n", duration)
		}
		ch <- peers
		return
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		// Normal mode: hash, tracker URL, error
		ErrorLog.Printf("Error forwarding %s to %s: failed to read response: %s\n", hash, trackerURL, err.Error())
		// Debug mode: show duration
		if ra.Config.Debug {
			ErrorLog.Printf("  Duration: %v\n", duration)
		}
		if ra.Prometheus != nil {
			ra.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: fmt.Sprintf("%d", response.StatusCode)}).Inc()
		}
		ch <- peers
		return
	}

	tempFilename := ``
	if ra.Config.Debug {
		tempFilename = ra.TempStorage.SaveBencodeFromForwarder(payload, fmt.Sprintf("%x", request.InfoHash), uri)
	}
	bitResponse, err := Response.Load(payload)
	if err != nil {
		// Normal mode: hash, tracker URL, error
		ErrorLog.Printf("Error forwarding %s to %s: failed to parse response: %s\n", hash, trackerURL, err.Error())
		// Debug mode: show raw received data if not binary
		if ra.Config.Debug {
			if isPrintableText(payload) {
				// Limit to first 500 chars to avoid huge logs
				payloadStr := string(payload)
				if len(payloadStr) > 500 {
					payloadStr = payloadStr[:500] + "... (truncated)"
				}
				ErrorLog.Printf("  Raw response data: %s\n", payloadStr)
			} else {
				ErrorLog.Printf("  Response is binary (%d bytes)\n", len(payload))
			}
			if tempFilename == `` {
				tempFilename = ra.TempStorage.SaveBencodeFromForwarder(payload, fmt.Sprintf("%x", request.InfoHash), uri)
			}
			if tempFilename != `` {
				ErrorLog.Printf("  Response saved to: %s\n", tempFilename)
			}
		}
		if ra.Prometheus != nil {
			ra.Prometheus.ForwarderStatus.With(prometheus.Labels{`name`: forwardName, `status`: fmt.Sprintf("%d", response.StatusCode)}).Inc()
		}
		ch <- peers
		return
	}

	peers = append(peers, bitResponse.Peers...)
	// Normal mode: response size + duration
	fmt.Printf("Received %d bytes from %s in %v\n", len(payload), trackerURL, duration)
	// Debug mode: decoded response data
	if ra.Config.Debug {
		DebugLog.Printf("  Decoded response: interval=%d, peers=%d\n", bitResponse.Interval, len(bitResponse.Peers))
	}
	ch <- peers
}

func NewReceiverAnnounce(config *Config, storage *Storage) *ReceiverAnnounce {
	announce := ReceiverAnnounce{
		Config:  config,
		Storage: storage,
	}
	return &announce
}
