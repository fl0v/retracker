# retracker Announcement Flow Diagram

This document contains a Mermaid diagram illustrating how retracker handles BitTorrent announcements and interacts with forwarders.

```mermaid
flowchart TD
    Start([BitTorrent Client<br/>HTTP Announce Request]) --> Parse[Parse Request<br/>info_hash, peer_id, event, etc.]
    Parse --> EventCheck{Event Type?}
    
    %% Stopped Event Path
    EventCheck -->|stopped| StoppedPath[handleStoppedEvent]
    StoppedPath --> CancelJobs[Cancel Pending Jobs<br/>for this peer]
    CancelJobs --> ForwardStopped[Forward Stopped Event<br/>to All Forwarders<br/>Parallel Execution]
    ForwardStopped --> DeletePeer[Delete Peer from<br/>Local Storage]
    DeletePeer --> CheckLocal{Local Peers<br/>Remaining?}
    CheckLocal -->|No| CleanupForwarder[Cleanup ForwarderStorage<br/>for info_hash]
    CheckLocal -->|Yes| StoppedResponse[Return Response<br/>with interval]
    CleanupForwarder --> StoppedResponse
    StoppedResponse --> End1([Response to Client])
    
    %% Completed Event Path
    EventCheck -->|completed| CompletedPath[handleCompletedEvent]
    CompletedPath --> UpdateStorage1[Update Local Storage]
    UpdateStorage1 --> GetPeers1[Get Peers from<br/>Local + Forwarder Storage]
    GetPeers1 --> CalcInterval1[Calculate Interval<br/>from Forwarder Avg]
    CalcInterval1 --> ForwardCompleted[Forward Completed Event<br/>to All Forwarders<br/>Parallel Execution]
    ForwardCompleted --> CacheRequest1[Cache Request]
    CacheRequest1 --> CheckReannounce1[Check and Re-announce<br/>if needed]
    CheckReannounce1 --> CompletedResponse[Return Response<br/>with peers + interval]
    CompletedResponse --> End2([Response to Client])
    
    %% Regular/Started Event Path
    EventCheck -->|started/empty| RegularPath[handleRegularAnnounce]
    RegularPath --> UpdateStorage2[Update Local Storage]
    UpdateStorage2 --> GetLocalPeers[Get Local Peers]
    GetLocalPeers --> FirstCheck{First Announce<br/>for info_hash?}
    
    %% First Announce Path
    FirstCheck -->|Yes| FirstAnnounce[handleFirstAnnounce]
    FirstAnnounce --> GetCachedPeers1[Get Cached Forwarder Peers<br/>usually empty]
    GetCachedPeers1 --> CacheRequest2[Cache Request]
    CacheRequest2 --> TriggerInitial[Trigger Initial Announce<br/>to All Forwarders]
    TriggerInitial --> QueueJobs1[Queue Jobs to<br/>Worker Pool]
    QueueJobs1 --> FirstResponse[Return Response<br/>interval=15s]
    FirstResponse --> End3([Response to Client])
    
    %% Subsequent Announce Path
    FirstCheck -->|No| SubsequentAnnounce[handleSubsequentAnnounce]
    SubsequentAnnounce --> GetCachedPeers2[Get Cached Forwarder Peers]
    GetCachedPeers2 --> CalcInterval2[Calculate Interval<br/>from Forwarder Avg]
    CalcInterval2 --> CacheRequest3[Cache Request]
    CacheRequest3 --> CheckReannounce2[Check and Re-announce<br/>Compare Intervals]
    CheckReannounce2 --> IntervalCheck{Client Interval<br/>vs Forwarder Interval?}
    
    IntervalCheck -->|Client > Forwarder| ImmediateReannounce[Queue Immediate<br/>Re-announce Jobs]
    IntervalCheck -->|Client < Forwarder| ScheduledReannounce[Queue Scheduled<br/>Re-announce Jobs]
    IntervalCheck -->|Equal| NoReannounce[No Re-announce<br/>Needed]
    
    ImmediateReannounce --> QueueJobs2[Queue Jobs to<br/>Worker Pool]
    ScheduledReannounce --> QueueJobs2
    NoReannounce --> SubsequentResponse[Return Response<br/>with peers + interval]
    QueueJobs2 --> SubsequentResponse
    SubsequentResponse --> End4([Response to Client])
    
    %% Forwarder Worker Pool Processing
    QueueJobs1 -.->|Job Queue| WorkerPool[Worker Pool<br/>N Workers]
    QueueJobs2 -.->|Job Queue| WorkerPool
    
    WorkerPool --> Worker[Worker Picks Job]
    Worker --> ExecuteAnnounce[executeAnnounce]
    ExecuteAnnounce --> BuildURI[Build Forwarder URI<br/>with request params]
    BuildURI --> HTTPRequest[HTTP GET Request<br/>to Forwarder<br/>with Timeout]
    HTTPRequest --> ResponseCheck{Response<br/>Status?}
    
    ResponseCheck -->|200 OK| ParseResponse[Parse Bencoded<br/>Response]
    ResponseCheck -->|Error| ErrorHandling[Log Error<br/>Update with Empty Peers<br/>Interval=60s]
    
    ParseResponse --> UpdateForwarderStorage[Update ForwarderStorage<br/>with Peers + Interval]
    UpdateForwarderStorage --> RecordStats[Record Statistics<br/>Response Time, Interval]
    RecordStats --> UnmarkJob[Unmark Job as Pending]
    ErrorHandling --> UnmarkJob
    UnmarkJob --> WorkerDone([Worker Ready<br/>for Next Job])
    
    %% Background Processes
    StoragePurge[Background Purge Routine<br/>Every 1 Minute] --> CheckAge{Peer Age<br/>> Config.Age?}
    CheckAge -->|Yes| RemovePeer[Remove Stale Peer]
    CheckAge -->|No| KeepPeer[Keep Peer]
    RemovePeer --> CheckEmpty{Hash Empty?}
    CheckEmpty -->|Yes| RemoveHash[Remove Hash Entry]
    CheckEmpty -->|No| StoragePurge
    RemoveHash --> StoragePurge
    KeepPeer --> StoragePurge
    
    %% Styling
    classDef eventPath fill:#ffcccc,stroke:#ff0000,stroke-width:2px
    classDef storagePath fill:#ccffcc,stroke:#00ff00,stroke-width:2px
    classDef forwarderPath fill:#ccccff,stroke:#0000ff,stroke-width:2px
    classDef decision fill:#ffffcc,stroke:#ffaa00,stroke-width:2px
    
    class StoppedPath,CompletedPath,RegularPath eventPath
    class UpdateStorage1,UpdateStorage2,DeletePeer,StoragePurge storagePath
    class ForwardStopped,ForwardCompleted,TriggerInitial,ExecuteAnnounce,WorkerPool forwarderPath
    class EventCheck,FirstCheck,IntervalCheck,ResponseCheck,CheckAge decision
```

