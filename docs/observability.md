# Request correlation and operational signals

The gateway emits JSON request-completion logs to stdout in both native HTTP and
Fiber modes. The reusable `smbhttp` handler and `access` policy still use only the
standard library; no logger, metrics registry or process listener is installed by
importing them. `client.Stats()` is a new dependency-free, credential-free admission
snapshot for applications that supply their own instrumentation.

## Request IDs and logs

Every request reaching gateway middleware receives a fresh 128-bit random hex
`X-Request-ID`, including authentication failures and unknown routes. An incoming
ID is deliberately replaced: clients can otherwise inject secrets or duplicate
IDs into logs. Callers should retain the **returned** ID for support. This is local
request correlation, not distributed tracing or trusted upstream trace propagation.
Protocol errors rejected before middleware may not receive an ID.

A completion log contains:

```json
{"time":"2026-11-16T00:00:00Z","level":"INFO","msg":"http_request","request_id":"5abc390f403a4510bff01f94c65518e2a","route":"file","method":"GET","status":200,"duration_seconds":0.25,"response_bytes":4096,"outcome":"complete"}
```

The example is illustrative, not a measured result. Routes are fixed categories:
`file`, `batch`, `hello`, `livez`, `readyz`, `other`. Methods use a fixed HTTP verb
allowlist, with unknown methods collapsed to `OTHER`. No raw URL, query, filename,
share ID, account, bearer token, request body, peer address or raw backend error
is included. Healthy probe requests count in metrics but omit completion logs.
Request logging is synchronous; provision stdout/log collection throughput and
retention for expected traffic. It is not a complete per-file audit trail.

`outcome` distinguishes `complete`, `client_error`, `server_error`, `canceled`,
`aborted` and unexpected native `panic`. A stream that fails **after** headers can
have `status=200,outcome=aborted`: the wrapper preserves connection aborts and does
not append JSON to the file or relabel the wire status. Native panic details are
redacted. Ordinary failures are classified by HTTP status, not guessed SMB causes.
A batch HTTP 200 describes the envelope: individual files can still fail, so this
metric is not a per-file success counter. Fiber recovery appears as a 500.

Native response bytes count bytes accepted by `ResponseWriter.Write`, not remote
acknowledgements. Native duration covers handler execution through streaming and
cleanup. Fiber bytes measure the buffered body (HEAD is zero); Fiber duration ends
when buffering completes, before socket delivery. Fiber cannot reliably observe
client disconnects. Neither measurement proves successful receipt by the client.

## Metrics listener

`METRICS_ADDR` defaults to empty (disabled). Set `127.0.0.1:9090` for local scraping,
or `:9090` in a Pod with restricted monitoring ingress. The listener is separate
from application HTTP: `GET /metrics` on port 3000 remains 404, and the supplied
OpenShift Route only targets the application port. Invalid addresses or bind
conflicts fail startup. The listener closes when the gateway exits.

Metrics are Prometheus text format 0.0.4, GET-only, no-store. There is no built-in
TLS/authentication on this internal listener; restrict access through network
policy and namespace RBAC. Never create a public Route for it. Counters reset on
restart and belong to one process. Scrapes themselves are excluded from request
instrumentation. No request IDs or user-controlled paths become metric labels.

| Metric | Type and meaning |
| --- | --- |
| `smb_gateway_http_requests_total{route,method,status,outcome}` | Counter of completed handlers, including denied requests and late aborts |
| `smb_gateway_http_request_duration_seconds{route}` | Histogram; bounds 0.005, 0.025, 0.1, 0.5, 1, 5, 15, 60 seconds and +Inf |
| `smb_gateway_http_response_bytes_total{route}` | Counter of accepted/buffered body bytes |
| `smb_gateway_http_in_flight` | Gauge of currently executing handlers |
| `smb_gateway_smb_active_reads` | Gauge of occupied client admission slots, including cleanup |
| `smb_gateway_smb_queued_reads` | Gauge of calls waiting for admission, before connecting to SMB |
| `smb_gateway_go_goroutines` | Current process goroutines |
| `smb_gateway_go_heap_alloc_bytes` | Allocated Go heap, **not** RSS or Pod memory |

`client.Stats()` fields are sampled independently; they may change immediately
and are not a transactionally consistent pair. Active slots are not a session-pool
size. Request series appear after first use; client and runtime gauges exist on
first scrape. Use cluster container metrics for RSS/CPU and actual memory limits.

Example queries (adjust selectors to your ServiceMonitor labels):

```promql
sum(rate(smb_gateway_http_requests_total{outcome=~"server_error|aborted|panic"}[5m]))
histogram_quantile(0.95, sum by (le) (rate(smb_gateway_http_request_duration_seconds_bucket{route="file"}[5m])))
sum(smb_gateway_smb_queued_reads)
```

## OpenShift monitoring

Use `deploy/openshift-monitoring` instead of `deploy/openshift` in your private site
Kustomization. It adds the metrics listener/Service, a ServiceMonitor and an ingress
NetworkPolicy. It requires Prometheus Operator CRDs and administrator-enabled
OpenShift user workload monitoring. The policy permits metrics ingress only from
`openshift-user-workload-monitoring`; adapt this if your monitoring namespace differs.
Its application-port rule preserves existing ingress on TCP 3000. Tighten that rule
for approved callers/router/probes before production. NetworkPolicies are additive:
another broad allow policy can reopen metrics access. Review effective policies.
The overlay does not grant cluster roles or enable monitoring cluster-wide.

The monitoring overlay is render-tested, not verified against a live Prometheus
Operator or OpenShift cluster. A missing CRD should fail server-side dry-run rather
than silently installing unusable monitoring. Confirm the target is actually `UP`
and a test request changes the expected counter before calling scraping verified.

## Read-only cluster acceptance

After deploying an approved digest in a **test namespace**, run from a machine
with `oc login`, namespace read access and reachability to the TLS Route:

```sh
python3 scripts/accept-openshift.py \
  --namespace YOUR_TEST_NAMESPACE \
  --expected-image YOUR_REGISTRY/smbfileserver@sha256:YOUR_IMAGE_DIGEST \
  --token-file /secure/service-token \
  --share reports --file fixture.txt --sha256 EXPECTED_FIXTURE_SHA256 \
  > acceptance.json
```

Supply `--ca-file /secure/router-ca.pem` if the router uses a private CA. The token
file contains the raw service token, not its hash; do not commit it or pass it on
the command line. Use a known read-only synthetic fixture (at most 256 MiB, within
the configured gateway file/request limits). The script follows no redirects with
credentials, prints no fixture path/token, changes no cluster resources and reads
no Kubernetes Secrets. Errors redact network exception text that can contain URLs.

The JSON evidence records the expected image, admitted SCC and assigned non-root
UIDs, TLS download hash success and returned request ID. Its `remaining` field
explicitly lists gates not covered by this smoke: actual process UID inspection,
Prometheus scraping, SMB outage/recovery, token rotation, in-flight rolling updates
and concurrent memory budgets. Pod-spec UID evidence alone is not runtime inspection.
Offline script tests validate rejection of weakened sample snapshots; they are not
cluster evidence. No live cluster acceptance result has been recorded yet.

References: [Prometheus exposition](https://prometheus.io/docs/instrumenting/exposition_formats/),
[Go ResponseController](https://pkg.go.dev/net/http#ResponseController),
[OpenShift user workload metrics](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.19/html/configuring_user_workload_monitoring/configuring-metrics-uwm).
