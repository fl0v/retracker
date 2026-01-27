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
│   ├── server/           # Server implementation (see file headers for details)
│   └── observability/    # Prometheus metrics and statistics
├── bittorrent/           # BitTorrent protocol modules
│   ├── common/           # Common types (InfoHash, PeerID, Peer, Address)
│   ├── tracker/          # Request parsing/validation
│   └── response/         # Response encoding (bencode + compact format)
├── common/               # Shared common module (Forward struct)
├── scripts/              # Build and utility scripts
├── configs/              # Runtime configuration files
└── docs/                 # Documentation
```

Each `.go` file in `internal/server/` has a header comment describing its responsibility.

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
- **Announce Documentation**: See `docs/ANNOUNCE_LOGIC.md` for detailed flow diagram

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
- **started**: Triggers forwarder announces if eligible (first announce or NextAnnounce due)
- **completed**: Forwarded to forwarders, does NOT cancel pending jobs
- **stopped**: Forwarded to forwarders, CANCELS pending jobs, removes peer from storage
- **Regular (empty)**: Normal periodic updates, uses `Config.AnnounceInterval`

### Peer Lifecycle
- Peers are stored when announce is received
- Background purge routine removes peers older than `Config.Age` (default 180 minutes)
- Stopped events should immediately remove peers from storage
- Forwarder jobs for stopped peers should be canceled

### Forwarder Behavior
- Forwarders are optional (only if `-f` flag provided)
- ForwarderManager uses worker pool for parallel processing
- Forwarder timeout controlled by `Config.ForwardTimeout` (default 30 seconds)
- Forwarder responses are aggregated with local peers via `getPeersForResponse()`
  - Returns (seeders, leechers, peers) from both local and forwarder storage
  - Uses IP-based deduplication for seeder/leecher counts
  - Forwarder peers counted as seeders (we lack state info)
- Protocol support: HTTP/HTTPS and UDP (BEP 15), auto-detected from URI scheme
- See Section 3 "Forwarder System" for detailed protocol implementation

## Code Style Guidelines

### Naming
- Receiver names: Use descriptive abbreviations (e.g., `ra` for ReceiverAnnounce, `fm` for ForwarderManager, `fs` for ForwarderStorage). Some older code uses `self`.
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
5. Update documentation (see below)

### Updating Documentation
After any logic changes, update relevant documentation:
- **File headers**: Update the header comment if file responsibility changes
- **AI.md**: Update this file if architectural patterns, conventions, or key behaviors change
- **docs/ANNOUNCE_LOGIC.md**: Update if announce flow, throttling, rate limiting, or suspension logic changes
- **docs/QUEUE_AND_SCHEDULER.md**: Update if queue/scheduler/worker implementation changes

Keep documentation concise and accurate. Remove outdated information rather than letting it accumulate.

### Debugging
- Enable debug mode with `-d` flag
- DebugLog will print detailed information about:
  - Storage operations
  - Peer counts per hash
  - Timestamp deltas
  - Lock/unlock operations

### Modifying Forwarder Logic
- Announce execution logic is in `forwarderAnnounce.go` (both HTTP and UDP)
- Queue/worker management is in `forwarderManager.go`
- UDP-specific protocol handling is in `udpForwarder.go`
- Consider worker pool impact and ensure proper job cancellation for stopped events
- Test with multiple forwarders (both HTTP and UDP)

## Important Files Reference

**Documentation:**
- **docs/ANNOUNCE_LOGIC.md**: announce handling logic and diagram
- **docs/QUEUE_AND_SCHEDULER.md**: Queue and scheduler implementation
- **docs/BITTORRENT.md**: BitTorrent protocol documentation

**Entry Points:**
- **cmd/retracker/main.go**: Application entry point, flag parsing
- **internal/server/core.go**: Central orchestration

**Note:** All `internal/server/*.go` files have header comments describing their responsibilities.

## Version Information

- Current version: `0.10.0` (defined in `main.go`)
- Check version with `-v` flag

## Notes for AI Agents

1. **Always check thread safety**: Any access to `Storage.Requests` must be locked
2. **Respect event semantics**: Event announces (started/completed/stopped) are immediate
3. **Forwarder jobs**: Understand the difference between canceling jobs (stopped) vs continuing (completed)
4. **Memory management**: This is an in-memory tracker - no persistence, no database
5. **Protocol compliance**: Refer to `BITTORRENT.md` for protocol details and BEP references
6. **Worker pools**: ForwarderManager uses worker pools - be careful with goroutine management
7. **UDP state**: UDP requires connection state tracking (see `tempStorage.go` for server-side, `udpForwarder.go` for client-side)
8. **Protocol detection**: Use `Forward.GetProtocol()` - don't hardcode protocol assumptions
9. **Keep docs updated**: After logic changes, update AI.md and relevant docs. This file must stay accurate.

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
    RetryIn       interface{} // BEP 31: int (minutes) or "never"
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
