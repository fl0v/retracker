package server

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	Response "github.com/fl0v/retracker/bittorrent/response"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"
)

var (
	DebugLogUDPFwd = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogUDPFwd = log.New(os.Stderr, `error#`, log.Lshortfile)
)

const (
	// UDP Tracker Protocol constants (BEP 15)
	udpConnectMagic   = 0x41727101980
	udpActionConnect  = 0
	udpActionAnnounce = 1
	udpActionScrape   = 2
	udpActionError    = 3

	// Connection ID lifetime per BEP 15 (2 minutes)
	connectionIDLifetime = 5 * time.Minute

	// Default timeout for UDP operations
	defaultUDPTimeout = 30 * time.Second
)

// connectionEntry stores a connection ID with its timestamp
type connectionEntry struct {
	connID    uint64
	timestamp time.Time
}

// UDPForwarder manages UDP connections to external trackers
type UDPForwarder struct {
	debug       bool
	timeout     time.Duration
	maxRetries  int
	retryBase   time.Duration
	connections map[string]connectionEntry // forwarderName -> connection entry
	connMu      sync.Mutex
	stopChan    chan struct{}
}

// NewUDPForwarder creates a new UDP forwarder client
func NewUDPForwarder(debug bool, timeout int, retries int, retryBaseMs int) *UDPForwarder {
	if timeout <= 0 {
		timeout = int(defaultUDPTimeout.Seconds())
	}
	if retries <= 0 {
		retries = 5
	}
	if retryBaseMs <= 0 {
		retryBaseMs = 500
	}
	uf := &UDPForwarder{
		debug:       debug,
		timeout:     time.Duration(timeout) * time.Second,
		maxRetries:  retries,
		retryBase:   time.Duration(retryBaseMs) * time.Millisecond,
		connections: make(map[string]connectionEntry),
		stopChan:    make(chan struct{}),
	}
	// Start connection cleanup routine
	go uf.cleanupConnections()
	return uf
}

// Stop stops the UDP forwarder and cleans up resources
func (uf *UDPForwarder) Stop() {
	close(uf.stopChan)
}

// cleanupConnections periodically removes expired connection IDs
func (uf *UDPForwarder) cleanupConnections() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-uf.stopChan:
			return
		case <-ticker.C:
			uf.connMu.Lock()
			now := time.Now()
			for name, entry := range uf.connections {
				if now.Sub(entry.timestamp) > connectionIDLifetime {
					delete(uf.connections, name)
					if uf.debug {
						DebugLogUDPFwd.Printf("Expired connection ID for %s\n", name)
					}
				}
			}
			uf.connMu.Unlock()
		}
	}
}

// getConnectionID retrieves or establishes a connection ID for the forwarder
func (uf *UDPForwarder) getConnectionID(forwarder CoreCommon.Forward) (uint64, *net.UDPConn, error) {
	forwarderName := forwarder.GetName()

	// Check if we have a valid cached connection ID
	uf.connMu.Lock()
	entry, ok := uf.connections[forwarderName]
	if ok && time.Since(entry.timestamp) < connectionIDLifetime-10*time.Second {
		// Still valid (with 10s buffer before expiration)
		uf.connMu.Unlock()
		conn, err := uf.dialUDP(forwarder.Uri)
		if err != nil {
			return 0, nil, err
		}
		return entry.connID, conn, nil
	}
	uf.connMu.Unlock()

	// Need to establish new connection
	return uf.connect(forwarder)
}

// dialUDP creates a UDP connection to the tracker
func (uf *UDPForwarder) dialUDP(uri string) (*net.UDPConn, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "6969") // Standard UDP tracker port
	}

	addr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial UDP: %w", err)
	}

	return conn, nil
}

// generateTransactionID generates a random 32-bit transaction ID
func generateTransactionID() uint32 {
	b := make([]byte, 4)
	rand.Read(b)
	return binary.BigEndian.Uint32(b)
}

