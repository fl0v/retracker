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

## Fast Context Loading (2-minute scan)

If you need fast orientation, read files in this order:

1. `cmd/retracker/main.go` and `cmd/retracker/config.go` (startup, configuration precedence/defaults, HTTP/UDP wiring)
2. `internal/config/config.go` (all config fields + YAML keys)
3. `internal/server/core.go` (how storage/forwarders/receivers are composed)
4. `internal/server/receiverAnnounce.go` (announce flow and response assembly)
5. `internal/server/forwarderManager.go` (queueing, scheduler, throttling, scaling)
6. `internal/server/forwarderAnnounce.go` + `internal/server/udpForwarder.go` (protocol execution)
7. `internal/server/receiverUDP.go` + `internal/server/scrape.go` (UDP BEP 15 + scrape BEP 48)

Useful quick checks:
- `go test ./...`
- `make check`
- `rg -n "QueueEligibleAnnounces|handleStoppedEvent|shouldRateLimitInitial|schedulerRoutine" internal/server/*.go`

## QA Flow (recommended)

Run QA in this order:

1. `make fmt-check`
2. `make lint`
3. `make test`

Full command:
- `make fmt-check && make lint && make test`

Notes:
- `make check` runs only `fmt-check` + `lint` (it does not run tests).
- If cache directories are not writable in restricted environments, run with local cache overrides:
  - `GOCACHE=/tmp/retracker-gocache GOLANGCI_LINT_CACHE=/tmp/retracker-golangci-cache make fmt-check && GOCACHE=/tmp/retracker-gocache GOLANGCI_LINT_CACHE=/tmp/retracker-golangci-cache make lint && GOCACHE=/tmp/retracker-gocache make test`

### QA Tooling Requirements

