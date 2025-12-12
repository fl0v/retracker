# Queue and Scheduler System

This document describes the queue management and job scheduling system used by retracker's forwarder manager.

## Overview

The forwarder system uses a multi-tier queue architecture to manage announce jobs:

1. **Job Queue** (`jobQueue`): Buffered channel for immediate execution jobs
2. **Scheduled Jobs Map** (`scheduledJobs`): Map of jobs scheduled for future execution
3. **Pending Jobs Map** (`pendingJobs`): Tracks jobs currently in queue or scheduled to prevent duplicates

## Queue Architecture

### 1. Job Queue (`jobQueue`)

**Type**: `chan AnnounceJob` (buffered channel)  
**Size**: `Config.ForwarderQueueSize` (default: 10000)  
**Purpose**: Holds jobs ready for immediate execution by worker pool

**Characteristics**:
- **FIFO**: First-in-first-out order
- **Non-blocking writes**: Uses `select` with `default` to handle full queue
- **Blocking reads**: Workers block on channel receive until job available
- **Thread-safe**: Go channels provide built-in thread safety

**Job Flow**:
```
Job Creation → Check if Scheduled → If Immediate → jobQueue → Worker → Execution
```

### 2. Scheduled Jobs Map (`scheduledJobs`)

**Type**: `map[time.Time][]AnnounceJob`  
**Purpose**: Stores jobs scheduled for future execution, organized by execution time

**Structure**:
- **Key**: `time.Time` - Absolute time when job should execute
- **Value**: `[]AnnounceJob` - Slice of jobs scheduled for that time
- **Thread Safety**: Protected by `scheduledJobsMu` (RWMutex)

**Job Flow**:
```
Job Creation → Check if Scheduled → If Future → scheduledJobs[time] → Scheduler Routine → jobQueue → Worker
```

### 3. Pending Jobs Map (`pendingJobs`)

**Type**: `map[string]bool`  
**Key Format**: `"infoHash:forwarderName:peerID"` (hex encoded)  
**Purpose**: Prevents duplicate jobs for same peer+forwarder+hash combination

**Lifecycle**:
1. **Mark**: When job is created (before queuing or scheduling)
2. **Check**: Before creating new job to prevent duplicates
3. **Unmark**: After job execution completes (success or failure)

**Thread Safety**: Protected by `pendingMu` (Mutex)

## Job Types and Scheduling

### Initial Announcements

**Function**: `QueueEligibleAnnounces()`  
**Scheduling**: **NOT scheduled** - queued immediately

**Logic**:
1. Check if forwarder is disabled/suspended → skip
2. Check `ShouldAnnounceNow()`:
   - Returns `true` if forwarder has never seen this hash
   - Returns `true` if `NextAnnounce` is zero or in the past
   - Returns `false` if `NextAnnounce` is in the future (skip)
3. If eligible, create job with `ScheduledTime = time.Time{}` (zero = immediate)
4. Queue directly to `jobQueue`

**Why Not Scheduled?**
- First time forwarder sees hash → no `NextAnnounce` constraint
- Immediate execution provides fastest peer discovery
- No rate limiting concerns for first announce

### Re-announcements

**Function**: `QueueEligibleAnnounces()`  
**Scheduling**: **NOT scheduled** – same path as initial announces

**Logic**:
1. `ShouldAnnounceNow()` must be true (never seen hash OR `NextAnnounce` is due)
2. If not due → skip forwarder (no job created)
3. If due → create immediate job (`ScheduledTime` zero) and enqueue

**Why Immediate?**
- Single code path for initial and subsequent announces
- Honors per-forwarder `NextAnnounce` via `ShouldAnnounceNow()`
- Avoids duplicate scheduling logic

### Retry Jobs (BEP 31)

**Function**: `handleRetryError()`  
**Scheduling**: **Always scheduled** at tracker-specified retry time

