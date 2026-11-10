# SMB File Server

A read-only **SMB client toolkit and SMB-to-HTTP gateway** written in Go. Applications fetch files from an existing Windows/Samba share through HTTP without mounting SMB inside their containers. The long-term target is Kubernetes and OpenShift application workloads.

This repository currently provides the reliability foundation, **not a production deployment**. Authentication, share/path authorization, total-response/queue limits, streaming and container manifests are planned in [the roadmap](docs/roadmap.md). Until those controls exist, run only in a trusted development environment with a dedicated read-only SMB account. CI includes an isolated Samba interoperability fixture; Windows SMB, AD/Kerberos and actual OpenShift deployment remain unverified.

## Added value over CloudSoda

CloudSoda implements SMB negotiation, authentication, signing/encryption and file operations. This project adds an application-level contract around those primitives:

| Need | What this project supplies |
| --- | --- |
| Read one file without managing session handles | `client.ReadFile(ctx, share, file)` owns and releases the connection, session, share and file |
| Bound a whole operation | One timeout covers admission, dialing, authentication, reads and cleanup; cancellation closes transport I/O |
| Limit upstream load across callers | One reusable client limits active reads across all requests (default **8**); expired queued calls do not dial |
| Avoid unbounded individual file reads | A **32 MiB** default limit checks both metadata and actual bytes, including files that grow after Stat; failures return no partial contents |
| Handle failures without backend-specific types | `errors.Is` categories and sanitized `OpError` identify the failing stage without leaking server paths or credentials |
| Embed or deploy the same logic | Compose `client` + `smbhttp`, or run the existing Fiber gateway, backed by the same tests |

These are lifecycle, resource-policy and integration benefits. SMB protocol features come from CloudSoda; this project does not claim faster transfer speeds, new cryptography or broader protocol coverage. Reads currently establish fresh sessions and buffer complete files. See [client semantics and tradeoffs](docs/client.md).

## Use the client directly

```go
import (
    "context"
    "github.com/jasonjiang8866/smbfileserver/client"
)

// credentials is supplied by your application's secret mechanism.
c, err := client.New(client.Config{
    Host: "files.example.test",
    Username: credentials.Username,
    Password: credentials.Password,
    MaxConcurrentReads: 8,
    MaxFileBytes: 32 << 20,
})
if err != nil {
    return err
}
data, err := c.ReadFile(context.Background(), "reports", "nested/report.csv")
```

Construct once and reuse `c` across callers so they share its admission limit. `New` validates configuration without contacting SMB. Each read releases its resources; there is no persistent session or `Close` method. Credentials are fixed at construction; create a new client to rotate them. The package does not load env/dotenv or import Fiber/the HTTP handler. See the [compilable examples](client/example_test.go).

## Embed the standard HTTP handler

Import `github.com/jasonjiang8866/smbfileserver/smbhttp` to embed the file API in an existing Go service. The package and its production dependency tree use only the standard library; importing it does not compile Fiber, CloudSoda or dotenv. The repository still uses one Go module, so those gateway dependencies remain in its `go.mod`.

Pass a `*client.Client` directly, or provide your own implementation of the small interface below. The gateway injects the same public client through this boundary.

```go
type FileReader interface {
    ReadFile(ctx context.Context, share, file string) ([]byte, error)
}
```

Given an initialized `reader` (for example, `c` above), mount the handler in your application's mux:

```go
handler, err := smbhttp.New(reader, smbhttp.Options{
    RequestTimeout: 30 * time.Second,
})
if err != nil {
    return err
}
mux.Handle("/files/", http.StripPrefix("/files", handler))
// GET /files/reports/hello.txt
// POST /files/reports/batch
```

The host application owns its server, middleware, credentials and reader lifecycle. The handler starts no listener, reads no environment variables and installs no signal hooks. Wrap it with your application's authentication and share/path authorization. See the [executable example](smbhttp/handler_test.go) and [embedding contract](docs/http-handler.md).

The existing Fiber gateway now adapts this handler and preserves its canonical `/api/v1/smb/...` endpoints and JSON response shape. Standard `net/http` hosting propagates request cancellation and disconnects to the reader; the Fiber v2 adapter retains shutdown/timeout cancellation but does not gain client-disconnect detection.

## Build and run

Use Go 1.26 or newer. Dependencies and checksums are committed. The SMB backend is `github.com/cloudsoda/go-smb2`, pinned to `v0.0.0-20261105190815-95be04a983b8`; the original `hirochachacha/go-smb2` module is no longer used. See [migration notes](docs/cloudsoda-migration.md).

```sh
go mod download
go test -race ./...
go build -o smbfileserver .
```

Set the connection environment variables using your environment's secret mechanism, then run `./smbfileserver` or `go run .`. A local `.env` copied from `.env.example` is optional; it is read once at startup. Missing `.env` is normal. A malformed or unreadable file is a startup error. Environment values override dotenv values, including explicit empty values, and credentials are never included in configuration errors.

