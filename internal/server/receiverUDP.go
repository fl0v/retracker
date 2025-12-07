package server

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

var (
	DebugLogUDP = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogUDP = log.New(os.Stderr, `error#`, log.Lshortfile)
)

const (
	UDPActionConnect  = 0
	UDPActionAnnounce = 1
	UDPActionScrape   = 2
	UDPActionError    = 3

	UDPConnectMagic = 0x41727101980 // Magic constant for connect request
)

type ReceiverUDP struct {
	Config      *config.Config
	Storage     *Storage
	Prometheus  *observability.Prometheus
	TempStorage *TempStorage
	conn        *net.UDPConn
	connections map[uint64]time.Time // connection_id -> timestamp
	connMu      sync.Mutex
}

type UDPConnectRequest struct {
	Magic         uint64
	Action        uint32
	TransactionID uint32
}

type UDPConnectResponse struct {
	Action        uint32
	TransactionID uint32
	ConnectionID  uint64
}

type UDPAnnounceRequest struct {
	ConnectionID  uint64
	Action        uint32
	TransactionID uint32
	InfoHash      [20]byte
	PeerID        [20]byte
	Downloaded    uint64
	Left          uint64
	Uploaded      uint64
	Event         uint32
	IP            uint32
	Key           uint32
	NumWant       int32
	Port          uint16
}

type UDPAnnounceResponse struct {
	Action        uint32
	TransactionID uint32
	Interval      uint32
	Leechers      uint32
	Seeders       uint32
	Peers         []UDPPeer
}

type UDPPeer struct {
	IP   [4]byte
	Port uint16
}

type UDPScrapeRequest struct {
	ConnectionID  uint64
	Action        uint32
	TransactionID uint32
	InfoHashes    [][20]byte
}

type UDPScrapeResponse struct {
	Action        uint32
	TransactionID uint32
	ScrapeData    []UDPScrapeData
}

type UDPScrapeData struct {
	Seeders   uint32
	Completed uint32
	Leechers  uint32
}

func NewReceiverUDP(cfg *config.Config, storage *Storage) *ReceiverUDP {
	receiver := ReceiverUDP{
		Config:      cfg,
		Storage:     storage,
		connections: make(map[uint64]time.Time),
	}
	return &receiver
}

func (ru *ReceiverUDP) Start(listenAddr string) error {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %w", err)
	}

	ru.conn = conn
	fmt.Printf("UDP tracker listening on %s\n", listenAddr)

	// Start connection cleanup routine
	go ru.cleanupConnections()

	// Start packet handler
	go ru.handlePackets()

	return nil
}

func (ru *ReceiverUDP) cleanupConnections() {
	for {
		time.Sleep(1 * time.Minute)
		ru.connMu.Lock()
		now := time.Now()
		// BEP 15: Trackers should accept connection IDs until 2 minutes after they've been sent
		for connID, timestamp := range ru.connections {
			if now.Sub(timestamp) > 2*time.Minute {
				delete(ru.connections, connID)
			}
		}
		ru.connMu.Unlock()
	}
}

func (ru *ReceiverUDP) handlePackets() {
	buffer := make([]byte, 4096)
	for {
		n, clientAddr, err := ru.conn.ReadFromUDP(buffer)
		if err != nil {
			ErrorLogUDP.Printf("UDP read error: %s\n", err.Error())
			continue
		}

		if n < 16 {
			ErrorLogUDP.Printf("UDP packet too short: %d bytes\n", n)
			continue
		}

		go ru.handlePacket(buffer[:n], clientAddr)
	}
}

