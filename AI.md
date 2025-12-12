# AI Indications for retracker

This file provides guidance for AI agents working on the retracker codebase. It documents key architectural patterns, conventions, and important considerations.

## Project Overview

**retracker** is a lightweight BitTorrent tracker written in Go that:
- Receives and processes BitTorrent announce/scrape requests (HTTP and UDP)
- Stores peer information in-memory (no database)
- Can forward announces to external trackers (HTTP/HTTPS and UDP) and aggregate peer lists
- Supports BEP 15 UDP tracker protocol (both server and client)
- Exposes Prometheus metrics
- Single binary executable

## Project Structure

The project follows the [Standard Go Project Layout](https://github.com/golang-standards/project-layout):

```
retracker/
├── cmd/retracker/         # Application entry point (package main)
├── internal/              # Private application code
│   ├── config/           # Configuration handling
│   ├── server/           # Server implementation
│   │   ├── core.go       # Core application structure
│   │   ├── storage.go    # In-memory peer storage
│   │   ├── receiverAnnounce.go  # HTTP announce handler
│   │   ├── receiverUDP.go        # UDP announce handler (server-side)
│   │   ├── scrape.go            # Scrape handler
│   │   ├── forwarderManager.go  # Manages forwarding to external trackers
│   │   ├── forwarderStorage.go  # Forwarder state storage
│   │   └── udpForwarder.go      # UDP forwarder client (for UDP trackers)
│   └── observability/    # Prometheus metrics
├── bittorrent/           # BitTorrent protocol modules
│   ├── common/           # Common types (InfoHash, PeerID, Peer, Address)
│   ├── tracker/          # Request parsing/validation
│   └── response/         # Response encoding (bencode + compact format)
├── common/               # Shared common module (Forward struct)
├── scripts/              # Build and utility scripts
├── configs/              # Runtime configuration files
└── docs/                 # Documentation
```

## Key Architectural Patterns

### 1. Core Structure
- **Core** (`core.go`): Central orchestrator containing:
  - `Storage`: In-memory peer storage
  - `ForwarderStorage`: Forwarder-specific storage
  - `ForwarderManager`: Manages forwarding to external trackers
  - `Receiver`: Handles incoming requests (HTTP/UDP)

### 2. Storage Pattern
- **Storage** (`storage.go`): Thread-safe in-memory storage
  - Uses `sync.Mutex` for concurrent access
  - Structure: `map[InfoHash]map[PeerID]Request`
  - Background goroutine (`purgeRoutine`) removes stale peers based on `Config.Age`
  - Always lock with `requestsMu` before accessing `Requests` map

### 3. Forwarder System
- **ForwarderManager** (`forwarderManager.go`): Core forwarder orchestration
  - Uses worker pool pattern (`ForwarderWorkers` config)
  - Manages job scheduling, cancellation, and queue management
  - Handles forwarder enable/disable/suspend logic
  - **Protocol detection**: Automatically detects HTTP/HTTPS vs UDP from forwarder URI
- **ForwarderAnnounce** (`forwarderAnnounce.go`): Unified announce execution logic
  - `executeAnnounce()` routes to appropriate protocol handler
  - `doHTTPAnnounce()` and `doUDPAnnounce()` implement protocol-specific logic
  - `handleAnnounceResult()` processes results uniformly for both protocols
  - Event forwarding (`ForwardStoppedEvent`, `ForwardCompletedEvent`)
- **ForwarderStats** (`forwarderStats.go`): Statistics collection and reporting
  - Implements `StatsDataProvider` interface for observability
  - Records response times with EMA (exponential moving average)
- **UDPForwarder** (`udpForwarder.go`): UDP forwarder client implementation
  - Implements BEP 15 UDP tracker protocol as a client
  - Manages connection IDs with 2-minute lifetime (per BEP 15)
  - Handles connect, announce, and error responses
  - Supports IPv4 and IPv6 peer formats
  - Automatic retry with exponential backoff
- **ForwarderStorage** (`forwarderStorage.go`): Tracks forwarder state and peer mappings

### 4. Request/Response Handling
- **Receiver** (`receiver.go`): Routes requests to appropriate handlers
- **HTTP Handler** (`receiverAnnounce.go`): Processes HTTP announce requests
- **UDP Handler** (`receiverUDP.go`): Processes UDP announce requests (BEP 15)
- **Scrape Handler** (`scrape.go`): Handles scrape requests (BEP 48)
- **Responses**: Use `bittorrent/response` for HTTP/UDP replies (compact and verbose forms). Legacy `bittorrent/tracker/response.go` was removed as dead code.

## Important Conventions

### Thread Safety
- Always use mutex locks when accessing shared data structures
- Storage operations must lock `requestsMu` before accessing `Requests`
- Follow the pattern: `Lock() -> defer Unlock() -> access data`

### Error Handling
- Use `ErrorLog` for error messages: `ErrorLog.Printf(...)` or `ErrorLog.Println(...)`
- Use `DebugLog` for debug messages (only when `Config.Debug == true`)
- Both loggers are defined in `main.go` with `log.Lshortfile` flag

### Configuration
- All configuration is in `Config` struct (`config.go`)
- CLI flags are parsed in `main.go` and passed to `Config`
- Forwarders are loaded from YAML file via `Config.ReloadForwards()`

### BitTorrent Protocol
- See `BITTORRENT.md` for detailed protocol documentation
- Key types: `InfoHash`, `PeerID`, `Peer` (from `bittorrent/common`)
- Requests are built in `bittorrent/tracker/request.go`; responses come from `bittorrent/response`
- Event types: `started`, `completed`, `stopped`, or empty (regular)

## Critical Behaviors

### Event Handling
- **started**: Sent immediately, triggers forwarder announces, continues normal scheduling
- **completed**: Sent immediately once, forwarded to forwarders, does NOT cancel pending jobs
- **stopped**: Sent immediately, forwarded to forwarders, CANCELS pending jobs, no future re-announces
- **Regular (empty)**: Respects `interval`/`min_interval`, normal periodic updates

### Peer Lifecycle
- Peers are stored when announce is received
- Background purge routine removes peers older than `Config.Age` (default 180 minutes)
- Stopped events should immediately remove peers from storage
- Forwarder jobs for stopped peers should be canceled

### Forwarder Behavior
- Forwarders are optional (only if `-f` flag provided)
- ForwarderManager uses worker pool for parallel processing
- Forwarder timeout controlled by `Config.ForwardTimeout` (default 30 seconds)
- Forwarder responses are aggregated with local peers
- **Protocol Support**: 
  - HTTP/HTTPS forwarders: Uses standard HTTP GET requests
  - UDP forwarders: Uses BEP 15 UDP protocol with connection ID management
  - Protocol automatically detected from URI scheme (`http://`, `https://`, `udp://`)
- **UDP Forwarder Features**:
  - Connection ID caching (2-minute lifetime per BEP 15)
  - Automatic connection ID refresh on expiration
  - Transaction ID matching for request/response pairs
  - IPv4/IPv6 peer format detection
  - Retry logic with exponential backoff

## Code Style Guidelines

### Naming
- Use `self` as receiver name (not `this` or single letter)
- Package-level variables use PascalCase (e.g., `ErrorLog`, `DebugLog`)
- Struct fields use PascalCase
- Local variables use camelCase

### Go Modules
- Project uses Go workspaces (`go.work`)
- Multiple modules in `bittorrent/` subdirectories
- Main module `github.com/fl0v/retracker` with local replaces for `bittorrent/*` and `common`

### Dependencies
- External: `gopkg.in/yaml.v2`, `github.com/zeebo/bencode`, `github.com/prometheus/client_golang`
- Internal: `github.com/fl0v/retracker/bittorrent/*`, `github.com/fl0v/retracker/common`

## Testing Considerations

- Storage operations should test thread safety
- Forwarder cancellation on stopped events should be tested
- Age-based purging should be tested with time manipulation
- UDP handler should test connection state management

## Common Tasks

### Adding a New Feature
1. Determine if it belongs in Core, Storage, Receiver, or ForwarderManager
2. Follow existing patterns for thread safety
3. Add appropriate logging (ErrorLog/DebugLog)
4. Update Config if new configuration needed

### Debugging
- Enable debug mode with `-d` flag
- DebugLog will print detailed information about:
  - Storage operations
  - Peer counts per hash
  - Timestamp deltas
  - Lock/unlock operations

### Modifying Forwarder Logic
- Changes should go in `forwarderManager.go` for HTTP or `udpForwarder.go` for UDP
- Consider worker pool impact
- Ensure proper job cancellation for stopped events
- Test with multiple forwarders (both HTTP and UDP)
- For UDP forwarders: Ensure connection ID management follows BEP 15 (2-minute lifetime)
- Protocol detection is automatic via `Forward.GetProtocol()` method

## Important Files Reference

- **docs/BITTORRENT.md**: Comprehensive BitTorrent protocol documentation
- **cmd/retracker/main.go**: Entry point, server setup, flag parsing
- **internal/server/core.go**: Application structure initialization
- **internal/server/storage.go**: Peer storage and purging logic
- **internal/server/forwarderManager.go**: Core forwarder orchestration, queue management, worker pool
- **internal/server/forwarderAnnounce.go**: Unified announce execution logic (HTTP and UDP)
- **internal/server/forwarderStats.go**: Statistics collection and StatsDataProvider implementation
- **internal/server/udpForwarder.go**: UDP forwarder client implementation (BEP 15)
- **internal/server/receiverAnnounce.go**: HTTP announce request processing
- **internal/server/receiverUDP.go**: UDP announce request processing (server-side)
- **common/forwards.go**: Forward configuration with protocol detection

## Version Information

- Current version: `0.10.0` (defined in `main.go`)
- Check version with `-v` flag

## Notes for AI Agents

1. **Always check thread safety**: Any access to `Storage.Requests` must be locked
2. **Respect event semantics**: Event announces (started/completed/stopped) are immediate, regular announces respect intervals
3. **Forwarder jobs**: Understand the difference between canceling jobs (stopped) vs continuing (completed)
4. **Memory management**: This is an in-memory tracker - no persistence, no database
5. **Protocol compliance**: Refer to `BITTORRENT.md` for protocol details and BEP references
6. **Worker pools**: ForwarderManager uses worker pools - be careful with goroutine management
7. **UDP state**: UDP requires connection state tracking (see `tempStorage.go` for server-side, `udpForwarder.go` for client-side)
8. **Protocol detection**: Use `Forward.GetProtocol()` to detect HTTP/HTTPS vs UDP - don't hardcode protocol assumptions
9. **UDP connection IDs**: UDP forwarders cache connection IDs with 2-minute lifetime - ensure proper cleanup and refresh

## Quick Reference: Key Types

```go
// From bittorrent/common
type InfoHash string // must be 20 bytes
type PeerID string   // must be 20 bytes
type Peer struct {
    PeerID PeerID
    IP     Address
    Port   int
}

// From bittorrent/tracker/request.go
type Request struct {
    InfoHash common.InfoHash
    PeerID   common.PeerID
    // ... other fields
    Peer() common.Peer       // returns IP with remoteAddr fallback
    TimeStampDelta() float64 // minutes since timestamp
}

// From bittorrent/response/response.go
type Response struct {
    Interval      int
    MinInterval   int
    Complete      int
    Incomplete    int
    TrackerID     string
    FailureReason string
    Peers         []common.Peer
}
```

## Common Pitfalls to Avoid

1. **Using the wrong response package**: Always use `bittorrent/response` (the old `bittorrent/tracker/response.go` was removed).
2. **Forgetting to lock**: Always lock before accessing `Storage.Requests`.
3. **Not canceling stopped jobs**: Stopped events must cancel pending forwarder jobs.
4. **Ignoring event semantics**: Event announces are immediate, not scheduled.
5. **Race conditions**: Be careful with goroutines and shared state.
6. **Memory leaks**: Ensure purge routine is working and peers are removed on stopped events.
