# Streaming reads

| API / server | Default behavior |
| --- | --- |
| `client.Open(ctx, share, path)` | Incremental `fs.File`; caller closes it |
| `client.ReadFile(...)` | Complete buffered contents, internally using Open |
| `smbhttp` single-file GET with public client | Streaming, unless DisableStreaming is true |
| `smbhttp` with ReadFile-only custom reader | Buffered compatibility fallback |
| Gateway `HTTP_SERVER=nethttp` (default) | Native streaming and disconnect cancellation |
| Gateway `HTTP_SERVER=fiber` | Explicit buffered single-file alternative |
| Batch GET/POST | Buffered Base64 JSON with count and aggregate decoded-content limits |

## Client ownership

```go
file, err := c.Open(ctx, "reports", "large.csv")
if err != nil {
    return err
}
defer file.Close()
_, err = io.Copy(destination, file)
return err
```

Open acquires the client's admission slot and SMB resources, checks metadata and returns without reading file contents. Its timeout continues after return. Close is idempotent and can interrupt a concurrent blocked Read; Read calls are serialized. EOF, read errors, deadline and cancellation also release resources, including when the caller stops reading without closing. Call Close promptly anyway, rather than holding a slot until the deadline.

To interrupt reads reliably, Close cancels the operation and closes its private TCP transport before attempting handle cleanup. Server-side resources are released on disconnect; admission is released once, after local cleanup. Open failures clean up partially acquired resources. Stat returns the opening metadata snapshot and exposes no backend-specific Sys payload.

The default 32 MiB MaxFileBytes policy still applies to streams. Increase it explicitly for larger files. Opening metadata catches known oversize; cumulative byte checks and an extra-byte probe catch growth during reading. A stream can deliver some bytes before an error; only ReadFile guarantees nil data on failure. For disk downloads, use a temporary destination and publish/rename it only after a successful copy. Concurrent file modification is not an atomic snapshot guarantee.

## HTTP behavior

The handler reads at most 512 bytes to sniff MIME, flushes that prefix, then copies with a 32 KiB buffer. The native gateway supplies write/read/idle timeouts and propagates disconnect context. Slow HTTP clients apply socket backpressure; the client operation timeout can reclaim SMB resources even while HTTP output is blocked. Embedders must configure their own server write timeout to bound such blocked writes.

GET omits Content-Length because opening metadata can change; HTTP/1.1 normally uses chunked framing. If an upstream error, timeout or growth limit occurs after response headers, the handler aborts the response rather than appending JSON or completing the framing. A 200 status alone does not mean success: check the body read/copy result. In Go this commonly appears as io.ErrUnexpectedEOF. HTTP middleware must rethrow http.ErrAbortHandler rather than converting it to a normal response.

HEAD opens, reads metadata and closes without reading file data. It reports the opening size with application/octet-stream. Buffered fallback HEAD retains the older complete-read behavior. Range and conditional requests are not implemented.

## Batch limits and memory scope

Default MaxBatchFiles is 100; excessive count is rejected before reads. Default MaxBatchBytes is 32 MiB of decoded contents retained in successful entries. An item that does not fit gets a per-item error while the batch remains HTTP 200. Results retain input order, though concurrent completion determines which items consume the budget. FileOpener-backed batch reads are additionally limited to that per-request byte budget plus one detection byte before buffering an individual entry. Custom ReadFile-only readers must supply their own per-file limits.

Neither batch limit is a process RSS limit: concurrent workers hold temporary buffers, Base64 and JSON encoding allocate copies, queued HTTP requests still consume memory, and multiple clients/replicas have independent limits. Single-file streaming removes whole-file buffers in the native handler, but CloudSoda, TLS and socket buffers remain.

## Verification

Offline tests cover first-prefix delivery while the rest of a source is blocked, disconnect-driven Close, late read errors/panics aborting the HTTP body, HEAD without file reads, fixed-size read buffers, explicit fallback, batch limits, EOF/early Close and concurrent Close/Read. Both HTTP server implementations retain graceful-shutdown tests.

A local Go 1.27.4 Linux amd64 handler-only benchmark used generated data and a discard writer (3 iterations per size):

| Generated response | Allocated bytes/op | Allocations/op |
| --- | ---: | ---: |
| 8 MiB | 34,194 | 16 |
| 64 MiB | 34,194 | 16 |

Reproduce with `go test ./smbhttp -run '^$' -bench BenchmarkStreaming -benchmem -benchtime=3x`. These are handler allocation measurements, not SMB network throughput or whole-process peak RSS.

Real Samba CI includes a 64 MiB encryption-required download verified by SHA-256, HTTP bytes delivered while the upstream transfer remains paused, disconnect and early Close reclaiming the only admission slot, idle deadline reclamation, late upstream failure producing an incomplete HTTP body, and slow HTTP readers not retaining SMB admission beyond timeout. See PR checks for executed results. This does not validate Windows/AD or an actual OpenShift deployment.