func (ru *ReceiverUDP) handlePacket(data []byte, clientAddr *net.UDPAddr) {
	if ru.Prometheus != nil {
		ru.Prometheus.Requests.Inc()
	}

	reader := bytes.NewReader(data)

	// Check if it's a connect request (first 8 bytes are magic)
	var magic uint64
	if err := binary.Read(reader, binary.BigEndian, &magic); err != nil {
		ErrorLogUDP.Printf("Failed to read magic: %s\n", err.Error())
		return
	}

	if magic == UDPConnectMagic {
		ru.handleConnect(data, clientAddr)
		return
	}

	// Otherwise, it's an announce or scrape request
	// First 8 bytes are connection ID
	var connectionID uint64
	reader = bytes.NewReader(data)
	if err := binary.Read(reader, binary.BigEndian, &connectionID); err != nil {
		ErrorLogUDP.Printf("Failed to read connection ID: %s\n", err.Error())
		return
	}

	// Validate connection ID
	ru.connMu.Lock()
	_, valid := ru.connections[connectionID]
	ru.connMu.Unlock()

	if !valid {
		// Read transaction ID before sending error
		var transactionID uint32
		reader.Seek(12, 0) // Skip connection_id (8 bytes) and action (4 bytes) to get transaction_id
		if err := binary.Read(reader, binary.BigEndian, &transactionID); err == nil {
			ru.sendError(clientAddr, transactionID, "Invalid connection ID")
		} else {
			ru.sendError(clientAddr, 0, "Invalid connection ID")
		}
		return
	}

	// Read action
	var action uint32
	if err := binary.Read(reader, binary.BigEndian, &action); err != nil {
		ErrorLogUDP.Printf("Failed to read action: %s\n", err.Error())
		// Try to read transaction ID for error response
		var transactionID uint32
		reader.Seek(12, 0)
		if err2 := binary.Read(reader, binary.BigEndian, &transactionID); err2 == nil {
			ru.sendError(clientAddr, transactionID, "Invalid packet format")
		} else {
			ru.sendError(clientAddr, 0, "Invalid packet format")
		}
		return
	}

	// Read transaction ID for potential error responses
	var transactionID uint32
	reader.Seek(12, 0)
	binary.Read(reader, binary.BigEndian, &transactionID)

	switch action {
	case UDPActionAnnounce:
		ru.handleAnnounce(data, clientAddr)
	case UDPActionScrape:
		ru.handleScrape(data, clientAddr)
	default:
		ErrorLogUDP.Printf("Unknown action: %d\n", action)
		ru.sendError(clientAddr, transactionID, "Unknown action")
	}
}

func (ru *ReceiverUDP) handleConnect(data []byte, clientAddr *net.UDPAddr) {
	// BEP 15: Check whether the packet is at least 16 bytes
	if len(data) < 16 {
		ErrorLogUDP.Printf("Connect request too short: %d bytes (minimum 16)\n", len(data))
		return
	}

	var req UDPConnectRequest
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.BigEndian, &req.Magic); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Action); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.TransactionID); err != nil {
		return
	}

	// BEP 15: Check whether the action is connect (0)
	if req.Action != UDPActionConnect {
		ErrorLogUDP.Printf("Invalid action in connect request: %d (expected 0)\n", req.Action)
		ru.sendError(clientAddr, req.TransactionID, "Invalid action")
		return
	}

	if ru.Config.Debug {
		DebugLogUDP.Printf("UDP Connect request from %s, transaction_id: %d\n", clientAddr.String(), req.TransactionID)
	}

	// Generate connection ID - BEP 15: Connection IDs should not be guessable by the client
	// Use crypto/rand for secure random generation
	randomBytes := make([]byte, 8)
	var connectionID uint64
	if _, err := rand.Read(randomBytes); err != nil {
		ErrorLogUDP.Printf("Failed to generate random connection ID: %s\n", err.Error())
		// Fallback to timestamp-based if random fails (not ideal but better than crashing)
		connectionID = uint64(time.Now().UnixNano())
	} else {
		connectionID = binary.BigEndian.Uint64(randomBytes)
	}

	// Store connection ID
	ru.connMu.Lock()
	ru.connections[connectionID] = time.Now()
	ru.connMu.Unlock()

	// Build response
	var resp UDPConnectResponse
	resp.Action = UDPActionConnect
	resp.TransactionID = req.TransactionID
	resp.ConnectionID = connectionID

	ru.sendConnectResponse(clientAddr, resp)
}