**Logic**:
1. Parse `retry_in` field from forwarder response
2. If `retry_in == "never"` → disable forwarder permanently
3. If `retry_in >= 10 minutes` → treat as completed (don't retry)
4. If `retry_in < 10 minutes`:
   - Calculate `scheduledTime = now + retry_in minutes`
   - Schedule job for `scheduledTime`

**Why Scheduled?**
- Implements BEP 31: Failure Retry Extension
- Respects tracker's requested retry delay
- Prevents immediate retry that would likely fail again

## Scheduler Routine

**Function**: `schedulerRoutine()`  
**Frequency**: Runs every 5 seconds  
**Purpose**: Moves ready scheduled jobs to job queue

### Algorithm

```go
Every 5 seconds:
  1. Lock scheduledJobs map
  2. For each scheduledTime in scheduledJobs:
     - If now >= scheduledTime:
       - Add all jobs at this time to readyJobs list
       - Mark scheduledTime for removal
  3. Remove processed time slots from map
  4. Unlock map
  5. For each ready job:
     - Try to send to jobQueue (non-blocking)
     - If queue full:
       - Reschedule job for now + 10 seconds
       - Add back to scheduledJobs map
```

### Queue Full Handling

When `jobQueue` is full and scheduler tries to move a scheduled job:

1. **Reschedule**: Job is rescheduled for `now + 10 seconds`
2. **Retry**: Scheduler will try again in next tick (5 seconds)
3. **Backpressure**: Prevents losing jobs when system is overloaded

### Early Job Detection

Workers also check if a job pulled from queue is scheduled for future:

```go
if !job.ScheduledTime.IsZero() && time.Now().Before(job.ScheduledTime):
    // Job not ready yet, reschedule it
    scheduleJob(job)
    continue
```

This handles edge cases where:
- Job was queued before its scheduled time
- Clock adjustments occurred
- Job was rescheduled due to queue full

## Worker Pool

### Base Workers

**Count**: `Config.ForwarderWorkers`  
**Purpose**: Always-running workers for steady-state load

### Dynamic Scaling

**Function**: `scaleWorkers()`  
**Frequency**: Checks every 1 second

**Scale Up Conditions**:
- Queue fill percentage >= `queueScaleThreshold` (default: 60%)
- Current workers < `maxWorkers` (default: `ForwarderWorkers * 2`)
- Action: Add 1 worker per second (rate limited)

**Scale Down Conditions**:
- Queue fill percentage < 40%
- Current workers > base workers
- Action: Signal 1 worker to exit via `scaleDownChan`

**Scale Down Mechanism**:
- Workers listen on `scaleDownChan`
- When signaled, worker checks if it's beyond base workers
- If yes, worker exits gracefully
- If no, worker continues (prevents scaling below base)

### Worker Lifecycle

```go
worker():
  while running:
    select:
      case <-stopChan:
        return
      case <-scaleDownChan:
        if currentCount > baseWorkers:
          decrement count
          return
      case job := <-jobQueue:
        if job.ScheduledTime not zero and not ready:
          reschedule job
          continue
        executeAnnounce(job)
        unmarkJobPending(job)
```

## Queue Management Features

### Rate Limiting

**Function**: `shouldRateLimitInitial()`  
**Applies To**: Initial announcements only

**Conditions**:
- Queue fill percentage >= `queueRateLimitThreshold` (default: 80%)
- Token bucket rate limiter active

**Behavior**:
- Rejects new initial announcements with retry message
- Uses `Config.RetryPeriod` as retry interval
- Prevents queue overflow during high load

**Token Bucket**:
- **Rate**: `Config.RateLimitInitialPerSec` tokens/second
- **Burst**: `Config.RateLimitInitialBurst` tokens
- **Refill**: Tokens refill based on elapsed time

### Throttling

**Implemented In**: `QueueEligibleAnnounces()`  
**Applies To**: All announcements (initial and subsequent)

**Conditions**:
- Queue fill percentage >= `queueThrottleThreshold` (default: 60%)
- `queueThrottleTopN` > 0 (default: 20)

**Behavior**:
- Limits forwarders to `queueThrottleTopN` per announce
- Forwarders are shuffled randomly before iteration
- First N eligible forwarders are selected
- Distributes load across forwarders when queue is full

**Why Random Shuffle?**
- Prevents always selecting same forwarders
- Ensures fair distribution of load
- Avoids bias toward first forwarders in config

### Max Forwarders Per Announce

**Config**: `max_forwarders_per_announce` (default: 100)  
**Applies To**: All announcements

**Behavior**:
- Limits forwarders even when not throttling
- When throttling is active, uses `min(max_forwarders_per_announce, queueThrottleTopN)`
- Forwarders are shuffled randomly for fair distribution

### Queue Full Handling

**Initial Announcements**:
- If queue full → reject with retry message
- Client receives error response
- Job is not queued or scheduled

**Scheduled Jobs**:
- If queue full when scheduler tries to move job → reschedule for +10 seconds
- Job is preserved, will retry in next scheduler tick
- No job loss, just delayed execution

**Re-announcements**:
- If queue full → job is dropped
- `unmarkJobPending()` called to allow retry later
- Increments `droppedFullCount` metric

## Job Deduplication

### Key Generation

**Format**: `"infoHash:forwarderName:peerID"`  
**Example**: `"a1b2c3d4e5f6:tracker1:peer123"`

### Deduplication Points

1. **Before Job Creation**:
   - Check `isJobPending()` before creating job
   - If pending → skip job creation

2. **Before Scheduling**:
   - `scheduleJob()` checks for duplicate scheduled jobs
   - Compares `InfoHash` and `ForwarderName` (ignores PeerID for scheduled)
   - If duplicate exists → return false, don't schedule

3. **After Execution**:
   - `unmarkJobPending()` called after job completes
   - Allows new job for same peer+forwarder+hash

### Why PeerID in Pending Key?

- Different peers can announce same hash to same forwarder
- Each peer needs its own job (different peer_id in request)
- Prevents one peer's job from blocking another's

### Why No PeerID in Scheduled Deduplication?

- Scheduled jobs are for re-announcements
- Re-announcements use cached request (same peer_id)
- Only one scheduled job per hash+forwarder needed

## Metrics and Monitoring

### Queue Metrics

- **Queue Depth**: Current number of jobs in `jobQueue`
- **Queue Capacity**: Maximum size of `jobQueue`
- **Queue Fill Percentage**: `(depth / capacity) * 100`

### Worker Metrics

- **Active Workers**: Current number of running workers
- **Max Workers**: Maximum allowed workers

### Drop Counters

- **Dropped Full**: Jobs dropped when queue full
- **Rate Limited**: Initial announcements rejected by rate limiter

### Scheduled Jobs

- **Pending Count**: Number of jobs in `pendingJobs` map
- **Scheduled Announces**: List of next 10 scheduled jobs with `TimeToExec`

## Configuration Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `ForwarderQueueSize` | 10000 | Size of job queue buffer |
| `ForwarderWorkers` | 10 | Base number of workers |
| `MaxForwarderWorkers` | 20 | Maximum workers for scaling |
| `QueueScaleThresholdPct` | 60 | Queue fill % to trigger scale up |
| `QueueRateLimitThreshold` | 80 | Queue fill % to enable rate limiting |
| `QueueThrottleThreshold` | 60 | Queue fill % to enable throttling |
| `QueueThrottleTopN` | 20 | Number of forwarders to use when throttling |
| `MaxForwardersPerAnnounce` | 100 | Max forwarders per announce (even when not throttling) |
| `RateLimitInitialPerSec` | 100 | Tokens per second for rate limiter |
| `RateLimitInitialBurst` | 200 | Burst size for rate limiter |

## Performance Considerations

### Memory Usage

- **Job Queue**: `O(queueSize)` - fixed size buffer
- **Scheduled Jobs**: `O(scheduled_jobs)` - grows with scheduled jobs
- **Pending Jobs**: `O(active_jobs)` - cleared after execution

### Lock Contention

- **jobQueue**: No locks (Go channels are lock-free)
- **scheduledJobs**: RWMutex (read-heavy, write-light)
- **pendingJobs**: Mutex (short critical sections)

### Scheduler Efficiency

- **Tick Interval**: 5 seconds balances responsiveness vs CPU
- **Batch Processing**: Processes all ready jobs in one tick
- **Time-based Indexing**: O(1) lookup by time (map key)

## Example Flow

### Scenario: Client announces, forwarder needs re-announce

1. **Client Request** → `handleSubsequentAnnounce()`
2. **Eligibility** → `QueueEligibleAnnounces()` checks `ShouldAnnounceNow()`
   - If `NextAnnounce` is due → enqueue immediate job
   - If not due → skip forwarder (no job)
3. **Worker**:
   - Pulls job from `jobQueue`
   - Executes announce to forwarder
   - `MarkAnnounced` advances `NextAnnounce`
   - Unmarks from `pendingJobs`

### Scenario: Queue is full, scheduled job ready

1. **Scheduler Routine** detects job ready
2. **Try to queue** → `jobQueue` is full
3. **Reschedule**:
   - Calculate `newTime = now + 10 seconds`
   - Add job back to `scheduledJobs[newTime]`
4. **Next Tick** (5 seconds later):
   - Job still in `scheduledJobs`
   - Try again to queue
   - If still full → reschedule again

This ensures no job loss, just delayed execution until queue has capacity.

