# Public client: added value and contract

Import `github.com/jasonjiang8866/smbfileserver/client`. `New(Config)` validates an immutable configuration snapshot and returns a concurrent `*Client`. `Open(context.Context, share, file)` returns a streaming `fs.File`; `ReadFile` remains the complete-byte convenience API built on Open. The client implements `smbhttp.FileReader` structurally, and can also be used by a worker or CLI with no HTTP handler.

## What belongs to this project

CloudSoda owns the SMB wire protocol: negotiation, NTLM implementation, signing/encryption and remote file operations. We retain that dependency and its tested version. This package adds the application policy that otherwise has to be repeated around those primitives:

| Added behavior | Observable evidence |
| --- | --- |
| One operation budget includes waiting, connecting, authenticating, reading and cleanup | Queued cancellation does not dial; stalled negotiation ends on timeout and closes TCP |
| One reusable client shares admission across callers for each stream's lifetime | A paused live SMB transfer holds the only configured slot; a second HTTP request times out without opening another SMB connection |
| Per-file size policy covers actual bytes, not just Stat | Known oversize is rejected before content reads; a simulated file growing beyond its advertised size also fails; exact-boundary and empty live Samba files succeed |
| Central ownership of file/share/session/connection cleanup | Cancellation, transport failure and size rejection release the slot; subsequent reads succeed without recreating the client |
| Backend-independent, sanitized errors | `errors.Is` and `errors.As` work without importing CloudSoda; no raw backend messages, usernames, passwords, hosts or paths are returned in library diagnostics |
| Direct composition with the HTTP layer and gateway | Executable constructor/mux example; gateway and Samba tests use the public client rather than a private implementation |

This is not a new SMB protocol implementation or a performance optimization. The package exposes Open/ReadFile and standard metadata, but no writes, directory APIs, arbitrary session handles, protocol knobs or Kerberos configuration. Applications needing those should use CloudSoda's lower-level APIs. Fresh connections cost a negotiation/authentication round trip per file; pooling requires separate measurements and credential/session isolation work.

## Configuration

| Field | Default / requirement |
| --- | --- |
| `Host` | Required bare hostname, IPv4 or IPv6 address; no URL, UNC path or embedded port |
| `Port` | Zero selects 445; otherwise 1–65535 |
| `Username`, `Password` | Required NTLM credentials, supplied by the embedding application |
| `Domain` | Optional NTLM domain; not Kerberos delegation |
| `Timeout` | Zero selects 30 seconds; negative is invalid |
| `MaxConcurrentReads` | Zero selects 8; negative is invalid |
| `MaxFileBytes` | Zero selects 32 MiB; negative or MaxInt64 is invalid |

Use one client for callers that should share a limit. Configuration is copied at construction, so mutating the original Config does not change active or later calls. There is no environment/dotenv loading, listener, signal hook, persistent goroutine or open connection at construction. Credentials remain in memory for future calls; create a new client to rotate them. Do not log or serialize Config: it contains the password. No Client.Close is needed. Open callers close the returned stream; EOF, errors and deadline/cancellation also release it automatically. The zero Client value is not usable and a constructed Client should not be copied.

The Go module still includes gateway dependencies. The client package does not import Fiber, dotenv or smbhttp; CloudSoda and its authentication dependencies remain in its dependency graph. The HTTP package's production dependency graph remains standard-library-only.

## Lifecycle and cancellation

A call creates a timeout context from the caller, waits for a slot, then acquires TCP → session → share → file. A returned stream retains admission until Close, EOF, error or context expiry. Close first closes TCP to interrupt a concurrent stalled Read, then attempts file/share/session cleanup and releases admission exactly once. Open failures clean up the resources acquired so far. The timeout context is propagated to SMB operations and a TCP deadline; cancellation additionally closes TCP to interrupt stalled I/O and cleanup. An earlier parent deadline wins. Callers must supply a non-nil context.

