package response

import (
	"bytes"
	"encoding/binary"
	"net"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/zeebo/bencode"
)

type ResponseCompacted struct {
	Interval      int    `bencode:"interval"`
	MinInterval   int    `bencode:"min interval,omitempty"`
	Complete      int    `bencode:"complete,omitempty"`
	Incomplete    int    `bencode:"incomplete,omitempty"`
	TrackerID     string `bencode:"tracker id,omitempty"`
	FailureReason string `bencode:"failure reason,omitempty"`
	Peers4        []byte `bencode:"peers"`
	Peers6        []byte `bencode:"peers6"`
}

func (self *ResponseCompacted) Bencode() (string, error) {
	return bencode.EncodeString(self)
}

func (self *ResponseCompacted) Response() Response {
	response := Response{
		Interval:      self.Interval,
		MinInterval:   self.MinInterval,
		Complete:      self.Complete,
		Incomplete:    self.Incomplete,
		TrackerID:     self.TrackerID,
		FailureReason: self.FailureReason,
		Peers:         self.Peers(),
	}
	return response
}

func (self *ResponseCompacted) Peers() []common.Peer {
	peers := make([]common.Peer, 0)
	peers = append(peers, self.peers4Parse()...)
	peers = append(peers, self.peers6Parse()...)
	return peers
}

func (self *ResponseCompacted) peers4Parse() []common.Peer {
	peers := make([]common.Peer, 0)
	buf := bytes.NewBuffer(self.Peers4)
	for {
		ipBytes := make([]byte, 4)
		if n, err := buf.Read(ipBytes); err != nil || n != 4 {
			break
		}
		portBytes := make([]byte, 2)
		if n, err := buf.Read(portBytes); err != nil || n != 2 {
			break
		}
		peer := common.Peer{
			IP:   common.Address(net.IP(ipBytes).String()),
			Port: int(binary.BigEndian.Uint16(portBytes)),
		}
		peers = append(peers, peer)
	}
	return peers
}

func (self *ResponseCompacted) peers6Parse() []common.Peer {
	peers := make([]common.Peer, 0)
	buf := bytes.NewBuffer(self.Peers6)
	for {
		ipBytes := make([]byte, 16)
		if n, err := buf.Read(ipBytes); err != nil || n != 16 {
			break
		}
		portBytes := make([]byte, 2)
		if n, err := buf.Read(portBytes); err != nil || n != 2 {
			break
		}
		peer := common.Peer{
			IP:   common.Address(net.IP(ipBytes).String()),
			Port: int(binary.BigEndian.Uint16(portBytes)),
		}
		peers = append(peers, peer)
	}
	return peers
}

func (self *ResponseCompacted) ReloadPeers(peers []common.Peer) {
	var peers4 bytes.Buffer
	var peers6 bytes.Buffer
	for _, peer := range peers {
		if ip, err := peer.IP.IPv4(); err == nil {
			peers4.Write([]byte(ip.To4()))
			portBytes := make([]byte, 2)
			binary.BigEndian.PutUint16(portBytes, uint16(peer.Port))
			peers4.Write(portBytes)
			continue
		}
		if ip, err := peer.IP.IPv6(); err == nil {
			peers6.Write([]byte(ip.To16()))
			portBytes := make([]byte, 2)
			binary.BigEndian.PutUint16(portBytes, uint16(peer.Port))
			peers6.Write(portBytes)
			continue
		}
		ErrorLog.Printf("Can't identify IP: %v", peer.IP)
	}
	self.Peers4 = peers4.Bytes()
	if peers6.Len() > 0 {
		self.Peers6 = peers6.Bytes()
	} else {
		self.Peers6 = nil
	}
}