func (ru *ReceiverUDP) handleAnnounce(data []byte, clientAddr *net.UDPAddr) {
	// BEP 15: Check whether the packet is at least 20 bytes (minimum for announce)
	// Actual IPv4 announce request is 98 bytes, but we check minimum per spec
	if len(data) < 20 {
		ErrorLogUDP.Printf("Announce request too short: %d bytes (minimum 20)\n", len(data))
		// Try to read transaction ID for error response
		var transactionID uint32
		if len(data) >= 16 {
			reader := bytes.NewReader(data)
			reader.Seek(12, 0) // Skip connection_id (8) + action (4) = 12
			binary.Read(reader, binary.BigEndian, &transactionID)
		}
		ru.sendError(clientAddr, transactionID, "Announce request too short")
		return
	}

	var req UDPAnnounceRequest
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.BigEndian, &req.ConnectionID); err != nil {
		ru.sendError(clientAddr, 0, "Invalid announce packet")
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Action); err != nil {
		ru.sendError(clientAddr, 0, "Invalid announce packet")
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.TransactionID); err != nil {
		ru.sendError(clientAddr, 0, "Invalid announce packet")
		return
	}

	// BEP 15: Check whether the action is announce (1)
	if req.Action != UDPActionAnnounce {
		ErrorLogUDP.Printf("Invalid action in announce request: %d (expected 1)\n", req.Action)
		ru.sendError(clientAddr, req.TransactionID, "Invalid action")
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.InfoHash); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.PeerID); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Downloaded); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Left); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Uploaded); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Event); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.IP); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Key); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.NumWant); err != nil {
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &req.Port); err != nil {
		return
	}

	// Convert to string format for existing ProcessAnnounce
	infoHashStr := string(req.InfoHash[:])
	peerIDStr := string(req.PeerID[:])
	remoteAddr := clientAddr.IP.String()
	portStr := strconv.Itoa(int(req.Port))
	uploadedStr := strconv.FormatUint(req.Uploaded, 10)
	downloadedStr := strconv.FormatUint(req.Downloaded, 10)
	leftStr := strconv.FormatUint(req.Left, 10)
	numwantStr := strconv.FormatInt(int64(req.NumWant), 10)

	// Map event
	var eventStr string
	switch req.Event {
	case 0:
		eventStr = "" // none
	case 1:
		eventStr = "completed"
	case 2:
		eventStr = "started"
	case 3:
		eventStr = "stopped"
	default:
		eventStr = ""
	}

	// Get IP from request or use client IP
	ipStr := ""
	if req.IP != 0 {
		ipBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(ipBytes, req.IP)
		ipStr = net.IP(ipBytes).String()
	}

	if ru.Config.Debug {
		DebugLogUDP.Printf("UDP Announce from %s, hash: %x, peer_id: %x, port: %d, event: %s\n",
			remoteAddr, req.InfoHash, req.PeerID, req.Port, eventStr)
	}

	// Use existing ProcessAnnounce logic
	response := ru.ProcessAnnounce(
		remoteAddr,
		infoHashStr,
		peerIDStr,
		portStr,
		uploadedStr,
		downloadedStr,
		leftStr,
		ipStr,
		numwantStr,
		eventStr,
		"", // UDP doesn't have user agent
	)

	if response == nil {
		ru.sendError(clientAddr, req.TransactionID, "Invalid announce request")
		return
	}

	// Convert response to UDP format
	infoHash := common.InfoHash(infoHashStr)
	ru.sendAnnounceResponse(clientAddr, req.TransactionID, response, infoHash)
}

func (ru *ReceiverUDP) handleScrape(data []byte, clientAddr *net.UDPAddr) {
	// BEP 15: Check whether the packet is at least 8 bytes (minimum for scrape)
	// But we need at least connection_id (8) + action (4) + transaction_id (4) = 16 bytes
	if len(data) < 16 {
		ErrorLogUDP.Printf("Scrape request too short: %d bytes (minimum 16)\n", len(data))
		// Try to read transaction ID for error response
		var transactionID uint32
		if len(data) >= 12 {
			reader := bytes.NewReader(data)
			reader.Seek(12, 0) // Skip connection_id (8) + action (4) = 12
			binary.Read(reader, binary.BigEndian, &transactionID)
		}
		ru.sendError(clientAddr, transactionID, "Scrape request too short")
		return
	}

	var connectionID uint64
	var action uint32
	var transactionID uint32

	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.BigEndian, &connectionID); err != nil {
		ru.sendError(clientAddr, 0, "Invalid scrape packet")
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &action); err != nil {
		ru.sendError(clientAddr, 0, "Invalid scrape packet")
		return
	}
	if err := binary.Read(reader, binary.BigEndian, &transactionID); err != nil {
		ru.sendError(clientAddr, 0, "Invalid scrape packet")
		return
	}

	// BEP 15: Check whether the action is scrape (2)
	if action != UDPActionScrape {
		ErrorLogUDP.Printf("Invalid action in scrape request: %d (expected 2)\n", action)
		ru.sendError(clientAddr, transactionID, "Invalid action")
		return
	}

	// Parse info hashes (each is 20 bytes)
	infoHashes := make([][20]byte, 0)
	remaining := data[16:]
	for len(remaining) >= 20 {
		var hash [20]byte
		copy(hash[:], remaining[:20])
		infoHashes = append(infoHashes, hash)
		remaining = remaining[20:]
	}

	if ru.Config.Debug {
		DebugLogUDP.Printf("UDP Scrape from %s, transaction_id: %d, hashes: %d\n",
			clientAddr.String(), transactionID, len(infoHashes))
	}

	// Build scrape response
	scrapeData := make([]UDPScrapeData, 0)
	for _, hash := range infoHashes {
		infoHash := common.InfoHash(string(hash[:]))
		if !infoHash.Valid() {
			scrapeData = append(scrapeData, UDPScrapeData{})
			continue
		}

		// Get statistics from storage
		ru.Storage.requestsMu.Lock()
		requestInfoHash, found := ru.Storage.Requests[infoHash]
		seeders := uint32(0)
		leechers := uint32(0)
		if found {
			for _, peerRequest := range requestInfoHash {
				if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
					seeders++
				} else {
					leechers++
				}
			}
		}
		ru.Storage.requestsMu.Unlock()

		scrapeData = append(scrapeData, UDPScrapeData{
			Seeders:   seeders,
			Completed: seeders, // We don't track actual completed count
			Leechers:  leechers,
		})
	}

	ru.sendScrapeResponse(clientAddr, transactionID, scrapeData)
}

