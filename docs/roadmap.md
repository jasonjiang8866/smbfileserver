# Roadmap: a small, dependable SMB-to-HTTP gateway

Target: read existing enterprise file shares from Kubernetes/OpenShift applications without mounting SMB in application Pods. Files remain on the existing SMB server. Start with read-only service-to-service access; do not require a database or copy the dataset.

## PR 1 — reliability foundation (this change)

- Commit the module manifest/checksums and automate build, real-route tests and dependency checks.
- Separate startup configuration, HTTP handling and SMB I/O behind a narrow reader interface.
- Preserve existing endpoints and legacy environment names; make dotenv optional and fix GET batch routing and MIME responses.
- Replace operational panics with bounded errors; register cleanup at acquisition, apply contexts to session/share operations, and handle signals.
- Exercise route failures, cancellation, configuration precedence and process lifecycle. Do not label fake-reader or stalled-TCP tests as live SMB validation.

## Additional foundation work — CloudSoda migration

The backend is now pinned to CloudSoda instead of the original v1 module. An isolated Samba CI harness advances part of the PR 5 interoperability work: signed/encryption-required read-only shares, real reads, directory listing, and controlled transfer failures. Windows SMB, AD/Kerberos and OpenShift acceptance remain outstanding.

The public `smbhttp` package now supplies the standard `net/http` handler through a backend-independent `FileReader` interface. Its production dependency tree is standard-library-only; Fiber remains in the gateway and adapts the shared handler. Handler construction does not load process configuration or open listeners. Native HTTP disconnect cancellation is covered by unit tests and a real-Samba CI case.

The public `client` package now supplies reusable read-only CloudSoda access: validated configuration, whole-operation timeouts, per-instance active-read admission, per-file byte limits, resource cleanup and backend-independent error categories. The gateway constructs one client for all requests. See [added value and limits](client.md).

Next, move process/configuration wiring into a gateway command and complete the access-boundary work. Consider a public Fiber adapter separately; the current bridge is gateway wiring. Public APIs expose no CloudSoda types.

## PR 2 — access boundaries

- Add rotatable service authentication. Keep long-lived service credentials out of browser code; initially place a business backend or authentication proxy in front.
- Map logical share IDs to configured servers/shares/root directories. Do not let callers select arbitrary backend locations.
- Define and test path handling for slash/backslash, absolute paths, encoding, Unicode, traversal, DFS and reparse points. String filtering alone is not a confinement guarantee.
- Require a dedicated read-only SMB identity and test denied access. Define error codes and safe content-disposition/active-content handling.
- Limit batch counts; document migration from raw share names and legacy routes.

Acceptance: unauthenticated and unauthorized requests are rejected; directory escape cases fail; allowed operations cannot mutate the SMB source.

## PR 3 — bounded resources and transfer behavior

- Implemented: client.Open and default native HTTP streaming, with handle ownership until EOF/Close/cancellation, early headers, late-error abort and fixed-size copy buffers. Fiber is an explicit buffered alternative.
- Per-client active-read admission, queue wait within the operation timeout, and per-file byte checks (including growth after Stat) are implemented. Batch count and retained decoded-byte limits are now implemented. Add queue-count admission; account for retained responses and multiple client instances when claiming a per-Pod memory bound.
- Base64 batch contract now has explicit count/decoded-content limits; consider metadata-plus-download URLs for larger work.
- Native streaming tests exercise disconnects, idle stream deadlines, early Close, upstream errors and slow HTTP readers. Keep Fiber buffered until its streaming ownership/disconnect behavior has separate validation.
- Benchmark connection setup cost before adding a bounded connection pool. Isolate reuse by server and credential identity, and invalidate broken sessions.

Acceptance: memory stays within a declared budget during large transfers and overload; cancellation/failures return resources; overload produces documented responses. Multiply limits by replica count when sizing upstream load.

## PR 4 — Kubernetes/OpenShift delivery

- Multi-stage image, Kubernetes base and OpenShift overlay, Service, optional Route, ConfigMap/Secret examples and resource requests/limits.
- Run as an arbitrary non-root UID with a read-only root filesystem, dropped capabilities and no privileged mounts. Document required outbound DNS/TCP connectivity.
- Separate process liveness from backend readiness; avoid restarting every Pod during an SMB outage. Define readiness semantics for multiple shares.
- Add request IDs, durations, transferred bytes, error classes and active-operation metrics without exposing secrets or sensitive full paths.
- Match termination grace periods to application shutdown budgets; verify rolling updates.

Acceptance: actual restricted OpenShift deployment and evidence, including UID/filesystem constraints and in-flight request behavior. Do not claim this gate passed from local container tests alone.

## PR 5 — interoperability and release

- Isolated Samba integration service in CI, with synthetic fixtures and no enterprise credentials/data.
- Verify authentication failure, denied/missing paths, Unicode/long names, nested paths, large files, concurrency, server outage and recovery.
- Add an opt-in Windows SMB validation procedure; document tested server/dialect combinations and actual signing/encryption behavior.
- Specify consistency when files are modified during reads; recommend producer-side temporary-file plus rename publication where supported.
- Build/scan/publish versioned images tied to a commit and publish a v0.1 release only after the security, resource and deployment gates pass.

## Later, after evidence

Range and conditional requests, measured connection-pool optimizations, OIDC and finer-grained authorization, user identity delegation, and carefully scoped cache invalidation. Upload/delete, a file-manager UI and additional storage protocols are outside v0.1.
