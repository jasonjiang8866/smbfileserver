# Container, Kubernetes and OpenShift deployment

These are deployment foundations, not a production certification. CI builds the
actual image and reads an isolated Samba fixture with a random non-root UID,
read-only root filesystem and dropped capabilities. It also checks probes,
authentication, share denial, server-side link denial, SIGTERM and cleanup.
Kustomize rendering is checked. Actual Kubernetes admission, restricted OpenShift
SCC assignment, Route behavior and rolling updates require a cluster acceptance
run; those have not been verified by container tests.

## Build an image

```sh
docker build -t smbfileserver:local .
```

The final scratch image contains only a statically linked binary. It runs as
`65532:0` by default and supports a cluster-assigned non-root UID. It needs no
writable home, `/tmp`, persistent volume, SMB mount, Linux capabilities or service
account token. The builder uses Go 1.27.4; review/pin the builder digest in your
own supply chain. This repository does not publish a prebuilt release image yet.
Build, scan, tag and push to your own approved registry, then record its digest.

## Prepare configuration and credentials

Use a dedicated server-side read-only SMB account and dedicated published shares.
Configure the server's symlink/reparse policy as described in
[access control](access-control.md); lexical path validation is not filesystem
confinement. Generate a random service token and its SHA-256 digest using that
guide, and prepare a private `access.json` from [the example](../examples/access.json).
Only hashes belong in this file. Distribute the raw token separately to callers.

In a private file `/secure/smb.env`, set `SMB_USERNAME`, `SMB_PASSWORD` and optional
`SMB_DOMAIN`, one `KEY=value` per line. Do not put this file in the repository or
Docker build context. Create both Secrets in your chosen namespace:

```sh
kubectl -n YOUR_NAMESPACE create secret generic smbfileserver-smb \
  --from-env-file=/secure/smb.env
kubectl -n YOUR_NAMESPACE create secret generic smbfileserver-access \
  --from-file=access.json=/secure/access.json
```

On OpenShift use `oc` in place of `kubectl`. The policy Secret is mounted read-only
with mode `0444` so arbitrary UIDs can read its hashes without a fixed `fsGroup`.
The SMB password is passed through the Secret environment, never the ConfigMap.
Restrict Secret read/exec permissions with namespace RBAC. Configuration is loaded
at startup: changing either Secret requires a rollout. Token rotation uses an
overlap period as documented in the access guide.

## Render and deploy

The base provides a two-replica Deployment, ClusterIP Service and ConfigMap.
The OpenShift overlay adds an edge-TLS Route with HTTP-to-HTTPS redirect. Neither
creates Secrets, a namespace, privileged SCC grants or public registry resources.
The image name and SMB hostname are deliberate placeholders.

Create `deploy/site/kustomization.yaml` locally (keep private site configuration
out of the public repository):

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: YOUR_NAMESPACE
resources:
  - ../openshift # Use ../base for Kubernetes without an OpenShift Route.
images:
  - name: smbfileserver
    newName: YOUR_REGISTRY/YOUR_PROJECT/smbfileserver
    digest: sha256:YOUR_IMAGE_DIGEST
patches:
  - target:
      kind: ConfigMap
      name: smbfileserver
    patch: |-
      - op: replace
        path: /data/SMB_HOST
        value: YOUR_SMB_HOST
```

```sh
kubectl kustomize deploy/site > /tmp/smbfileserver-rendered.yaml
kubectl apply --dry-run=server -f /tmp/smbfileserver-rendered.yaml
kubectl apply -f /tmp/smbfileserver-rendered.yaml
kubectl -n YOUR_NAMESPACE rollout status deployment/smbfileserver
```

Configure image pull credentials separately if your registry is private. The
manifests omit `runAsUser` and `fsGroup` so OpenShift can assign namespace-specific
IDs. They request `runAsNonRoot`, `RuntimeDefault` seccomp, no privilege escalation,
all capabilities dropped and a read-only root filesystem. Do not grant `anyuid`
or privileged SCC just to run this gateway. Kubernetes without UID injection uses
the image's numeric non-root user.

The Route terminates TLS at the router; router-to-Pod traffic is HTTP. Restrict
that network to trusted workloads/router and kubelet probes, or deploy a separately
validated re-encryption/mTLS proxy if your environment requires transport encryption
inside the cluster. Do not send service tokens over an exposed plain-HTTP Service.
Cluster-internal callers need their own trusted TLS ingress/proxy if the Pod network
is not an acceptable trust boundary.

Required connectivity: inbound TCP 3000 from approved callers/router and probes;
outbound DNS and TCP to the configured SMB host/port (normally 445). Add NetworkPolicy
rules for your cluster's DNS, router and SMB endpoint. No generic allow-all policy
is supplied. The service account has no API permissions and its token is not mounted.

## Health and shutdown semantics

| Endpoint | Meaning | Authentication |
| --- | --- | --- |
| `/livez` | Process can serve HTTP | Public GET/HEAD, no-store |
| `/readyz` | Valid startup configuration, accepting work, not draining | Public GET/HEAD, no-store |
| `/api/v1/hello` | Legacy process-only hello | Public |

Neither probe opens an SMB connection, checks a share, validates the remote SMB
password nor promises that a download will succeed. Invalid **local** configuration
and access policy prevent startup. An unreachable server or inaccessible share does
not remove every replica or trigger restarts. Monitor authenticated synthetic reads
and download errors separately for backend availability, including each required
share. Exhausted read admission uses the existing 503 contract; it does not flip
readiness globally.

SIGTERM marks readiness false before closing the listener and draining in-flight
requests. It can then become unreachable immediately as the listener closes; there
is no guaranteed interval during which a probe will observe 503. Kubernetes also
removes terminating Pods from ready endpoints. The application waits up to 15s;
the Pod termination budget is 30s. Requests exceeding the drain budget are aborted.
The default 30s request budget therefore does not guarantee completion during a
rollout. Increase both shutdown and Pod grace together if that guarantee is needed,
leaving margin for signal delivery/endpoint propagation. Clients must handle
interrupted streaming bodies and retry according to their data consistency needs.

## Capacity and acceptance

The example requests 100m CPU/128Mi memory and limits 1 CPU/512Mi. These are starting
values, not a measured whole-Pod memory guarantee: concurrent Base64 batches,
HTTP/1.0 buffered downloads and retained HTTP responses still require workload
measurement and admission controls. Two replicas allow up to 16 active SMB reads;
the one surge replica can temporarily raise this to 24. Include other clients when
sizing the SMB server. See [resource limits](admission.md).

Before production, record the image digest, cluster/version, assigned UID/SCC,
server/dialect and these results:

- Admission succeeds with the restricted security settings; no writable root or
  privileged mounts are needed. Probes stay healthy during a controlled SMB outage.
- Through the real TLS Route, unauthenticated requests return 401, disallowed shares
  return 403, and authorized Unicode/large downloads match their checksums.
- Token overlap and rolling restart work across replicas. New requests move to
  ready replicas; in-flight streams finish within grace or fail detectably.
- DNS/SMB firewall rules, certificate trust, actual Windows reparse/DFS behavior
  and memory under expected concurrency pass your environment's checks.

Request IDs, structured operation metrics, Windows/AD validation, published/scanned
release images and real OpenShift acceptance remain follow-up work.

References: [Kubernetes probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/),
[security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/),
[OpenShift image guidance](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/images/creating-images).
