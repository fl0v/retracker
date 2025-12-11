# Job Queue Memory Usage Analysis

## Current Memory Footprint

### Per-Job Memory Estimate

Each `AnnounceJob` contains:
- `InfoHash`: 20 bytes (fixed)
- `ForwarderName`: ~20-50 bytes (string, variable)
- `PeerID`: 20 bytes (fixed)
- `Forwarder`: ~100-200 bytes (struct with Name, Uri, Ip, Host strings)
- `Request`: ~200-400 bytes (tracker.Request with all fields including strings)

**Total per job: ~340-690 bytes** (average ~500 bytes)

### Queue Memory Usage

| Queue Size | Memory Usage (approx) |
|------------|----------------------|
| 1,000 (default) | ~500 KB |
| 5,000 | ~2.5 MB |
| 10,000 | ~5 MB |
| 50,000 | ~25 MB |
| 100,000 | ~50 MB |

## Current Behavior on Startup

When retracker starts:
1. Each client's first announce goes through `QueueEligibleAnnounces()`
2. Jobs are only queued for forwarders where `ShouldAnnounceNow()` is true (never seen hash or NextAnnounce is due)
3. If 100 clients connect simultaneously with unique hashes and 5 forwarders:
   - Up to **500 jobs** may be created, but skips occur when forwarders aren't due
   - With default queue (1000), this uses ~250 KB
4. If queue fills up, initial announces are rejected with a retry; scheduled jobs are rescheduled

## Considerations

1. **Backpressure**: Initial announces are rate-limited and may be rejected when the queue is full
2. **Memory vs. Throughput**: Larger queue = more memory but better burst handling
3. **Prioritization**: Initial and re-announce jobs share the same queue; scheduling defers non-urgent work

## Alternatives to Increasing Queue Size

### 1. **Rate Limiting / Throttling** (Recommended)

Limit the rate at which jobs are queued, especially for initial announces:

```go
// Add to ForwarderManager
type rateLimiter struct {
    tokens chan struct{}
    ticker *time.Ticker
}

// Limit to X jobs per second
func (fm *ForwarderManager) rateLimitInitialAnnounces() {
    // Only allow N initial announces per second
    // Queue others with slight delay
}
```

**Pros:**
- Prevents queue overflow
- Smooths out bursts
- Predictable memory usage

**Cons:**
- Adds latency for some announces
- More complex implementation

### 2. **Batching / Deduplication**

Group similar announces together:

```go
// Instead of: 1 job per (infoHash, forwarder, peerID)
// Use: 1 job per (infoHash, forwarder) with multiple peerIDs
type BatchedAnnounceJob struct {
    InfoHash      common.InfoHash
    ForwarderName string
    Requests      []tracker.Request  // Multiple peers
}
```

**Pros:**
- Reduces queue size significantly
- Fewer HTTP requests (can batch)
- Better memory efficiency

**Cons:**
- More complex job processing
- May delay some announces

### 3. **Backpressure Mechanism**

Instead of dropping jobs, apply backpressure:

```go
// Blocking queue with timeout
select {
case fm.jobQueue <- job:
    // Success
case <-time.After(100 * time.Millisecond):
    // Queue full - retry with exponential backoff
    go fm.retryJobWithBackoff(job)
}
```

**Pros:**
- No silent failures
- Jobs eventually processed
- Better reliability

**Cons:**
- Can cause memory buildup if workers are slow
- More complex retry logic

### 4. **Adaptive Queue Sizing**

Dynamically adjust queue size based on load:

```go
// Monitor queue fill rate
// If consistently > 80% full, increase size
// If consistently < 20% full, decrease size
func (fm *ForwarderManager) adjustQueueSize() {
    currentFill := float64(len(fm.jobQueue)) / float64(cap(fm.jobQueue))
    if currentFill > 0.8 {
        // Increase queue (requires channel recreation)
    }
}
```

**Pros:**
- Adapts to actual load
- Efficient memory usage

**Cons:**
- Complex to implement (channels can't be resized)
- Requires queue recreation

### 5. **Priority Queue**

Prioritize initial announces over re-announces:

```go
type Priority int
const (
    PriorityHigh Priority = iota  // Initial announces
    PriorityNormal                 // Re-announces
)

type PriorityJob struct {
    Priority Priority
    Job      AnnounceJob
}
```

**Pros:**
- Important jobs processed first
- Better user experience

**Cons:**
- More complex queue management
- Re-announces may be delayed

### 6. **Worker Scaling**

Increase workers instead of queue size:

```go
// More workers = faster processing = smaller queue needed
// Current: 10 workers (default)
// Alternative: Scale workers based on queue depth
```

**Pros:**
- Faster processing
- Smaller queue needed
- Better CPU utilization

**Cons:**
- More goroutines/connections
- May hit connection limits
- Higher CPU usage

## Current Defaults (implemented)
- Queue size: 10,000 (configurable via flag/env)
- Worker scaling: enabled; scales when queue > ~60% (configurable)
- Forwarder throttling: keeps fastest top-N when queue > ~60% (configurable, default top 20)
- Rate limiting: enabled when queue > ~80% (configurable); **set threshold to 0 to disable**
- Overload suspension: forwarder suspended on overload responses (e.g., HTTP 429) for configurable duration (default 300s)

## Recommended Operational Settings
- Queue: 5,000–10,000 for moderate bursts
- Scale threshold: 50–70% based on desired responsiveness
- Throttle top-N: 10–30 depending on forwarder quality variance
- Rate limit: 100/sec, burst 200; disable by setting threshold to 0 if you prefer no client-visible retry hints
- Suspension: 300–600s for overload (429) responses

## Memory vs. Performance Trade-offs

| Approach | Memory | Latency | Complexity | Reliability |
|----------|--------|---------|------------|-------------|
| Large Queue (50K) | High (~25MB) | Low | Low | Medium |
| Rate Limiting | Low | Medium | Medium | High |
| Throttling (fastest top-N) | Low | Medium | Medium | High |
| Backpressure | Medium | High | Medium | High |
| Priority Queue | Medium | Low | High | High |

## Monitoring Recommendations

Add metrics to track:
- Queue depth (current/max)
- Queue fill rate
- Dropped jobs count
- Average job wait time
- Worker utilization

These metrics should be exposed via `/stats` endpoint and Prometheus (if enabled).

