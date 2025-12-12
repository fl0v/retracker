# Queue, Throttling, Rate Limiting, and Suspension

This document describes how the forwarder job queue, worker scaling, throttling, rate limiting, and forwarder suspension work.

## Configuration (flags/env)
- Queue size: `-Q` / `FORWARDER_QUEUE_SIZE` (default: 10000)
- Workers (base/max): `-w` / `FORWARDER_WORKERS`, `-W` / `MAX_FORWARDER_WORKERS`
- Worker scale threshold (% full): `--queue-scale-threshold` / `QUEUE_SCALE_THRESHOLD` (default: 60)
- Throttle threshold & top-N: `--queue-throttle-threshold` / `QUEUE_THROTTLE_THRESHOLD` (default: 60), `--queue-throttle-top` / `QUEUE_THROTTLE_TOP` (default: 20)
- Rate-limit threshold (% full): `--queue-rate-limit-threshold` / `QUEUE_RATE_LIMIT_THRESHOLD` (default: 80; **set to 0 to disable**)
- Rate-limit tokens/sec & burst: `--rate-limit-initial-ps` / `RATE_LIMIT_INITIAL_PER_SEC` (default: 100), `--rate-limit-initial-burst` / `RATE_LIMIT_INITIAL_BURST` (default: 200)
- Forwarder suspension duration (overload/429): `--forwarder-suspend` / `FORWARDER_SUSPEND_SECONDS` (default: 300s)

## Queue Flow
- Incoming announces enqueue jobs to eligible forwarders (up to `max_forwarders_per_announce`, default 100).
- Forwarders are shuffled randomly before selection to distribute load evenly.
- When queue fill exceeds the throttling threshold, forwarder count is limited to `queue_throttle_top_n` (default 20).
- When queue fill exceeds the rate-limit threshold, initial announces are rate limited by a token bucket; limited requests receive a tracker failure with a short retry hint. If the threshold is 0, rate limiting is disabled and no client-facing retry hint is sent.
- If the queue is full, jobs are dropped and logged; counters increment (`dropped_full`). Clients are not explicitly informed of queue-full drops.

## Worker Scaling
- A background scaler monitors queue depth and spawns extra workers when fill percentage exceeds the scale threshold, up to `MAX_FORWARDER_WORKERS`. Worker count is reflected in stats and Prometheus gauges.

## Forwarder Suspension
- Overload responses (currently HTTP 429) trigger a suspension for the configured duration. Suspended forwarders are skipped for new announcements until the suspension expires automatically.
- Suspension uses the generic helper `shouldSuspendForwarder(statusCode, err)` so additional overload signals can be added centrally.

## Retries to Forwarders
- HTTP forwarder requests use the configured retry attempts/base backoff (`ForwarderRetryAttempts`, `ForwarderRetryBaseMs`, `ForwardTimeout`). Non-retryable errors or tracker rejections disable the forwarder; 429 triggers suspension instead.
- UDP forwarder requests are also retried based on the same config.

## Metrics & Stats
- `/stats` (text & JSON) includes: queue depth/capacity/fill %, active/max workers, dropped_full, rate_limited, throttled_forwarders.
- Prometheus exports gauges/counters: `queue_depth`, `queue_capacity`, `queue_fill_pct`, `queue_dropped_full`, `queue_rate_limited`, `queue_throttled_forwarders`, `forwarder_workers`, plus existing forwarder_status/request metrics.

## Client-Facing Behavior
- Rate-limited initial announces return a tracker failure reason: “tracker busy, retry in 10 seconds” with a short interval.
- When rate limiting is disabled (threshold=0), no special failure is sent; queue-full drops remain server-side only.

## Tests
- See `internal/server/forwarderManager_test.go` for coverage of:
  - Rate-limit threshold disabled and active cases
  - Throttle limits forwarder count
  - Max forwarders per announce limit
  - Skip recently announced forwarders
  - Suspension expiration
- See `internal/server/forwarderAnnounce_test.go` for coverage of:
  - Announce result handling (success, failure, suspend)
  - Job pending/cancellation logic
  - HTTP status code handling

