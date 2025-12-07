# BitTorrent Protocol Specification

This document summarizes the BitTorrent announce protocol logic as implemented in `retracker`, with references to official BitTorrent Enhancement Proposals (BEPs).

## Announce Request Parameters

When a BitTorrent client communicates with a tracker, it sends an HTTP GET or UDP request containing the following parameters:

**Required Parameters:**
- `info_hash`: 20-byte SHA-1 hash of the torrent's info dictionary, uniquely identifying the torrent
- `peer_id`: 20-byte unique identifier for the client
- `port`: Port number the client is listening on for incoming peer connections
- `uploaded`: Total number of bytes uploaded since the client started (cumulative)
- `downloaded`: Total number of bytes downloaded since the client started (cumulative)
- `left`: Number of bytes remaining to download (0 if seeding)

**Optional Parameters:**
- `event`: Client's current state - `started`, `stopped`, `completed`, or empty (regular update)
- `numwant`: Number of peers requested (typically 50, default if not specified)
- `ip`: Client's IP address (if different from connection IP)
- `compact`: Request compact peer list format (1 for compact, 0 or absent for dictionary format)
- `key`: Random value to prevent spoofing
- `no_peer_id`: Omit peer_id in compact peer list responses (BEP 23)
- `trackerid`: Tracker-assigned identifier for subsequent requests

**Important Note:**
- `uploaded`, `downloaded`, and `left` are included on **ALL** announces, including event announces (`started`, `completed`, `stopped`). This is critical for private trackers that use these values for ratio accounting.

**References:**
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html)
- [BEP 15 - UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html) (for UDP announces)

## Peer Identification

The `peer_id` field is a 20-byte identifier that uniquely identifies a client in the swarm. Beyond its primary purpose, the `peer_id` also encodes information about the client software and version, which trackers can decode to identify which BitTorrent client is being used.

### Client Identification Methods

Trackers can identify client software through multiple methods:

#### 1. HTTP/HTTPS Trackers

**a) User-Agent Header (Common)**
- Most BitTorrent clients send an HTTP `User-Agent` header with announce/scrape requests
- Example: `User-Agent: qBittorrent/4.6.3`
- Easy to parse but can be spoofed or missing
- **Note:** UDP trackers do not have HTTP headers, so User-Agent is not available

**b) peer_id Encoding (Always Present, More Reliable)**
- Every announce request must include a 20-byte `peer_id`
- The `peer_id` embeds client type and version using conventions from BEP 3 and BEP 20
- More reliable than User-Agent because it's always present (both HTTP and UDP)
- Harder to spoof without modifying client code

#### 2. UDP Trackers

- No HTTP headers (no User-Agent available)
- `peer_id` is still present in the binary packet
- Client identification relies solely on `peer_id` decoding

### peer_id Encoding Formats

#### Azureus-Style Encoding (Most Common)

The majority of modern BitTorrent clients use Azureus-style encoding (also known as Azureus-style peer ID):

**Format:** `-XX####-xxxxxxxxxxxx`

Where:
- `-` = Leading dash (required)
- `XX` = Two-character client identifier
- `####` = Four-character version encoding
- `-` = Separator dash
- `xxxxxxxxxxxx` = Random characters

**Examples:**
- `-qB4610-xxxxxxxxxxxx` → qBittorrent 4.6.10
- `-TR300Z-xxxxxxxxxxxx` → Transmission 3.0.0
- `-LT2210-xxxxxxxxxxxx` → libtorrent 2.2.10
- `-UT3530-xxxxxxxxxxxx` → µTorrent 3.5.30

**Version Decoding:**
- Numeric format: `4610` → `4.6.10` (major.minor.patch)
- Alphanumeric format: `300Z` → `3.0.0` (letters are ignored in version parsing)

**Common Client IDs:**
| Client ID | Client Name |
|-----------|-------------|
| `qB` | qBittorrent |
| `TR` | Transmission |
| `LT` | libtorrent |
| `UT` | µTorrent |
| `BT` | BitTorrent |
| `DE` | Deluge |
| `AZ` | Azureus/Vuze |
| `BC` | BitComet |
| `KT` | KTorrent |

#### Shadow-Style Encoding (Older Format)

Some older clients use Shadow-style encoding where the first character(s) identify the client:

- `M` = Mainline BitTorrent
- `ex` = BitComet
- `FC` = FileCroc
- `FD` = Free Download Manager

#### BitComet Style

BitComet also uses a format starting with `exbc`:
- `exbc...` = BitComet

### Implementation in retracker

The `retracker` implementation decodes client information from `peer_id` using the following priority:

1. **Azureus-style decoding** (checks for leading dash and 2-character client ID)
2. **Shadow-style decoding** (checks for known prefixes)
3. **BitComet style** (checks for `exbc` prefix)
4. **Fallback to User-Agent** (if peer_id decoding fails and User-Agent is available)
5. **Unknown** (if all methods fail)

This approach ensures:
- **Reliability:** Works for both HTTP and UDP trackers
- **Accuracy:** peer_id encoding is standardized and harder to spoof
- **Compatibility:** Falls back to User-Agent when peer_id cannot be decoded

### Use Cases

**Private Trackers:**
- Detect banned or unsupported clients
- Identify client spoofing attempts
- Enforce client whitelist policies
- Monitor client distribution in the swarm

**Statistics and Monitoring:**
- Track which clients are most popular
- Monitor client version distribution
- Identify outdated or vulnerable client versions
- Generate client usage reports

**References:**
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html) (peer_id field definition)
- [BEP 20 - Peer ID Conventions](https://www.bittorrent.org/beps/bep_0020.html) (peer_id encoding conventions)
- [BEP 15 - UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html) (UDP peer_id format)

## Announce Response

The tracker responds with a bencoded dictionary (HTTP) or binary packet (UDP) containing:

**Response Fields:**
- `interval`: Number of seconds the client should wait before sending the next regular announce request (typically 15-30 minutes)
- `min interval`: Optional lower bound on announce interval (clients should not announce more frequently than this)
- `peers`: List of peer addresses (IP:port) in dictionary or compact format
- `peers6`: IPv6 peer list in compact binary format (BEP 7)
- `complete`: Number of seeders (optional, may be in scrape response)
- `incomplete`: Number of leechers (optional, may be in scrape response)
- `downloaded`: Number of times the torrent has completed (optional, may be in scrape response)

**References:**
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html)
- [BEP 23 - Tracker Returns Compact Peer Lists](https://www.bittorrent.org/beps/bep_0023.html) (for compact format)

## Event Types

BitTorrent clients use the `event` parameter to indicate significant state changes:

- **`started`**: Sent when the client begins downloading a torrent (first announce)
- **`completed`**: Sent once when the client finishes downloading the entire torrent (becomes a seeder)
- **`stopped`**: Sent when the client stops downloading or seeding (leaving the swarm)
- **Empty/None**: Regular periodic update with no specific event

**Event Behavior:**
- **Event announces (`started`, `completed`, `stopped`) should be sent immediately**, not delayed by the tracker's `interval`. They are one-time notifications that bypass normal interval scheduling.
- **Regular announces (no event)** respect the tracker's `interval` / `min interval` and are sent periodically.

**Specific Event Details:**
- `started`: Sent immediately when torrent starts (transition from not-announced to active). Triggers initial forwarder announces, normal announce scheduling continues after this.
- `completed`: Sent immediately once when download finishes (transition from leecher to seed). Client continues to announce periodically. Should be forwarded to forwarders but does NOT cancel pending jobs.
- `stopped`: Sent immediately when client is leaving the swarm (torrent stop/remove, or client shutdown). Should be forwarded to forwarders immediately. Pending jobs for this peer should be canceled. No future re-announces scheduled.
- **Regular (empty)**: Normal periodic update, respects `interval` / `min interval`, continues normal announce scheduling

**UDP Event Values (BEP 15):**
- `0`: None (regular update)
- `1`: Completed
- `2`: Started
- `3`: Stopped

**References:**
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html)
- [BEP 15 - UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html) (event field values: 0=none, 1=completed, 2=started, 3=stopped)

## Scraping

Scraping is a lightweight query mechanism that allows clients to obtain swarm statistics without announcing their participation.

**What Scrape Returns:**
- `complete`: Number of seeders
- `incomplete`: Number of leechers
- `downloaded`: Total number of completed downloads (if tracked by tracker)

**When Scraping is Executed:**
- Before adding a torrent: To check swarm health before starting download
- Periodically: To monitor swarm status without announcing
- On demand: When user requests statistics

**Scrape Behavior:**
- Scrapes are informational queries that **do not register you as a peer** and **do not affect ratio**
- They can be executed **immediately** without waiting for announce intervals
- The tracker's `interval` value (from announce responses) does **NOT apply to scrapes**
- Scrapes are client-initiated based on client's own logic
- Clients must still obey **local rate limits** (per-tracker and global) to avoid hammering the tracker
- qBittorrent uses scrapes mainly to refresh swarm statistics shown in the UI and sometimes when a torrent is added or the user manually requests tracker updates

