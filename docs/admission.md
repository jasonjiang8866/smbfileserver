# Bounded SMB admission

Use one `client.Client` for callers that should share limits. Both `Open` and
`ReadFile` use the same admission gate; the gateway constructs one client for all
single-file and batch reads, in native and Fiber modes.

| Limit | Go Config field | Gateway environment | Default |
| --- | --- | --- | --- |
| Active operations | `MaxConcurrentReads` | `SMB_MAX_CONCURRENT_READS` | 8 |
| Waiting calls | `MaxQueuedReads` | `SMB_MAX_QUEUED_READS` | 32 |
| Maximum admission wait | `QueueTimeout` | `SMB_QUEUE_TIMEOUT` | 5s |
| Whole operation budget | `Timeout` | `REQUEST_TIMEOUT` | 30s |

Go zero values select defaults; negative values are invalid. Gateway values must
be positive, and durations must not exceed 10 minutes. Neither zero nor negative
values enable unlimited waiting. These are per-client limits, not per-user or
cluster-wide limits. With R replicas, each constructing one client, the configured
maximum active SMB load is R times MaxConcurrentReads.

## What happens under load

1. If an active slot is available, the call proceeds without queueing.
2. Otherwise it reserves one waiting place. If the queue is full, it fails
   immediately with `*client.OpError{Op: "admit", Err: client.ErrBusy}`.
3. A waiter acquires a released active slot, or leaves on cancellation,
   QueueTimeout, or the earlier operation/parent deadline. Queue expiry is
   ErrBusy; parent/operation deadlines remain context.DeadlineExceeded.
4. A successful admission releases its waiting place immediately. The active
   slot stays owned through SMB setup, the stream lifetime and cleanup. Close,
   EOF, read failure or cancellation release it exactly once.

Rejected calls open no SMB connection. Expiring a waiter does not cancel an
existing stream. QueueTimeout only bounds admission; it does not become a
shorter read deadline after admission. Waiting still consumes the total operation
budget, so a 2-second wait within a 30-second operation leaves at most 28 seconds.
A QueueTimeout longer than Timeout is valid; the operation deadline wins.
Admission is not FIFO and has no starvation guarantee.

## Callers and HTTP behavior

```go
file, err := c.Open(ctx, "reports", "report.csv")
if errors.Is(err, client.ErrBusy) {
    // Report local overload, defer the job, or retry under an explicit policy.
    return err
}
if err != nil {
    return err
}
defer file.Close()
_, err = io.Copy(destination, file)
return err
```

Single-file GET/HEAD returns 503 before headers on ErrBusy, with the sanitized
message `SMB reader busy` (HEAD has no body). The handler recognizes the
`Overloaded() bool` marker through wrapped errors and stays standard-library-only.
The same mapping applies to buffered fallback and streaming. A valid batch
continues to return 200 with per-item errors, even when all items are busy.
An item counts toward admission when a batch worker calls Open/ReadFile; the
batch's not-yet-dispatched items do not each occupy a queue place.

No automatic retries occur, and no Retry-After value is promised. If callers
retry read-only operations, use a bounded retry budget with backoff/jitter and
respect their original deadline. Do not create another client to bypass overload.

## Scope and evidence

This gate bounds SMB operations and blocked admission calls, not all HTTP
connections or request goroutines. Requests being parsed, undispatched batch
items, completed buffered responses, socket buffers and slow HTTP writers still
consume resources outside the gate. Batch count/byte limits and server timeouts
remain necessary; use ingress/global HTTP admission for a process-wide request
budget. This feature alone does not establish a total RSS bound or authorization.

Deterministic virtual-time tests exercise a burst of 50 waiting attempts against
three queue places, immediate rejection, cancellation and slot reuse, queue versus
parent deadlines, and admission without shortening an operation's lifetime.
A stalled TCP test checks that repeated queue expiry opens no extra connection.
HTTP contract tests cover GET/HEAD and batch, streaming and buffered readers,
wrapped overload markers and markers returning false. Real Samba CI verifies a
live stream holding the sole active slot, concurrent HTTP overload, handoff after
Close, queue timeout and successful subsequent reads. See PR checks for actual
execution results; this is not Windows/AD or OpenShift deployment validation.