A call uses a fresh connection, with no transparent retries. After a failure, a later call can negotiate again; this is reconnection on the next call, not replay of the failed operation. Avoid constructing a new client on every HTTP request, which would give each request an independent limit.

Native net/http disconnect cancellation flows through smbhttp into the client. Fiber v2 still lacks that signal. Its bridge uses the gateway-owned lifecycle context, avoiding cancellation propagation that outlives fasthttp's pooled RequestCtx. The discovered shutdown race is covered by repeated lifecycle race tests.

## File size and resource scope

`Open` checks opening metadata and limits cumulative reads to the configured maximum, probing one extra byte to detect growth. `ReadFile` consumes that same bounded stream. A successful result has at most `MaxFileBytes` bytes. ReadFile failures return nil data. Open streams can deliver a prefix before failing; callers must check Read/io.Copy errors and close the stream. Stat returns a cached opening snapshot (also after Close), not a fresh server query. EOF and fs.ErrClosed are standard stream results; backend failures retain OpError classification.

The byte cap applies to a single file's contents, not total network traffic, allocation overhead, batch output or process RSS. Batch responses retain completed files and Base64 adds overhead. The active-read cap does not bound the number of waiting callers. The HTTP handler now caps batch file count and retained decoded contents, but transient worker/encoding allocations, queue length, slow-client memory, process-wide policy across multiple clients and replica-wide load still require sizing. Single-file streaming uses fixed-size handler buffers; see [streaming evidence](streaming.md). These limits do not make the gateway production-ready for arbitrary untrusted workloads.

The gateway now defaults to 8 active reads and 32 MiB per file, replacing the previous per-batch-only concurrency and unbounded individual reads. Configure `SMB_MAX_CONCURRENT_READS` and `SMB_MAX_FILE_BYTES` explicitly for another workload. Both environment values must be positive integers; zero is reserved for defaults only in the Go Config API.

## Error contract

Open/ReadFile backend failures return an `*OpError` with a stage such as `admit`, `connect`, `authenticate`, `mount`, `open` or `read`. Its wrapped error is one of:

| `errors.Is` target | Meaning | smbhttp response |
| --- | --- | --- |
| `context.Canceled` | Caller canceled | 503 |
| `context.DeadlineExceeded` | Operation/parent deadline or network timeout | 504 |
| `fs.ErrInvalid` | Invalid input | 400 |
| `fs.ErrNotExist` | Backend reports missing file | 404 |
| `fs.ErrPermission` | Backend reports access denied | 403 |
| `client.ErrTooLarge` | File exceeds configured byte limit | 413 |
| `client.ErrUnavailable` | Other connection, authentication or protocol failure | 502 |

The stage is useful for diagnostics; the error contains no raw backend cause. This intentionally trades detailed vendor status codes for a portable, sanitized public contract. A missing share may still be categorized as ErrUnavailable. ErrUnavailable does not establish that retrying is safe. Batch requests retain HTTP 200 with the corresponding per-file error message.

The ErrTooLarge error exposes `SizeLimitExceeded() bool` so the HTTP package can recognize it with errors.As without importing the client. Custom readers may implement the same marker.

A bare share name is required; UNC share paths and empty/NUL input are rejected. Nested filenames remain supported. This is not path confinement, symlink/DFS/reparse protection or caller authorization. Use authorized share mappings and a dedicated server-side read-only account. Reads do not guarantee a consistent snapshot if another process modifies a file during transfer.

## Validation scope

Public-package tests cover constructor validation, input rejection, config snapshotting, canceled admission, timeout/transport cleanup, slot reuse, backend-neutral errors and client/handler composition. Unit fixtures cover growth after Stat; Samba tests cover actual size boundaries, signed/encrypted shares, HTTP cancellation and shared admission across requests. CI runs Go 1.26/1.27 race tests and the real Samba fixture; current execution results are recorded in the PR checks. Windows SMB/AD/Kerberos and restricted OpenShift deployment are not validated by these fixtures.
