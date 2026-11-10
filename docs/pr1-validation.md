# Initial reliability foundation validation

Historical results for the first PR #2 commit (`8cfe119`). The subsequent CloudSoda migration supersedes the dependency findings below; see [current migration notes](cloudsoda-migration.md) and the PR checks for current evidence.

Baseline: `3684852beddaeb737a7fab8320e4da6db0f1a58e` (main).

## Local evidence

Environment: Linux amd64, Go 1.27.3.

- `go test -race ./...`: passed. Tests use the production Fiber router and injected file readers; local TCP/HTTP tests cover stalled SMB negotiation, cancellation/connection closure, immediate shutdown, in-flight draining and grace-period expiry.
- `go vet ./...`: passed.
- `go mod verify`: all modules verified.
- `go build -trimpath -o /tmp/smbfileserver-pr1 .`: passed.
- Binary smoke in a fresh directory without `.env`: invalid configuration exits 1; environment-only configuration starts; a refused backend connection returns sanitized HTTP 502; hello still succeeds; SIGTERM exits 0.
- `git diff --check`: passed. CI YAML parsed successfully.
- CI is configured for Go 1.26.x and 1.27.x; its remote result must be checked on the PR independently of this local evidence.

## Dependency scan scope

`govulncheck v1.9.0` on Go 1.27.3 reports **zero reachable vulnerable symbols**, not zero advisories in the dependency graph. HTTP transport and crypto transitive dependencies were updated and pinned.

Two non-reachable findings remain in this scan:

- [GO-2026-5051](https://pkg.go.dev/vuln/GO-2026-5051): the retained `github.com/hirochachacha/go-smb2 v1.1.0` has a directory-listing parser issue. This gateway calls `Share.ReadFile`, not `Share.ReadDir`; the scan reports no reachable vulnerable symbol. The advisory lists no fixed v1 version. Evaluate a maintained/fixed SMB backend before adding directory enumeration, and re-run the scan whenever the call graph changes. This does not establish safety against all malicious-server inputs.
- [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932): the `golang.org/x/crypto v0.58.0` module includes deprecated OpenPGP code. This application does not import OpenPGP. Do not introduce that package; keep tracking upstream updates.

The SMB module's major-version/API migration is intentionally separate from this reliability change so protocol behavior can be validated with a real server before switching.

## Not yet verified

No live Samba or Windows SMB authentication/read fixture, real OpenShift cluster, large-file memory limit, client-disconnect streaming, share confinement or HTTP authorization was tested. These remain explicit roadmap gates. This PR is not approval to expose the current unauthenticated API or declare a production-ready v0.1.