## Key Components

### 1. Event Handling
- **stopped**: Immediately forwards to all forwarders, cancels pending jobs, deletes peer
- **completed**: Forwards event, updates storage, continues normal flow (doesn't cancel jobs)
- **started/empty**: Normal announce flow with first/subsequent distinction

### 2. Storage Systems
- **Local Storage**: Thread-safe in-memory storage of peers (map[InfoHash]map[PeerID]Request)
- **ForwarderStorage**: Caches peers and intervals from forwarder responses
- **Background Purge**: Removes peers older than Config.Age (default 180 minutes)

### 3. Forwarder System
- **Worker Pool**: Parallel processing of forwarder requests (Config.ForwarderWorkers)
- **Job Queue**: Buffered channel for announce jobs
- **Re-announcing Logic**:
  - If client interval > forwarder interval → immediate re-announce
  - If client interval < forwarder interval → scheduled re-announce
  - Jobs are deduplicated (pending job tracking)

### 4. Response Generation
- **First Announce**: Returns interval=15s, triggers initial forwarder announces
- **Subsequent Announces**: Returns average interval from forwarders, includes cached forwarder peers
- **Peer Aggregation**: Combines local peers + forwarder peers in response

## Flow Characteristics

1. **Non-blocking**: Forwarder operations don't block client responses
2. **Parallel Execution**: Stopped/completed events sent to all forwarders in parallel
3. **Deduplication**: Prevents duplicate jobs for same peer+forwarder+hash
4. **Interval Management**: Dynamically adjusts based on forwarder responses
5. **Error Handling**: Failed forwarder requests don't affect client response