The previous `go run main.go readremotefile.go` command no longer includes all source files; use `go run .`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SMB_HOST` | Required | Bare hostname or IPv4/IPv6 address, without protocol or port |
| `SMB_PORT` | `445` | SMB TCP port |
| `SMB_USERNAME` | Required | Service account username |
| `SMB_PASSWORD` | Required | Service account password |
| `SMB_DOMAIN` | Empty | Optional NTLM domain; this is not Kerberos delegation |
| `HTTP_ADDR` | `:3000` | HTTP listener address; use `127.0.0.1:3000` for local-only access |
| `REQUEST_TIMEOUT` | `30s` | Total SMB operation budget for a single or batch HTTP request, including queued batch work |
| `SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight HTTP requests after SIGINT/SIGTERM |
| `SMB_MAX_CONCURRENT_READS` | `8` | Maximum active reads shared across the gateway's one client, including cleanup; waiting uses the request timeout |
| `SMB_MAX_FILE_BYTES` | `33554432` | Maximum contents of one file (32 MiB); an oversized file is rejected without partial output |

Timeouts must be positive durations of at most `10m`. Legacy `serverIP`, `userName` and `userPassword` remain accepted. Canonical environment names take precedence over legacy environment names; either environment form takes precedence over either dotenv form. The old unused `serverName` setting remains unused. Configuration changes require restarting the process. Both resource-limit environment values must be positive integers; the byte limit must be less than MaxInt64. These defaults change the previous unbounded-file/per-batch-only behavior; raise the file limit explicitly for trusted larger files.

## API

- `GET /api/v1/hello` — process-only hello response; does not check SMB availability.
- `GET /api/v1/smb/:shareName/:fileName` — one file as raw bytes, with content-sniffed MIME type and `X-Content-Type-Options: nosniff`. This route accepts one filename segment, not a nested URL path.
- `POST /api/v1/smb/:shareName/batch` — multiple files as Base64 JSON.
- `GET /api/v1/smb/:shareName/batch?files=one.png,two.png&concurrency=3` — legacy query form; filenames containing commas require POST.

The literal GET `batch` path is reserved. To read a file named `batch`, use POST batch with that filename.

```json
{
  "files": [{"fileName": "image1.jpg"}, {"fileName": "image2.png"}],
  "concurrency": 3
}
```

```json
{
  "files": [
    {"fileName": "image1.jpg", "content": "YmFzZTY0IGV4YW1wbGU="},
    {"fileName": "image2.png", "content": "", "error": "file or share not found"}
  ]
}
```

Batch results retain input order, including duplicates. Each file can succeed or fail independently; a valid batch returns HTTP 200 with per-file errors, including when every file fails. Missing files, permission errors, timeouts and transport failures do not terminate the process. Raw backend error text is not returned to clients.

Single-file failures return JSON `{"error":"..."}` with HTTP 404 (missing file), 403 (SMB access denied), 504 (timeout), 503 (cancellation), 413 (file size limit), 400 (invalid file/share name), or 502 (other SMB failure). Invalid batch requests return 400. POST accepts `Content-Type: application/json` (optional charset); other media types return 415, bodies over 4 MiB return 413, and extra JSON values are rejected. Unsupported methods return 405 with an `Allow` header. GET routes also accept HEAD, computing the same response headers without a response body. The backend may classify a missing share as a generic SMB failure.

Batch worker concurrency defaults to 3 and is capped at 10 per request. The shared client additionally caps all active SMB reads at 8 by default, across requests, and rejects files larger than 32 MiB. Files remain fully buffered; batch JSON adds Base64 overhead and completed results remain in memory. There is no batch-count, aggregate-response-size or waiting-request-count limit: these controls do **not** establish a whole-process memory bound. Multiple clients or replicas each have their own limits. HTTP input/read/idle timeouts are enabled; the SMB request budget starts when the handler runs. The Fiber v2 gateway does not propagate HTTP client disconnects as request cancellation. The embeddable standard handler does propagate them; streaming remains separate work.

## Lifecycle and verification

Each read owns its TCP connection, SMB session, share and file. Cleanup runs in file → share → session → connection order, including on errors; the concurrency slot is released after cleanup. Both SMB contexts and a TCP deadline bound work; cancellation closes the connection. Startup validates configuration without requiring the SMB server to be online. SIGINT/SIGTERM stops new connections and drains active requests; expiry cancels outstanding SMB work and exits unsuccessfully.

```sh
go test -race ./...
go vet ./...
go mod verify
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

Tests exercise production routes with an injected reader, configuration precedence, error redaction, timeouts, batch ordering and worker panic containment. External-package tests verify standard HTTP mounting, context values/deadlines, disconnect cancellation and response contracts. Local TCP tests exercise a stalled SMB negotiation, transport cleanup and HTTP shutdown. The separate integration suite starts an isolated Samba server with mandatory signing, a read-only share and an encryption-required share. It verifies reads, directory enumeration, permission/credential failures, and transfer interruption/recovery through a loopback fault proxy, including an end-to-end standard HTTP disconnect during a real SMB transfer, shared admission across HTTP requests, exact file-size boundaries and oversize recovery. This does not validate Windows SMB or AD/Kerberos.

On a disposable Linux development/CI host with Samba tools and root or passwordless sudo:

```sh
python3 scripts/test-samba.py
```

The harness uses synthetic data and credentials, binds only to loopback on a temporary port, keeps its own Samba configuration/passdb/state, and cleans up its process group and temporary files. It does not mount a filesystem or need enterprise secrets. Default `go test` does not require Samba; CI runs the integration harness as a mandatory separate job.

See [the roadmap](docs/roadmap.md) for the remaining v0.1 acceptance gates.

## License

[MIT](LICENSE).
