# Embedding `smbhttp`

`New(reader FileReader, options Options) (*Handler, error)` creates a concurrent `http.Handler`. A public `*client.Client` implements this reader interface directly. The handler receives an already initialized reader; it does not own or close that reader. A reader must support concurrent calls and stop promptly on context cancellation. The optional FileOpener capability returns a streaming fs.File; it is preferred for HTTP/1.1+ single-file GET and metadata-only HEAD responses. HTTP/1.0 GET uses buffered ReadFile. ReadFile-only readers remain buffered. The public contract exposes no Fiber or CloudSoda types.

## Mounting and routes

Mount with `mux.Handle("/files/", http.StripPrefix("/files", h))`. Routes below are relative to that prefix and match case-sensitively:

| Method | Relative route | Response |
| --- | --- | --- |
| GET / HEAD | `/{share}/{file}` | HTTP/1.1+ GET streams sniffed bytes; HTTP/1.0 GET buffers; HEAD uses metadata (see below) |
| GET / HEAD | `/{share}/batch?files=a,b&concurrency=3` | Legacy Base64 batch JSON |
| POST | `/{share}/batch` | Base64 batch JSON from `BatchFileRequest` |

With FileOpener, HEAD reads metadata only, returning opening Content-Length and generic `application/octet-stream`. HTTP/1.1+ GET sniffs the first 512 bytes, then copies with a 32 KiB buffer; it does not promise Content-Length from mutable metadata. HTTP/1.0 GET buffers before sending headers, returning complete JSON on upstream failure and an exact Content-Length on success. HTTP/1.0 HEAD still uses metadata only when Open is enabled. Buffered fallback HEAD still performs a complete read. Unknown paths return 404; unsupported methods return 405 with Allow. No hello/health endpoint is installed by this package; the gateway supplies `/api/v1/hello`.

Streaming failures before headers use normal JSON error mapping. After headers, a failure aborts HTTP framing with `http.ErrAbortHandler`; middleware must preserve that sentinel. Clients must check body-read errors rather than treating 200 as proof of a complete download. The handler closes streams on every path. See [streaming contract](streaming.md).

A single-file URL occupies one decoded path segment. Nested names and names containing commas belong in POST batch JSON. URL paths are decoded once by net/http; the handler does not perform another URL-unescape. The exact name `batch` is reserved; read such a file through the batch payload. Empty segments, extra segments and trailing slashes do not match. Mounting behind a ServeMux may additionally apply that mux's canonical-path redirects.

The host must authenticate and authorize callers and constrain shares/paths before backend access. A URL prefix is not a filesystem confinement boundary. This extraction intentionally does not claim traversal, symlink, DFS or reparse-point protection.

## Options and cancellation

- `RequestTimeout`: zero selects 30 seconds; a negative value is invalid. The request context is retained, including middleware values and any earlier deadline. It covers the whole batch, including queued files.
- `MaxBodyBytes`: zero selects 4 MiB; a negative value is invalid. POST requires `application/json`, accepts exactly one JSON value, and returns 413 for an oversized input body. Unknown JSON fields remain accepted for compatibility. Media types previously accepted incidentally by Fiber's generic BodyParser, such as form/XML bodies, are now rejected with 415.
- `DisableStreaming`: false by default; true explicitly uses ReadFile for single-file requests. FileOpener can still be used internally to bound buffered batch reads.
- `MaxBatchFiles`: default 100. Excess count is rejected with 413 before backend access.
- `MaxBatchBytes`: default 32 MiB of decoded contents retained across successful batch entries. Excess items receive a per-file `batch byte limit exceeded` error. Completion order determines budget allocation; result order remains input order. Negative values (and MaxInt64 for the byte budget) are invalid.
- A nil reader is invalid. As with standard Go interfaces, callers must also avoid supplying a typed nil pointer as a reader.

The host server owns HTTP header/body-read/write/idle timeouts and graceful shutdown. The handler cannot forcibly stop a reader that ignores its context, and its context timeout alone cannot interrupt a stalled HTTP body read. Set server read timeouts. No background worker persists after a request completes.

Native net/http request cancellation, including a disconnected HTTP client, reaches the reader. The Fiber gateway uses a thin adapter with an application-owned shutdown context, avoiding asynchronous cancellation against Fiber's pooled RequestCtx. Fiber v2 cannot supply the same disconnect signal; the adapter does not promise it.

## Results and limits

`FileRequest`, `BatchFileRequest`, `FileResponse` and `BatchFileResponse` describe the existing JSON contract. Batch output retains order and duplicates, defaults to 3 workers and caps requested concurrency at 10. Canceled queued work does not call the reader. A valid batch returns 200 with per-item errors, even if all reads fail. A backend panic becomes a sanitized 500 for a single read, or a per-item generic error inside a batch.

Wrap `fs.ErrNotExist`, `fs.ErrPermission`, `context.Canceled` or `context.DeadlineExceeded` to preserve HTTP classification. `fs.ErrInvalid` maps to 400. An error implementing `SizeLimitExceeded() bool` and returning true maps to 413; the public client uses that marker for `client.ErrTooLarge`, preserving the handler's standard-library-only production dependency tree. Network timeouts map to 504; unknown backend errors map to a generic 502. Raw backend text is never included in HTTP errors.

HTTP/1.1+ single-file GET responses stream when FileOpener is available and streaming is enabled. HTTP/1.0 GET buffers with memory proportional to file size and retains the reader's per-file limits, request timeout and native disconnect cancellation. Batch responses and ReadFile-only fallback are buffered; Base64 expands batch memory use. Body size is an input limit, not an output/file limit. Range/conditional requests, global request/queue admission and authentication remain separate work. The public client supplies per-instance active-read and queue caps, a maximum admission wait and a per-file byte cap when injected; a custom reader defines its own resource policy. The dependency boundary and lifecycle contract are intended to support those additions without binding importers to the gateway.

## Verification

- External-package tests import only the public API and exercise mounting, MIME/binary/HEAD responses, batch ordering, errors, malformed/oversized input, middleware context values, deadlines, queued cancellation, panic containment and real HTTP socket disconnects.
- Existing Fiber gateway route and graceful-shutdown tests run unchanged in intent against the shared handler.
- CI enforces the standard-library-only production dependency tree of `smbhttp`, recursively formats all Go packages, and runs Go 1.26/1.27 race tests, vet, builds and vulnerability scans.
- The Samba job includes an actual native HTTP disconnect after a transfer has reached 64 KiB, checks cancellation reaches CloudSoda, then reads from an encryption-required share through the same handler/reader. Consult the PR's CI run for executed results; Windows/AD and OpenShift are still separate acceptance work.

## Overload classification

Errors wrapping an `Overloaded() bool` marker returning true map to HTTP 503 with `SMB reader busy` before response headers. This covers the public client's `ErrBusy` (full queue or queue wait timeout) and custom readers without adding an import dependency. GET and HEAD use the same classification in streaming and buffered modes; HEAD omits the body. Valid batches keep HTTP 200 with this message on each rejected item. Parent/operation deadlines remain 504; caller cancellation remains 503 with `request canceled`. There is no automatic retry or Retry-After promise. See [admission](admission.md).

## Optional access policy

The bare handler remains authentication-neutral. For the provided service-token and logical-share policy, inject `access.Policy` as the reader and wrap the mounted handler with `Policy.Wrap`. Both steps are required. The standalone gateway requires this policy at startup; embeds may use their own authentication/authorization. See [access control and path limits](access-control.md).
