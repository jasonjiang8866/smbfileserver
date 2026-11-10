# CloudSoda SMB backend migration

Historical PR #2 migration record. The public handler and client have since been extracted; see [client behavior and current resource limits](client.md) and [HTTP embedding](http-handler.md).

## Selected dependency and API changes

- Backend: `github.com/cloudsoda/go-smb2 v0.0.0-20261105190815-95be04a983b8` (commit `95be04a983b8770c7255d547aa02ee384e9882c7`). Pin this pseudo-version and its checksums; do not float to `main`.
- The original `github.com/hirochachacha/go-smb2 v1.1.0` is removed from both the module graph and checksums. This is a real module migration, not a `replace` directive.
- Keep the existing HTTP routes, configuration names and `FileReader` boundary. Public package extraction is a separate next step.
- Use CloudSoda's `DialConn(ctx, conn, hostname)` so the application retains transport deadline/cancellation control. Supply the bare configured hostname for SMB share addressing; the TCP connection separately uses host plus port.
- Session/share operations still require explicit `WithContext`. CloudSoda returns context errors directly, so the old `ContextError` adapter is removed. Its `TransportError` still needs unwrapping for timeout classification.
- CloudSoda's `Logoff` also closes its transport; the caller's deferred `conn.Close` remains an idempotent fallback covering negotiation/authentication failures and cancellations.
- Existing Go 1.26 minimum and Go 1.26/1.27 test matrix are retained. No server feature or automatic Kerberos integration is implied by switching the dependency.

## Verification

Local Linux amd64 / Go 1.27.3:

- Existing route, configuration and lifecycle tests pass under the race detector with CloudSoda.
- Integration-tagged tests compile without requiring a local SMB server.
- `go vet -tags=integration ./...`, module verification and application build are required before submitting the update.
- `govulncheck v1.9.0 -show verbose ./...`: zero reachable vulnerable symbols and zero other findings in imported packages. The original directory-listing advisory GO-2026-5051 no longer appears with this pinned, fixed CloudSoda version. One module-only advisory remains for unused `golang.org/x/crypto/openpgp` (GO-2026-5932); this application does not import OpenPGP.

GitHub CI adds a mandatory `samba` job. Its current result is recorded in PR #2 checks; do not equate successful local compilation with an executed integration suite. `scripts/test-samba.py` starts an isolated Samba instance using synthetic fixtures and validates:

1. NTLM authentication and exact reads of empty, regular, Unicode/nested, long-name and 4 MiB binary files.
2. Reads through the production HTTP single-file and concurrent batch handlers, including duplicate entries and a missing file.
3. Server-required signing and an encryption-required share; directory enumeration using the pinned backend and denial of writes to a read-only share.
4. Missing file/share, filesystem access denial, and invalid credentials without exposing backend error details over HTTP.
5. Parent cancellation, deadlines and dropped connections during an established SMB file transfer, followed by a successful new read using the same reader instance. A loopback proxy pauses the transfer after 64 KiB to make the interruption deterministic.

The harness stops only its own process group, removes its private passdb/state/data and does not use the host's default Samba configuration. No production network or credentials are involved.

## Limits

This fixture exercises Samba over TCP with NTLM and controlled network interruption. It is not Windows SMB/AD/Kerberos validation, a full Samba restart test, a general malformed-packet audit, a large-file memory benchmark or an OpenShift deployment test. File buffering, missing HTTP authentication/path confinement and per-request (rather than global) concurrency limits remain on the roadmap. Directory listing is exercised at the backend test layer; no new listing HTTP endpoint is exposed.
