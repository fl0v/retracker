# AI Indications for retracker

This file provides guidance for AI agents working on the retracker codebase. It documents key architectural patterns, conventions, and important considerations.

## Project Overview

**retracker** is a lightweight BitTorrent tracker written in Go that:
- Receives and processes BitTorrent announce/scrape requests (HTTP and UDP)
- Stores peer information in-memory (no database)
- Can forward announces to external trackers and aggregate peer lists
- Exposes Prometheus metrics
- Single binary executable

## Project Structure

```
retracker/
├── main.go                 # Entry point, CLI flags, HTTP server setup
├── config.go              # Configuration struct and forwarder loading
├── core.go                # Core application structure (Storage, ForwarderManager, Receiver)
├── storage.go             # In-memory peer storage with age-based purging
├── tempStorage.go         # Temporary storage (likely for UDP connection state)
├── receiver.go            # Main receiver structure
├── receiverAnnounce.go    # HTTP announce handler
├── receiverUDP.go         # UDP announce handler
├── scrape.go              # Scrape handler
├── forwarderManager.go    # Manages forwarding announces to external trackers
├── forwarderStorage.go    # Storage for forwarder-related data
├── prometheus.go          # Prometheus metrics integration
└── bittorrent/            # BitTorrent protocol packages
    ├── common/            # Common types (InfoHash, PeerID, Peer, Address)
    ├── tracker/           # Tracker request/response types
    └── response/          # Response encoding (bencode, compact format)
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
- **ForwarderManager** (`forwarderManager.go`): Handles forwarding announces to external trackers
  - Uses worker pool pattern (`ForwarderWorkers` config)
  - Manages job scheduling and cancellation
  - Handles event types (started, completed, stopped) differently
- **ForwarderStorage** (`forwarderStorage.go`): Tracks forwarder state and peer mappings

### 4. Request Handling
- **Receiver** (`receiver.go`): Routes requests to appropriate handlers
- **HTTP Handler** (`receiverAnnounce.go`): Processes HTTP announce requests
- **UDP Handler** (`receiverUDP.go`): Processes UDP announce requests (BEP 15)
- **Scrape Handler** (`scrape.go`): Handles scrape requests (BEP 48)

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
- Request/Response types from `bittorrent/tracker`
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

## Code Style Guidelines

### Naming
- Use `self` as receiver name (not `this` or single letter)
- Package-level variables use PascalCase (e.g., `ErrorLog`, `DebugLog`)
- Struct fields use PascalCase
- Local variables use camelCase

### Go Modules
- Project uses Go workspaces (`go.work`)
- Multiple modules in `bittorrent/` subdirectories
- Main module in `retracker/` directory

### Dependencies
- External packages: `gopkg.in/yaml.v2` for YAML parsing
- Internal packages: `github.com/vvampirius/retracker/bittorrent/*`
- Internal packages: `github.com/vvampirius/retracker/common`

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
- Changes should go in `forwarderManager.go`
- Consider worker pool impact
- Ensure proper job cancellation for stopped events
- Test with multiple forwarders

## Important Files Reference

- **BITTORRENT.md**: Comprehensive BitTorrent protocol documentation
- **main.go**: Entry point, server setup, flag parsing
- **core.go**: Application structure initialization
- **storage.go**: Peer storage and purging logic
- **forwarderManager.go**: Complex forwarding logic with job scheduling
- **receiverAnnounce.go**: HTTP announce request processing

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
7. **UDP state**: UDP requires connection state tracking (see `tempStorage.go`)

## Quick Reference: Key Types

```go
// From bittorrent/common
type InfoHash [20]byte
type PeerID [20]byte
type Peer struct {
    IP   net.IP
    Port uint16
}

// From bittorrent/tracker
type Request struct {
    InfoHash common.InfoHash
    PeerID   common.PeerID
    // ... other fields
    Peer() common.Peer
    TimeStampDelta() time.Duration
}
```

## Common Pitfalls to Avoid

1. **Forgetting to lock**: Always lock before accessing `Storage.Requests`
2. **Not canceling stopped jobs**: Stopped events must cancel pending forwarder jobs
3. **Ignoring event semantics**: Event announces are immediate, not scheduled
4. **Race conditions**: Be careful with goroutines and shared state
5. **Memory leaks**: Ensure purge routine is working and peers are removed on stopped events