- Go 1.26+ with the Go 1.27.3 toolchain (see `go.mod` and `go.work`).
- `golangci-lint` v2.15.0 installed and in `PATH` for `make lint`.
  - Install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`
- `gofumpt` is optional; `make fmt`/`make fmt-check` will fall back to `gofmt` if unavailable.

### `go mod tidy` Guidance

- Run `go mod tidy` when dependencies/imports change (new module, removed module, changed imports that affect `go.mod`/`go.sum`).
- Do not run `go mod tidy` as a default QA step for every change.
- If lint says `running 'go mod tidy' may solve the problem`, first verify it is not a cache/permission issue; in restricted environments, setting `GOCACHE`/`GOLANGCI_LINT_CACHE` is often sufficient.

## Runtime Defaults Snapshot (v0.10.0)

Defaults are initialized in `cmd/retracker/config.go`:
- HTTP listen: `:6969`
- UDP listen: disabled (empty)
- Debug: `false`
- Peer age: `180` minutes
- Announce interval: `1800` seconds
- Retry period for busy/rate-limited initial announces: `300` seconds (both response interval fields preserve this period)
- Forward timeout: `30` seconds
- Forwarder workers: `10` (max `20`)
- Forwarder queue size: `10000`
- Queue scale threshold: `60%`
- Queue throttle threshold: `60%` (top `20`)
- Queue rate-limit threshold: `80%` (`0` disables)
- Initial announce token bucket: `10/sec`, burst `200`
- Prometheus metrics: disabled by default

## Endpoint Map

- `GET /announce` -> HTTP announce handler
- `GET /scrape` -> HTTP scrape handler
- `GET /stats` -> text stats (`?format=json` for JSON)
- `GET /metrics` -> Prometheus metrics (only if enabled)

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
  - Peer sessions survive periodic updates and are cancelled on deletion or expiry; forwarding jobs retain their originating session

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
- Runtime forwarder initialization uses the shared `Core.forwarderMu` read/write lock; HTTP/UDP announces, scrape, and stats synchronize reads with publication
- Storage stats suppression uses `atomic.Bool` because the manager can be initialized after startup
- All configuration is in `Config` struct (`config.go`)
- CLI flags are registered and configuration is assembled in `cmd/retracker/config.go`; `main.go` parses flags and starts the server
- Precedence is defaults, YAML, environment, then explicitly supplied flags. False debug values and empty UDP values override YAML; `-c` and `-f` override their environment paths, including empty flags to skip loading
- The Docker entrypoint passes arguments through; the Go binary reads configuration environment variables directly
- Forwarders are loaded from YAML file via `Config.ReloadForwards()`

### BitTorrent Protocol
- See `BITTORRENT.md` for detailed protocol documentation
- Key types: `InfoHash`, `PeerID`, `Peer` (from `bittorrent/common`)
- Requests are built in `bittorrent/tracker/request.go`; responses come from `bittorrent/response`
- Event types: `started`, `completed`, `stopped`, or empty (regular)

## Critical Behaviors

### Event Handling
- **started**: Triggers forwarder announces if eligible (first announce or NextAnnounce due)
- **completed**: Updates local storage and continues normal response flow
- **stopped**: Removes the peer, cancels its queued/scheduled forwarding work and active HTTP requests, and cleans the hash cache when no local peers remain
- **Regular (empty)**: Normal periodic updates, uses `Config.AnnounceInterval`

### Peer Lifecycle
- Peers are stored when announce is received
- Background purge routine removes peers older than `Config.Age` (default 180 minutes)
- Stopped events immediately remove peers and cancel the associated peer session; stale results cannot repopulate the forwarder cache
- Event-forwarding helpers exist in `ForwarderManager`, but are not currently invoked from `ProcessAnnounce`; stopped handling invokes `CancelPendingJobs` directly

### Forwarder Behavior
- Forwarders are optional (loaded with `-f` or added through `/add-forwarder` at runtime)
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
- Receiver names: Use descriptive abbreviations (e.g., `ra` for ReceiverAnnounce, `fm` for ForwarderManager, `fs` for ForwarderStorage). Do not use `self`/`this` in new changes; rename when touched and before committing.
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
- Forwarder queue eligibility, scheduling, and throttling should be tested
- Age-based purging should be tested with time manipulation
- UDP handler should test connection state management
- `make test-cover` tests all five workspace modules and writes a combined statement-coverage profile to `coverage.out` (override with `COVERPROFILE`); root `go test ./...` alone excludes nested modules

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
- **AGENTS.md**: Update this file if architectural patterns, conventions, or key behaviors change
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
- Consider worker pool impact and event-forwarding wiring (`ForwardStoppedEvent` / `ForwardCompletedEvent`)
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
3. **Forwarder jobs**: Check current wiring before assuming stopped/completed are forwarded via event helpers
4. **Memory management**: This is an in-memory tracker - no persistence, no database
5. **Protocol compliance**: Refer to `BITTORRENT.md` for protocol details and BEP references
6. **Worker pools**: ForwarderManager uses worker pools - be careful with goroutine management
7. **UDP state**: UDP requires connection state tracking (see `receiverUDP.go` for server-side, `udpForwarder.go` for client-side)
8. **UDP lifecycle**: Copy packet bytes before launching a handler. `ReceiverUDP.Close` stops packet reads and connection cleanup, waits for handlers, and is safe to repeat.
9. **Protocol detection**: Use `Forward.GetProtocol()` - don't hardcode protocol assumptions
10. **Keep docs updated**: After logic changes, update AGENTS.md and relevant docs. This file must stay accurate.

## Task to File Map (fast edit targeting)

- HTTP announce behavior: `internal/server/receiverAnnounce.go`
- UDP announce/scrape server behavior: `internal/server/receiverUDP.go`
- HTTP/UDP scrape aggregation: `internal/server/scrape.go`
- Local peer storage and aging: `internal/server/storage.go`
- Forwarder eligibility/queueing/scheduler: `internal/server/forwarderManager.go`
- Forwarder protocol execution + retries: `internal/server/forwarderAnnounce.go`
- UDP forwarder transport details: `internal/server/udpForwarder.go`
- Forwarder state (`NextAnnounce`, peer cache): `internal/server/forwarderStorage.go`
- Stats collection/formatting: `internal/server/forwarderStats.go`, `internal/observability/stats.go`
- Prometheus metric definitions: `internal/observability/prometheus.go`
- Forward config schema/protocol detection: `common/forwards.go`

## Current Behavior Clarification (important)

`ForwardStoppedEvent()` and `ForwardCompletedEvent()` exist in `internal/server/forwarderAnnounce.go`, but the current announce path in `internal/server/receiverAnnounce.go` does not call them directly. Current stopped/completed handling is:
- `stopped`: remove peer, cancel its session and pending/scheduled jobs, and clean the forwarder cache if no local peers remain; workers discard stale jobs and results, and HTTP requests use the session context
- `completed`: update local storage and continue normal response flow

If event forwarding behavior is changed, update this file and `docs/ANNOUNCE_LOGIC.md` together.

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
3. **Reusing jobs across peer sessions**: stopped and expired sessions invalidate old jobs, including after the same peer restarts.
4. **Ignoring event semantics**: Event announces are immediate, not scheduled.
5. **Race conditions**: Be careful with goroutines and shared state.
6. **Memory leaks**: Ensure purge routine is working and peers are removed on stopped events.
