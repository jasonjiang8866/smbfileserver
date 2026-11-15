# Service authentication and share access

The executable requires `ACCESS_CONFIG_FILE` before it starts listening. A missing,
invalid, empty or oversized policy stops startup. There is no automatic anonymous
fallback. This changes existing gateway deployments: migrate callers and provide
a policy before upgrading. Direct `client` and bare `smbhttp` APIs keep their
existing contracts; embedding applications remain responsible for choosing policy.

This release maps a logical ID to a **whole dedicated SMB share on the configured
SMB_HOST**. It does not provide a confined subdirectory mount or per-path ACLs.
Unknown `root`/`host` fields in the gateway JSON are rejected, not ignored. For
separate data boundaries, publish separate server-side shares with appropriate
ACLs. Do not expose a broad enterprise share under the assumption that lexical
path checks isolate its subdirectories.

## Configure identities and mappings

Copy [the example](../examples/access.json) to a private deployment configuration.
The placeholder digest intentionally fails validation. Generate an independent,
high-entropy token per calling service, using at least 32 random bytes. For example,
run locally (these commands generate your token; there is no built-in token):

```sh
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | sha256sum
# Put only the hex SHA-256 digest into token_sha256.
# Give the raw TOKEN to the calling service through its secret mechanism.
```

```json
{
  "shares": [{"id": "reports", "name": "published-reports"}],
  "services": [{
    "id": "report-worker",
    "token_sha256": ["REPLACE_WITH_64_HEX_CHARACTERS"],
    "shares": ["reports"]
  }]
}
```

```sh
export ACCESS_CONFIG_FILE=/run/secrets/smb-access.json
./smbfileserver
curl -H "Authorization: Bearer $TOKEN" \
  https://gateway.example/api/v1/smb/reports/report.csv
```

SMB username/password still come from the existing secret mechanism and identify
the server-side read-only account. HTTP service tokens identify gateway callers;
the two credential sets have different roles. Token hashes are for high-entropy
machine tokens, not human passwords. Protect the policy from unauthorized writes:
editing it grants access. Keep raw tokens out of browser code, URLs and logs.

Use HTTPS, normally terminated by a trusted ingress/proxy; the gateway does not
implement TLS itself. Restrict direct access to its plaintext listener. Proxies
must preserve Authorization and must not log it. Query parameters, cookies and
forwarded-user headers do not authenticate requests. Exactly one Authorization
header is accepted, with a Bearer token of 32–512 ASCII token characters. All
configured digest comparisons use fixed-length constant-time comparison.

IDs use lowercase letters, digits, `_` and `-` (1–64 characters). Shares and services
are each capped at 128. Every service needs explicit, existing share grants; no
wildcards exist. Unknown and unauthorized logical shares return the same denial.
A raw share name works only if an administrator explicitly assigns that same
string as an authorized logical ID.

## Rotation and revocation

A service may have one or two unique token digests. To rotate:

1. Add the replacement digest alongside the current digest and roll/restart all
   gateway instances using the new configuration.
2. Switch the caller to the replacement token.
3. Remove the old digest and roll/restart all gateway instances again.

The policy is an immutable startup snapshot. There is **no hot reload**; changing
a mounted file alone has no effect. A removed token remains valid on old replicas
until they are replaced. Already-authenticated requests retain their grants until
completion/cancellation, bounded by operation/server timeouts. Immediate global
revocation requires draining/terminating old instances and in-flight requests.

## Shared path checks

The `access.Policy` wraps both Open and ReadFile, so streaming, buffered fallback,
HTTP/1.0, HEAD and every batch item use the same authorization and path checks.
Authentication happens before parsing a batch. A missing identity in a direct
policy call is denied, and a context authenticated by another Policy is rejected.
The original request context/deadline/cancellation is retained.

Paths use relative forward-slash-separated names. The policy rejects, without
normalizing into another name:

- Empty/dot/dot-dot components, leading/trailing/repeated separators, backslashes,
  UNC/drive paths, colons (including NTFS alternate data streams), wildcards and
  control characters.
- Percent characters remaining after URL decoding, including double-encoded
  traversal attempts. JSON filenames are not URL-decoded.
- Leading/trailing component whitespace, trailing dots and Windows device aliases.
- Invalid UTF-8, paths over 4096 bytes or more than 64 components.

Unicode and ordinary internal spaces are preserved. This intentionally excludes
some otherwise legal SMB filenames. Single-file URLs still accept one filename
segment; use batch for nested paths. These rules prevent lexical path aliases and
traversal; they are **not a race-free filesystem sandbox**.

The SMB server and account ACLs must enforce the final filesystem boundary. For
the tested Samba deployment, shares are read-only with `follow symlinks = no` and
`wide links = no`. CI exercises both a file symlink and a directory symlink aimed
outside the share. Client-side resolution of server-returned symlinks, Windows
junctions/reparse points, DFS referrals and concurrently changed directory trees
are not independently confined or validated here. Until those are verified, use
administrator-controlled dedicated shares without those redirections. No
subdirectory-root isolation claim is made.

## HTTP contract and embedding

| Condition | Single-file GET/HEAD | Valid batch |
| --- | --- | --- |
| Missing/invalid token | 401, WWW-Authenticate | 401 before JSON parsing |
| Unknown/unauthorized share | 403 | 200 with denied entries |
| Disallowed filename | 400 (or routing 404) | 200 with invalid-name entries |
| Valid identity and allowed file | Existing download behavior | Existing ordered results |

HEAD omits the body. All protected responses include `Cache-Control: no-store`,
`Content-Disposition: attachment` (no untrusted filename interpolated), and
`Content-Security-Policy: sandbox`; existing MIME/nosniff behavior remains.
Downloads that used to render inline will now download. `/api/v1/hello` remains a
public process-only endpoint; it cannot access SMB and is not a readiness probe.
Both native and Fiber gateway modes use the policy. Fiber remains buffered and
retains its existing disconnect-cancellation limitation.

The optional `access` package, like `smbhttp`, has a standard-library-only
production dependency graph. The client has no HTTP authentication dependencies:

```go
policy, err := access.New(policyConfig, smbClient)
if err != nil { return err }
handler, err := smbhttp.New(policy, smbhttp.Options{})
if err != nil { return err }
mux.Handle("/files/", policy.Wrap(http.StripPrefix("/files", handler)))
```

**Both parts are required:** inject Policy as the reader and wrap the handler with
Policy.Wrap. Wrapping a handler backed by the raw client authenticates callers but
does not enforce share/path grants. Injecting Policy without Wrap denies all reads.
Applications using their own SSO can keep bare smbhttp and supply their own
identity-aware reader; this package does not add OIDC/JWT or trust identity headers.

## Verification and remaining gates

Unit/race tests check no backend calls on rejected identities/shares/paths,
wrapped reader behavior across GET/HEAD/batch, Unicode, independent identities,
configuration snapshotting, rotation, duplicate headers, malformed startup policy,
and native/Fiber adapters. Real Samba CI exercises authenticated HTTP downloads,
raw-share/unauthorized-share rejection, mixed allowed/denied batch paths and
server-enforced symlink denial. Previous streaming, cancellation and overload
cases remain enabled. Check the PR's CI logs for executed results.

Windows/AD/reparse/DFS acceptance, global HTTP admission, audit/metrics, TLS ingress
configuration and restricted OpenShift deployment remain separate gates. This
feature does not make the standalone gateway a fully validated production service.
