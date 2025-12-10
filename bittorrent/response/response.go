package response

import (
	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/zeebo/bencode"
)

type Response struct {
	Interval      int           `bencode:"interval"`
	MinInterval   int           `bencode:"min interval,omitempty"`
	Complete      int           `bencode:"complete,omitempty"`
	Incomplete    int           `bencode:"incomplete,omitempty"`
	TrackerID     string        `bencode:"tracker id,omitempty"`
	FailureReason string        `bencode:"failure reason,omitempty"`
	RetryIn       interface{}   `bencode:"retry in,omitempty"` // BEP 31: int (minutes) or "never"
	Peers         []common.Peer `bencode:"peers"`
}

func (self *Response) Bencode(compacted bool) (string, error) {
	if compacted && self.FailureReason == "" {
		response := self.Compacted()
		return response.Bencode()
	}
	return bencode.EncodeString(self)
}

func (self *Response) Compacted() ResponseCompacted {
	response := ResponseCompacted{
		Interval:      self.Interval,
		MinInterval:   self.MinInterval,
		Complete:      self.Complete,
		Incomplete:    self.Incomplete,
		TrackerID:     self.TrackerID,
		FailureReason: self.FailureReason,
	}
	response.ReloadPeers(self.Peers)
	return response
}

func NewFailure(reason string) *Response {
	return &Response{FailureReason: reason}
}

func Load(b []byte) (*Response, error) {
	response := Response{}
	if err := bencode.DecodeBytes(b, &response); err == nil {
		return &response, nil
	}

	// Try compact form
	responseCompacted := ResponseCompacted{}
	if err := bencode.DecodeBytes(b, &responseCompacted); err == nil {
		resp := responseCompacted.Response()
		return &resp, nil
	}

	// Last-resort tolerant decoding: accept peers as list-of-dicts (including empty list)
	raw := make(map[string]interface{})
	if err := bencode.DecodeBytes(b, &raw); err != nil {
		return nil, err
	}

	resp := Response{
		Interval:    asInt(raw["interval"]),
		MinInterval: asInt(raw["min interval"]),
		Complete:    asInt(raw["complete"]),
		Incomplete:  asInt(raw["incomplete"]),
		TrackerID:   asString(raw["tracker id"]),
	}

	if retryInVal, ok := raw["retry in"]; ok {
		resp.RetryIn = retryInVal
	}

	if peersVal, ok := raw["peers"]; ok {
		if peersList, ok := peersVal.([]interface{}); ok {
			resp.Peers = parsePeerList(peersList)
		}
	}

	return &resp, nil
}

func asInt(v interface{}) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case int:
		return t
	}
	return 0
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func parsePeerList(list []interface{}) []common.Peer {
	peers := make([]common.Peer, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		peer := common.Peer{}
		if ipVal, ok := m["ip"].(string); ok {
			peer.IP = common.Address(ipVal)
		}
		if portVal, ok := m["port"].(int64); ok {
			peer.Port = int(portVal)
		}
		if portVal, ok := m["port"].(int); ok {
			peer.Port = portVal
		}
		if peerIDVal, ok := m["peer id"].(string); ok {
			peer.PeerID = common.PeerID(peerIDVal)
		} else if peerIDVal, ok := m["peer_id"].(string); ok {
			peer.PeerID = common.PeerID(peerIDVal)
		}
		// Only append if we have at least IP or port to avoid empty peers
		if peer.IP != "" || peer.Port != 0 {
			peers = append(peers, peer)
		}
	}
	return peers
}