// connect sends a connect request and receives a connection ID
func (uf *UDPForwarder) connect(forwarder CoreCommon.Forward) (uint64, *net.UDPConn, error) {
	forwarderName := forwarder.GetName()

	conn, err := uf.dialUDP(forwarder.Uri)
	if err != nil {
		return 0, nil, err
	}

	transactionID := generateTransactionID()

	// Build connect request
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint64(udpConnectMagic))
	binary.Write(buf, binary.BigEndian, uint32(udpActionConnect))
	binary.Write(buf, binary.BigEndian, transactionID)

	// Send connect request with retries
	var connectionID uint64
	for retry := 0; retry < uf.maxRetries; retry++ {
		if retry > 0 {
			time.Sleep(uf.retryBase * time.Duration(1<<retry)) // Exponential backoff
		}

		// Reset deadline for each retry attempt
		conn.SetDeadline(time.Now().Add(uf.timeout))

		_, err = conn.Write(buf.Bytes())
		if err != nil {
			if retry == uf.maxRetries-1 {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to send connect request: %w", err)
			}
			if !isTimeoutErr(err) {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to send connect request: %w", err)
			}
			continue
		}

		// Read response
		respBuf := make([]byte, 16)
		n, err := conn.Read(respBuf)
		if err != nil {
			if retry == uf.maxRetries-1 {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to read connect response: %w", err)
			}
			if !isTimeoutErr(err) {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to read connect response: %w", err)
			}
			continue
		}

		if n < 16 {
			if retry == uf.maxRetries-1 {
				conn.Close()
				return 0, nil, fmt.Errorf("connect response too short: %d bytes", n)
			}
			continue
		}

		// Parse response
		reader := bytes.NewReader(respBuf)
		var respAction uint32
		var respTransactionID uint32
		binary.Read(reader, binary.BigEndian, &respAction)
		binary.Read(reader, binary.BigEndian, &respTransactionID)
		binary.Read(reader, binary.BigEndian, &connectionID)

		// Verify response
		if respAction == udpActionError {
			conn.Close()
			return 0, nil, fmt.Errorf("tracker returned error on connect")
		}

		if respAction != udpActionConnect {
			if retry == uf.maxRetries-1 {
				conn.Close()
				return 0, nil, fmt.Errorf("unexpected action in connect response: %d", respAction)
			}
			continue
		}

		if respTransactionID != transactionID {
			if retry == uf.maxRetries-1 {
				conn.Close()
				return 0, nil, fmt.Errorf("transaction ID mismatch: expected %d, got %d", transactionID, respTransactionID)
			}
			continue
		}

		// Success
		break
	}

	// Cache the connection ID
	uf.connMu.Lock()
	uf.connections[forwarderName] = connectionEntry{
		connID:    connectionID,
		timestamp: time.Now(),
	}
	uf.connMu.Unlock()

	if uf.debug {
		DebugLogUDPFwd.Printf("Got connection ID %d for %s\n", connectionID, forwarderName)
	}

	return connectionID, conn, nil
}