**References:**
- [BEP 48 - Tracker Protocol Extension: Scrape](https://www.bittorrent.org/beps/bep_0048.html)
- [BEP 15 - UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html) (UDP scrape action)

## Tracker Tiers

BitTorrent supports multiple trackers organized into tiers (BEP 12).

**How Tracker Tiers Work:**
- Trackers are organized into tiers (groups) in the torrent metadata
- All trackers in a tier are equivalent
- A client must try **all trackers in a tier** before falling back to the next tier
- Within a tier, the order can be shuffled and successful trackers promoted
- Tiers are processed in priority order (tier 0, then tier 1, etc.)

**qBittorrent/libtorrent Behavior:**
qBittorrent exposes libtorrent's tracker strategy via advanced options that affect tier behavior:

- **"Always announce to all trackers in a tier"**
  - **Enabled:** announce to **every** tracker in the same tier in parallel
  - **Disabled (spec-compliant):** announce to **one** tracker per tier; other trackers in the tier are used as fallbacks if the chosen one fails

- **"Always announce to all tiers"**
  - **Enabled (uTorrent-style):** announce to **one tracker from each tier** in parallel
  - **Disabled (spec-compliant):** only use higher tiers if all trackers in a lower tier have failed

**Example:**
```yaml
announce-list:
  - [tracker1, tracker2]  # Tier 0
  - [tracker3, tracker4]  # Tier 1
```

**References:**
- [BEP 12 - Multitracker Metadata Extension](https://www.bittorrent.org/beps/bep_0012.html)

## Protocol Expectations

### Stopped Events

- Clients should send `stopped` events **immediately** when stopping a torrent, removing it, or exiting
- Stopped events are **one-time notifications** (no periodic scheduling) and should be sent immediately, not delayed
- Include the current `uploaded`, `downloaded`, and `left` values in the stopped request
- If a client stops very shortly after starting (quick stop), it should still send the stopped event
- Trackers should forward stopped events to forwarders immediately
- Pending announce jobs for stopped peers should be canceled
- **qBittorrent behavior:** When removing an active torrent, it normally sends `event=stopped` to each tracker. The "Stop tracker timeout" advanced option controls how long qBittorrent waits for these stopped announces. If set to 0, qBittorrent will not send stopped announces at all.

### Completed Events

- Clients send `completed` event once when download finishes
- Unlike stopped, completed events indicate the client is now a seeder and will continue participating
- Completed events should be forwarded to forwarders (one-time notification)
- Pending jobs should **NOT** be canceled (client continues)
- Normal announce scheduling continues after completed event

### Quick Stop After Start

If a client sends `started` and then immediately sends `stopped` (within seconds):
- The stopped event should still be forwarded to forwarders
- Any pending/queued announce jobs for that peer should be canceled
- No future re-announces should be scheduled

**References:**
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html)

## Summary of Key Protocol Points

- **Regular announces** respect the tracker's `interval` / `min interval`; **event announces (`started`, `completed`, `stopped`) and scrapes are sent immediately** (subject to local rate limiting)
- `uploaded`, `downloaded`, and `left` are included on **ALL** announces, including event announces
- A well-behaved client sends `event=stopped` immediately when a torrent is stopped/removed or on clean exit, so trackers can drop the peer quickly
- qBittorrent's multi-tracker behavior depends on the "Always announce to all trackers in a tier" and "Always announce to all tiers" options; disabling them moves it closer to the BEP 12 spec semantics

## Additional References

### BitTorrent Enhancement Proposals (BEPs)

- [BEP 0 - Index of BitTorrent Enhancement Proposals](https://www.bittorrent.org/beps/bep_0000.html)
- [BEP 3 - BitTorrent Protocol Specification](https://www.bittorrent.org/beps/bep_0003.html) (core protocol, peer_id, announce parameters)
- [BEP 7 - IPv6 Tracker Extension](https://www.bittorrent.org/beps/bep_0007.html) (IPv6 peer lists)
- [BEP 12 - Multitracker Metadata Extension](https://www.bittorrent.org/beps/bep_0012.html) (tracker tiers)
- [BEP 15 - UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html) (UDP tracker implementation)
- [BEP 20 - Peer ID Conventions](https://www.bittorrent.org/beps/bep_0020.html) (peer_id encoding standards)
- [BEP 23 - Tracker Returns Compact Peer Lists](https://www.bittorrent.org/beps/bep_0023.html) (compact peer format)
- [BEP 27 - Private Torrents](https://www.bittorrent.org/beps/bep_0027.html) (private tracker behavior)
- [BEP 48 - Tracker Protocol Extension: Scrape](https://www.bittorrent.org/beps/bep_0048.html) (scrape protocol)

### External Resources

- [qBittorrent GitHub Repository](https://github.com/qbittorrent/qBittorrent/)
- [qBittorrent Wiki](https://github.com/qbittorrent/qBittorrent/wiki/)
- [BitTorrent.org BEP Index](https://www.bittorrent.org/beps/)

