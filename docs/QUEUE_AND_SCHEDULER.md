# Queue and Scheduler Implementation

This document describes the internal implementation of the queue, scheduler, and worker pool system.

## Architecture Overview

The system uses a multi-tier architecture:

1. **Job Queue** (`jobQueue`): Buffered channel for immediate execution
2. **Scheduled Jobs Map** (`scheduledJobs`): Jobs scheduled for future execution
3. **Pending Jobs Map** (`pendingJobs`): Tracks jobs to prevent duplicates

## Data Structures

### Job Queue (`jobQueue`)

**Type**: `chan AnnounceJob` (buffered channel)  
**Size**: `Config.ForwarderQueueSize` (default: 10000)

**Characteristics**:
- **FIFO**: First-in-first-out order
- **Non-blocking writes**: Uses `select` with `default` to handle full queue
- **Blocking reads**: Workers block until job available
- **Thread-safe**: Go channels provide built-in safety

### Scheduled Jobs Map (`scheduledJobs`)

**Type**: `map[time.Time][]AnnounceJob`

**Structure**:
- **Key**: `time.Time` - Absolute execution time
- **Value**: `[]AnnounceJob` - Jobs scheduled for that time
- **Thread Safety**: Protected by `scheduledJobsMu` (RWMutex)

### Pending Jobs Map (`pendingJobs`)

**Type**: `map[string]bool`  
**Key Format**: `"infoHash:forwarderName:peerID"` (hex encoded)

**Lifecycle**:
1. **Mark**: When job is created (before queuing/scheduling)
2. **Check**: Before creating new job to prevent duplicates
3. **Unmark**: After job execution completes

**Thread Safety**: Protected by `pendingMu` (Mutex)

## Scheduler Routine

**Function**: `schedulerRoutine()`  
**Frequency**: Every 5 seconds

### Algorithm

```go
Every 5 seconds:
  1. Lock scheduledJobs map
  2. For each scheduledTime in scheduledJobs:
     - If now >= scheduledTime:
       - Add all jobs to readyJobs list
       - Mark time for removal
  3. Remove processed time slots
  4. Unlock map
  5. For each ready job:
     - Try to send to jobQueue (non-blocking)
     - If queue full: reschedule for now + 10 seconds
```

### Queue Full Handling

When `jobQueue` is full during scheduled job transfer:
1. Job is rescheduled for `now + 10 seconds`
2. Scheduler retries in next tick
3. No job loss, just delayed execution

### Early Job Detection

Workers verify job readiness after pulling from queue:

```go
if !job.ScheduledTime.IsZero() && time.Now().Before(job.ScheduledTime):
    scheduleJob(job)  // Not ready, reschedule
    continue
```

Handles edge cases: premature queuing, clock adjustments, reschedules.

## Worker Pool

### Base Workers

**Count**: `Config.ForwarderWorkers`  
**Purpose**: Always-running workers for steady-state load

### Dynamic Scaling

**Function**: `scaleWorkers()`  
**Frequency**: Every 1 second

**Scale Up**:
- Queue fill >= `queueScaleThreshold` (default: 60%)
- Current workers < `maxWorkers`
- Action: Add 1 worker per second

**Scale Down**:
- Queue fill < 40%
- Current workers > base workers
- Action: Signal worker to exit via `scaleDownChan`

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
        executeJob(job)
        unmarkJobPending(job)
```

## Job Deduplication

### Key Generation

**Format**: `"infoHash:forwarderName:peerID"`  
**Example**: `"a1b2c3d4e5f6:tracker1:peer123"`

### Deduplication Points

1. **Before Job Creation**: `isJobPending()` check; if pending → skip
2. **Before Scheduling**: `scheduleJob()` checks for duplicates by InfoHash + ForwarderName
3. **After Execution**: `unmarkJobPending()` allows new jobs for same key

### PeerID in Pending vs Scheduled

- **Pending**: Includes PeerID (different peers can have concurrent jobs)
- **Scheduled**: Ignores PeerID (one scheduled job per hash+forwarder)

## Performance Characteristics

### Memory Usage

- **Job Queue**: `O(queueSize)` - fixed buffer
- **Scheduled Jobs**: `O(scheduled_jobs)` - dynamic
- **Pending Jobs**: `O(active_jobs)` - cleared after execution

### Lock Contention

- **jobQueue**: Lock-free (Go channels)
- **scheduledJobs**: RWMutex (read-heavy)
- **pendingJobs**: Mutex (short critical sections)

### Scheduler Efficiency

- **Tick Interval**: 5 seconds balances responsiveness vs CPU
- **Batch Processing**: All ready jobs processed per tick
- **Time-based Indexing**: O(1) lookup by time

## Configuration Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `ForwarderQueueSize` | 10000 | Job queue buffer size |
| `ForwarderWorkers` | 10 | Base worker count |
| `MaxForwarderWorkers` | 20 | Maximum workers |
| `QueueScaleThresholdPct` | 60 | Queue fill % to scale up |

## Example: Queue Full Scenario

1. **Scheduler** detects ready job in `scheduledJobs`
2. **Try to queue** → `jobQueue` is full
3. **Reschedule**: `newTime = now + 10 seconds`
4. **Next tick** (5s later): retry queuing
5. **If still full**: reschedule again

Ensures no job loss during overload.