// Announce sends an announce request to the UDP tracker
func (uf *UDPForwarder) Announce(forwarder CoreCommon.Forward, request tracker.Request) (*Response.Response, error) {
	forwarderName := forwarder.GetName()
	trackerURL := forwarder.Uri

	connectionID, conn, err := uf.getConnectionID(forwarder)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection ID: %w", err)
	}
	defer conn.Close()

	transactionID := generateTransactionID()

	// Determine IP to use
	var ipUint32 uint32 = 0
	if forwarder.Ip != "" {
		ip := net.ParseIP(forwarder.Ip)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				ipUint32 = binary.BigEndian.Uint32(ip4)
			}
		}
	}

	// Map event string to event code
	var eventCode uint32 = 0
	switch request.Event {
	case "completed":
		eventCode = 1
	case "started":
		eventCode = 2
	case "stopped":
		eventCode = 3
	}

	// Determine numwant
	numWant := int32(50)
	if request.NumWant > 0 && request.NumWant < 200 {
		numWant = int32(request.NumWant)
	}

	// Build announce request (98 bytes for IPv4)
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, connectionID)
	binary.Write(buf, binary.BigEndian, uint32(udpActionAnnounce))
	binary.Write(buf, binary.BigEndian, transactionID)
	buf.Write([]byte(request.InfoHash[:])) // 20 bytes
	buf.Write([]byte(request.PeerID[:]))   // 20 bytes
	binary.Write(buf, binary.BigEndian, request.Downloaded)
	binary.Write(buf, binary.BigEndian, request.Left)
	binary.Write(buf, binary.BigEndian, request.Uploaded)
	binary.Write(buf, binary.BigEndian, eventCode)
	binary.Write(buf, binary.BigEndian, ipUint32)
	binary.Write(buf, binary.BigEndian, uint32(0)) // key (not used)
	binary.Write(buf, binary.BigEndian, numWant)
	binary.Write(buf, binary.BigEndian, uint16(request.Port))

	// Send announce request with retries
	var response *Response.Response
	for retry := 0; retry < uf.maxRetries; retry++ {
		if retry > 0 {
			time.Sleep(uf.retryBase * time.Duration(1<<retry))
		}

		// Reset deadline for each retry attempt
		conn.SetDeadline(time.Now().Add(uf.timeout))

		_, err = conn.Write(buf.Bytes())
		if err != nil {
			if retry == uf.maxRetries-1 {
				return nil, fmt.Errorf("failed to send announce request: %w", err)
			}
			continue
		}

		// Read response (up to 20 + 6*N bytes for IPv4, or 20 + 18*N for IPv6)
		respBuf := make([]byte, 4096)
		n, err := conn.Read(respBuf)
		if err != nil {
			if retry == uf.maxRetries-1 {
				return nil, fmt.Errorf("failed to read announce response: %w", err)
			}
			if !isTimeoutErr(err) {
				return nil, fmt.Errorf("failed to read announce response: %w", err)
			}
			continue
		}

		if n < 20 {
			if retry == uf.maxRetries-1 {
				return nil, fmt.Errorf("announce response too short: %d bytes", n)
			}
			continue
		}

		// Parse response header
		reader := bytes.NewReader(respBuf[:n])
		var respAction uint32
		var respTransactionID uint32
		var interval uint32
		var leechers uint32
		var seeders uint32
		binary.Read(reader, binary.BigEndian, &respAction)
		binary.Read(reader, binary.BigEndian, &respTransactionID)

		// Check for error
		if respAction == udpActionError {
			errorMsg := string(respBuf[8:n])
			// Check if it's an invalid connection ID error
			if uf.debug {
				DebugLogUDPFwd.Printf("Tracker error: %s\n", errorMsg)
			}
			// Clear cached connection ID to force reconnect
			uf.connMu.Lock()
			delete(uf.connections, forwarderName)
			uf.connMu.Unlock()
			return nil, fmt.Errorf("tracker error: %s", errorMsg)
		}

		if respAction != udpActionAnnounce {
			if retry == uf.maxRetries-1 {
				return nil, fmt.Errorf("unexpected action in announce response: %d", respAction)
			}
			continue
		}

		if respTransactionID != transactionID {
			if retry == uf.maxRetries-1 {
				return nil, fmt.Errorf("transaction ID mismatch: expected %d, got %d", transactionID, respTransactionID)
			}
			continue
		}

		binary.Read(reader, binary.BigEndian, &interval)
		binary.Read(reader, binary.BigEndian, &leechers)
		binary.Read(reader, binary.BigEndian, &seeders)

		// Parse peers
		peers := make([]common.Peer, 0)
		peerData := respBuf[20:n]

		// Detect address family based on remote address
		remoteAddr := conn.RemoteAddr().(*net.UDPAddr)
		isIPv6 := remoteAddr.IP.To4() == nil

		if isIPv6 {
			// IPv6: 18 bytes per peer (16 bytes IP + 2 bytes port)
			for i := 0; i+18 <= len(peerData); i += 18 {
				ip := net.IP(peerData[i : i+16])
				port := binary.BigEndian.Uint16(peerData[i+16 : i+18])
				peers = append(peers, common.Peer{
					IP:   common.Address(ip.String()),
					Port: int(port),
				})
			}
		} else {
			// IPv4: 6 bytes per peer (4 bytes IP + 2 bytes port)
			for i := 0; i+6 <= len(peerData); i += 6 {
				ip := net.IP(peerData[i : i+4])
				port := binary.BigEndian.Uint16(peerData[i+4 : i+6])
				peers = append(peers, common.Peer{
					IP:   common.Address(ip.String()),
					Port: int(port),
				})
			}
		}

		response = &Response.Response{
			Interval: int(interval),
			Peers:    peers,
		}

		if uf.debug {
			DebugLogUDPFwd.Printf("UDP announce to %s: interval=%d, seeders=%d, leechers=%d, peers=%d\n",
				trackerURL, interval, seeders, leechers, len(peers))
		}

		break
	}

	if response == nil {
		return nil, fmt.Errorf("failed to get announce response after %d retries", uf.maxRetries)
	}

	return response, nil
}
