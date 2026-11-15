#!/usr/bin/env python3
"""Read-only acceptance of an already deployed gateway; never applies manifests.
Requires oc login, a selected namespace, an immutable image and a known fixture.
Does not read Kubernetes Secrets, print tokens, or disable TLS verification.
"""
import argparse
import hashlib
import json
import pathlib
import re
import secrets
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def validate_cluster(deployment, pods, route, expected_image):
    require(re.fullmatch(r'.+@sha256:[a-fA-F0-9]{64}', expected_image), 'use an immutable expected image digest')
    desired = deployment['spec'].get('replicas', 1)
    status = deployment.get('status', {})
    require(desired > 0 and status.get('observedGeneration', 0) >= deployment['metadata']['generation'],
            'deployment generation not observed')
    require(status.get('updatedReplicas', 0) == desired and status.get('availableReplicas', 0) >= desired,
            'deployment rollout incomplete')
    admitted = []
    for pod in pods['items']:
        if pod['metadata'].get('deletionTimestamp'):
            continue
        require(any(c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', [])),
                'a selected Pod is not ready')
        spec = pod['spec']
        scc = pod['metadata'].get('annotations', {}).get('openshift.io/scc', '')
        require(scc in ('restricted', 'restricted-v2', 'restricted-v3'), 'Pod not admitted by a restricted SCC')
        require(spec.get('automountServiceAccountToken') is False, 'service account token automount enabled')
        require(not spec.get('hostNetwork') and not spec.get('hostPID') and not spec.get('hostIPC'), 'host namespace enabled')
        container = next(c for c in spec['containers'] if c['name'] == 'gateway')
        require(container['image'] == expected_image, 'unexpected gateway image')
        pod_security = spec.get('securityContext', {})
        security = container.get('securityContext', {})
        uid = security.get('runAsUser', pod_security.get('runAsUser'))
        require(isinstance(uid, int) and uid > 0, 'no admitted non-root UID')
        require(security.get('runAsNonRoot', pod_security.get('runAsNonRoot')) is True, 'non-root policy missing')
        require(security.get('readOnlyRootFilesystem') is True and security.get('allowPrivilegeEscalation') is False,
                'writable root or privilege escalation allowed')
        require('ALL' in security.get('capabilities', {}).get('drop', []) and not security.get('capabilities', {}).get('add'),
                'capabilities are not dropped')
        require(security.get('seccompProfile', pod_security.get('seccompProfile', {})).get('type') == 'RuntimeDefault',
                'RuntimeDefault seccomp missing')
        admitted.append({'pod': pod['metadata']['name'], 'scc': scc, 'assigned_uid': uid})
    require(len(admitted) == desired, 'unexpected number of active Pods; wait for rollout completion')
    tls = route['spec'].get('tls', {})
    require(tls.get('termination') == 'edge' and tls.get('insecureEdgeTerminationPolicy') == 'Redirect',
            'expected edge TLS and HTTP redirect Route')
    require(route['spec']['to']['name'] == deployment['metadata']['name'] and route['spec'].get('port', {}).get('targetPort') == 'http',
            'Route does not target the gateway HTTP service')
    host = route['spec'].get('host')
    require(host and any(i.get('host') == host and any(c['type'] == 'Admitted' and c['status'] == 'True'
            for c in i.get('conditions', [])) for i in route.get('status', {}).get('ingress', [])), 'Route not admitted')
    return admitted, host


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--namespace', required=True)
    parser.add_argument('--deployment', default='smbfileserver')
    parser.add_argument('--expected-image', required=True)
    parser.add_argument('--token-file', type=pathlib.Path, required=True)
    parser.add_argument('--share', required=True, help='authorized logical share ID')
    parser.add_argument('--file', required=True, help='single-segment known read-only fixture filename')
    parser.add_argument('--sha256', required=True, help='expected fixture contents hash')
    parser.add_argument('--ca-file', help='optional trusted router CA bundle')
    args = parser.parse_args()
    require(re.fullmatch(r'[a-fA-F0-9]{64}', args.sha256), 'invalid expected SHA256')
    require('/' not in args.file and '\\' not in args.file, 'use a single-segment fixture filename')

    def get(resource, *extra):
        result = subprocess.run(['oc', '-n', args.namespace, 'get', resource, *extra, '-o', 'json'],
                                capture_output=True, text=True, timeout=30)
        require(result.returncode == 0, f'oc get {resource} failed; check connection and namespace RBAC')
        return json.loads(result.stdout)

    deployment = get('deployment', args.deployment)
    # Match the actual deployment selector; never enumerate Secrets.
    labels = deployment['spec']['selector']['matchLabels']
    pods = get('pods', '-l', ','.join(f'{k}={v}' for k, v in labels.items()))
    route = get('route', args.deployment)
    admitted, host = validate_cluster(deployment, pods, route, args.expected_image)
    token = args.token_file.read_text().strip()
    require(token and not any(c.isspace() for c in token), 'invalid token file')
    context = ssl.create_default_context(cafile=args.ca_file)
    http = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context), NoRedirect())
    base = 'https://' + host

    def request(path, auth=False):
        headers = {'Authorization': 'Bearer ' + token} if auth else {}
        try:
            return http.open(urllib.request.Request(base + path, headers=headers), timeout=10)
        except urllib.error.HTTPError as error:
            return error

    for path in ('/livez', '/readyz'):
        with request(path) as response:
            require(response.status == 200, 'local health probe failed through TLS Route')
    with request('/metrics') as response:
        require(response.status == 404, 'metrics exposed on public application Route')
    path = '/api/v1/smb/' + urllib.parse.quote(args.share, safe='') + '/' + urllib.parse.quote(args.file, safe='')
    with request(path) as response:
        require(response.status == 401, 'anonymous file request was not rejected')
    with request('/api/v1/smb/acceptance-denied-' + secrets.token_hex(8) + '/fixture', True) as response:
        require(response.status == 403, 'unknown share was not rejected')
    with request(path, True) as response:
        require(response.status == 200, 'authorized fixture read failed')
        request_id = response.headers.get('X-Request-ID', '')
        require(re.fullmatch(r'[a-f0-9]{32}', request_id), 'missing server request ID')
        digest, total, started = hashlib.sha256(), 0, time.monotonic()
        while chunk := response.read(1 << 20):
            total += len(chunk)
            require(total <= 256 << 20 and time.monotonic() - started < 60, 'fixture exceeded acceptance download budget')
            digest.update(chunk)
        require(digest.hexdigest() == args.sha256.lower(), 'fixture checksum mismatch')
    print(json.dumps({'status': 'passed', 'scope': 'read-only deployment and TLS download smoke',
                      'namespace': args.namespace, 'image': args.expected_image, 'pods': admitted,
                      'download_bytes': total, 'request_id': request_id,
                      'remaining': ['runtime UID inspection', 'Prometheus target scrape', 'SMB outage/recovery',
                                    'token rotation', 'in-flight rolling update', 'concurrent memory budget']}, indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Network exceptions can contain the sensitive URL; output only a class.
        reason = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(json.dumps({'status': 'failed', 'reason': reason}), file=sys.stderr)
        sys.exit(1)