func (ru *ReceiverUDP) ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant, event, userAgent string) *Response.Response {
	// Reuse the existing HTTP announce processing logic
	// We'll create a temporary ReceiverAnnounce to use its ProcessAnnounce method
	ra := &ReceiverAnnounce{
		Config:      ru.Config,
		Storage:     ru.Storage,
		Prometheus:  ru.Prometheus,
		TempStorage: ru.TempStorage,
	}
	return ra.ProcessAnnounce(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant, event, userAgent)
}

func (ru *ReceiverUDP) sendConnectResponse(clientAddr *net.UDPAddr, resp UDPConnectResponse) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, resp.Action)
	binary.Write(buf, binary.BigEndian, resp.TransactionID)
	binary.Write(buf, binary.BigEndian, resp.ConnectionID)

	ru.conn.WriteToUDP(buf.Bytes(), clientAddr)
}

func (ru *ReceiverUDP) sendAnnounceResponse(clientAddr *net.UDPAddr, transactionID uint32, response *Response.Response, infoHash common.InfoHash) {
	// Count seeders and leechers from storage
	seeders := uint32(0)
	leechers := uint32(0)

	ru.Storage.requestsMu.Lock()
	if requestInfoHash, found := ru.Storage.Requests[infoHash]; found {
		for _, peerRequest := range requestInfoHash {
			if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
				seeders++
			} else {
				leechers++
			}
		}
	}
	ru.Storage.requestsMu.Unlock()

	// Build response buffer
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint32(UDPActionAnnounce))
	binary.Write(buf, binary.BigEndian, transactionID)
	binary.Write(buf, binary.BigEndian, uint32(response.Interval))
	binary.Write(buf, binary.BigEndian, leechers)
	binary.Write(buf, binary.BigEndian, seeders)

	// BEP 15: Format is determined by address family of underlying UDP packet
	// IPv4: 6 bytes per peer (4 bytes IP + 2 bytes port)
	// IPv6: 18 bytes per peer (16 bytes IP + 2 bytes port)
	isIPv6 := clientAddr.IP.To4() == nil

	peerCount := 0
	for _, peer := range response.Peers {
		if isIPv6 {
			// IPv6 format: 18 bytes per peer
			if ip, err := peer.IP.IPv6(); err == nil {
				ipBytes := ip.To16()
				if ipBytes != nil {
					binary.Write(buf, binary.BigEndian, ipBytes)
					binary.Write(buf, binary.BigEndian, uint16(peer.Port))
					peerCount++
				}
			}
		} else {
			// IPv4 format: 6 bytes per peer
			if ip, err := peer.IP.IPv4(); err == nil {
				ipBytes := ip.To4()
				if ipBytes != nil {
					binary.Write(buf, binary.BigEndian, [4]byte{ipBytes[0], ipBytes[1], ipBytes[2], ipBytes[3]})
					binary.Write(buf, binary.BigEndian, uint16(peer.Port))
					peerCount++
				}
			}
		}
		// Limit to reasonable number of peers (typically 50)
		if peerCount >= 50 {
			break
		}
	}

	ru.conn.WriteToUDP(buf.Bytes(), clientAddr)

	if ru.Config.Debug {
		DebugLogUDP.Printf("UDP Announce response sent (%s): interval=%d, seeders=%d, leechers=%d, peers=%d\n",
			map[bool]string{true: "IPv6", false: "IPv4"}[isIPv6], response.Interval, seeders, leechers, peerCount)
	}
}

func (ru *ReceiverUDP) sendScrapeResponse(clientAddr *net.UDPAddr, transactionID uint32, scrapeData []UDPScrapeData) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint32(UDPActionScrape))
	binary.Write(buf, binary.BigEndian, transactionID)

	for _, data := range scrapeData {
		binary.Write(buf, binary.BigEndian, data.Seeders)
		binary.Write(buf, binary.BigEndian, data.Completed)
		binary.Write(buf, binary.BigEndian, data.Leechers)
	}

	ru.conn.WriteToUDP(buf.Bytes(), clientAddr)
}

func (ru *ReceiverUDP) sendError(clientAddr *net.UDPAddr, transactionID uint32, message string) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint32(UDPActionError))
	binary.Write(buf, binary.BigEndian, transactionID)
	buf.WriteString(message)

	ru.conn.WriteToUDP(buf.Bytes(), clientAddr)
}

func (ru *ReceiverUDP) Close() error {
	if ru.conn != nil {
		return ru.conn.Close()
	}
	return nil
}
