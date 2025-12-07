# BitTorrent Tracker Announces, Scrapes, and qBittorrent Behaviour

## 1. Tracker announces: what is sent and received

### 1.1 Sent fields

For HTTP/UDP trackers a client sends an **announce** request containing (at minimum):

- `info_hash` – 20-byte SHA-1 of the torrent's `info` dict  
- `peer_id` – 20-byte client identifier  
- `port` – TCP port the client listens on  
- `uploaded` – total bytes uploaded so far for this torrent  
- `downloaded` – total bytes downloaded so far for this torrent  
- `left` – bytes still missing (0 when complete)  
- `event` – optional: `started`, `completed`, `stopped`  
- `ip` – optional explicit IP override  
- `numwant` – number of peers requested  
- `compact`, `no_peer_id`, `key`, `trackerid` – optional extensions

**Normal periodic announces** (no `event` parameter) still include `uploaded`, `downloaded` and `left`. This is what most private trackers use for ratio accounting.

### 1.2 Response fields

Trackers answer with a bencoded dictionary that typically contains:

- `interval` – seconds until the next regular announce  
- `min interval` – optional lower bound on announce interval  
- `peers` / `peers6` – list of peers (dictionary or compact binary form)  
- `complete` – number of seeds  
- `incomplete` – number of leechers  
- `downloaded` – number of times the torrent has completed

qBittorrent (via libtorrent-rasterbar) uses this to schedule announces and to populate the “Seeds” / “Peers” columns.

---

## 2. Special events vs interval

There are two classes of announces:

1. **Regular announces** – respect the `interval` / `min interval` returned by the tracker.  
2. **Event announces (`started`, `completed`, `stopped`)** – should be sent **immediately**, not delayed to the next interval.

Practical behaviour:

- `event=started` – sent as soon as the torrent starts (transition from not-announced to active).  
- `event=completed` – sent once, right when the last piece is downloaded (transition from leecher to seed).  
- `event=stopped` – sent as soon as the client knows it is leaving the swarm (torrent stop/remove, or client shutdown for active torrents).

These event announces ignore the normal `interval`, but a sane client still rate-limits across torrents to avoid spamming trackers.

---

## 3. Scrapes

A **scrape** is a separate request (usually by replacing `announce` with `scrape` in the URL) that returns only aggregate stats:

- `complete` – seeds  
- `incomplete` – leechers  
- `downloaded` – completed downloads

Key points:

- Scrapes **do not register you as a peer** and do **not** affect ratio.  
- Scrapes are usually sent **immediately when needed**, independent of the announce `interval`.  
- Clients must still obey **local rate limits** (per-tracker and global) so they do not hammer the tracker with scrapes.

qBittorrent uses scrapes mainly to refresh swarm statistics shown in the UI and sometimes when a torrent is added or the user manually requests tracker updates.

---

## 4. What happens when a torrent is stopped, removed, or the client exits

### 4.1 Stopping/removing a torrent

Correct protocol behaviour (BEP 3) is:

- Send an announce with `event=stopped` **immediately** when leaving the swarm.  
- Include the current `uploaded`, `downloaded`, and `left` values in that request.

qBittorrent behaviour (via libtorrent):

- When you **remove** an active torrent (or stop it from a running state), it normally sends `event=stopped` to each tracker for that torrent.  
- The **"Stop tracker timeout"** advanced option controls how long qBittorrent waits for these `stopped` announces.  
  - If this timeout is **0**, qBittorrent will **not send `stopped` announces at all** (it just disconnects and lets trackers time out your peer).

### 4.2 Paused torrents

- Pausing a torrent in qBittorrent **does not send `event=stopped`**.  
- The tracker will keep your peer entry until its normal timeout expires.  
- This can cause “ghost peers” on private trackers if you pause/exit on one machine and immediately start seeding the same torrent from another.

### 4.3 Client exit

On exit qBittorrent tries to:

- Send `event=stopped` announces for **active (running) torrents**, again controlled by the "Stop tracker timeout".  
- It does **not** send `stopped` for paused torrents.

From the tracker’s perspective, the cleanest behaviour is:

- On exit, send `event=stopped` to all trackers for all **actively announced** torrents (as long as you can do so quickly).  
- If you cannot (crash, forced kill, timeout set to 0), trackers will simply time out the peer entry.

---

## 5. Tracker tiers and qBittorrent settings

### 5.1 Tiers in the `.torrent`

The `announce-list` (BEP 12) is a list of lists:

```text
[
  [ tracker1, tracker2 ],    # tier 0
  [ backup1 ],               # tier 1
  [ backup2, backup3 ]       # tier 2
]
```

Rules:

- All trackers in a **tier** are equivalent.  
- A client must try **all trackers in a tier** before falling back to the next tier.  
- Within a tier, the order can be shuffled and successful trackers promoted.

### 5.2 qBittorrent/libtorrent options

qBittorrent exposes libtorrent's tracker strategy via advanced options:

- **“Always announce to all trackers in a tier”**  
  - **Enabled:** announce to **every** tracker in the same tier in parallel.  
  - **Disabled:** announce to **one** tracker per tier; other trackers in the tier are used as fallbacks if the chosen one fails.

- **“Always announce to all tiers”**  
  - **Enabled (uTorrent-style):** announce to **one tracker from each tier**.  
  - **Disabled (spec-style):** only use higher tiers if all trackers in a lower tier have failed.

### 5.3 Multiple trackers in one tier with "one tracker per tier"

If you place multiple trackers in the same tier and **disable** "Always announce to all trackers in a tier":

- qBittorrent/libtorrent will choose **one tracker** from that tier for regular announces.  
- If that tracker fails or times out, it will try the others in the same tier and promote a working one.  
- The other trackers in the tier are effectively **fallbacks/load-balancers**, not all hit on every announce.

---

## 6. Summary

- Regular announces respect the tracker's `interval`; **event announces and scrapes are sent immediately** (subject to local rate limiting).  
- `uploaded`, `downloaded`, and `left` are included on **all** announces, including event announces.  
- A well-behaved client sends `event=stopped` immediately when a torrent is stopped/removed or on clean exit, so trackers can drop the peer quickly.  
- qBittorrent's multi-tracker behaviour depends on the "Always announce to all trackers in a tier" and "Always announce to all tiers" options; disabling them moves it closer to the BEP 12 spec semantics.

---

## 7. References

```text
[1] BitTorrent Protocol Specification (BEP 3)
    https://www.bittorrent.org/beps/bep_0003.html

[2] BitTorrent Extension Proposals Index
    https://www.bittorrent.org/beps/bep_0000.html

[3] Multitracker Metadata Extension (BEP 12)
    https://www.bittorrent.org/beps/bep_0012.html

[4] HTTP/HTTPS Scrape Support (BEP 48)
    https://www.bittorrent.org/beps/bep_0048.html

[5] BitTorrent official website
    https://www.bittorrent.org/

[6] qBittorrent official website
    https://www.qbittorrent.org/

[7] qBittorrent GitHub repository
    https://github.com/qbittorrent/qBittorrent/

[8] qBittorrent Wiki (Advanced Settings, trackers and scraping behaviour)
    https://github.com/qbittorrent/qBittorrent/wiki/
```
