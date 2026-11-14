#!/usr/bin/env python3
"""Exercise the built image against test-samba.py's synthetic Linux fixture."""
import hashlib
import json
import os
import pathlib
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()


def main():
    image = sys.argv[1]
    name = 'smbfileserver-smoke-' + secrets.token_hex(6)
    token = secrets.token_urlsafe(32)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix='smb-container-') as directory:
        root = pathlib.Path(directory)
        root.chmod(0o755)
        policy = root / 'access.json'
        policy.write_text(json.dumps({
            'shares': [{'id': 'reports', 'name': 'readonly'}],
            'services': [{'id': 'smoke', 'token_sha256': [hashlib.sha256(token.encode()).hexdigest()],
                          'shares': ['reports']}]}))
        policy.chmod(0o444)
        environment = {
            'HTTP_ADDR': f'127.0.0.1:{port}', 'HTTP_SERVER': 'nethttp',
            'ACCESS_CONFIG_FILE': '/access.json', 'REQUEST_TIMEOUT': '5s', 'SHUTDOWN_TIMEOUT': '2s',
            'SMB_HOST': os.environ['SMB_TEST_HOST'], 'SMB_PORT': os.environ['SMB_TEST_PORT'],
            'SMB_USERNAME': os.environ['SMB_TEST_USER'], 'SMB_PASSWORD': os.environ['SMB_TEST_PASSWORD'],
        }
        envfile = root / 'environment'
        envfile.touch(mode=0o600)
        envfile.write_text(''.join(f'{key}={value}\n' for key, value in environment.items()))
        # Bypass host proxy settings for this loopback-only fixture.
        http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, authorized=False, method='GET'):
            headers = {'Authorization': 'Bearer ' + token} if authorized else {}
            req = urllib.request.Request(f'http://127.0.0.1:{port}' + path, headers=headers, method=method)
            try:
                response = http.open(req, timeout=3)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, response.headers, response.read()

        try:
            # Host networking is solely for the isolated loopback Samba fixture.
            # Deployed Pods use the normal cluster network, never hostNetwork.
            docker('run', '-d', '--name', name, '--network', 'host', '--user', '1001234567:0',
                   '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                   '--memory', '512m', '--cpus', '1', '--pids-limit', '128',
                   '--mount', f'type=bind,source={policy},target=/access.json,readonly',
                   '--env-file', str(envfile), image)
            for _ in range(150):
                if docker('inspect', '--format', '{{.State.Running}}', name) != 'true':
                    raise AssertionError('gateway exited before readiness')
                try:
                    if request('/readyz')[0] == 200:
                        break
                except (OSError, urllib.error.URLError):
                    pass
                time.sleep(0.1)
            else:
                raise AssertionError('gateway never became ready')
            assert request('/livez')[0] == 200
            assert request('/readyz', method='HEAD')[2] == b''
            assert request('/api/v1/smb/reports/hello.txt')[0] == 401
            status, headers, body = request('/api/v1/smb/reports/hello.txt', True)
            assert status == 200 and body == b'SMB integration fixture\n', (status, body)
            assert headers.get_content_disposition() == 'attachment'
            assert headers['Cache-Control'] == 'no-store'
            assert request('/api/v1/smb/readonly/hello.txt', True)[0] == 403
            assert request('/api/v1/smb/reports/escape-link.txt', True)[0] != 200
            # Inspect only non-secret runtime fields; never dump container env.
            assert docker('inspect', '--format', '{{.HostConfig.ReadonlyRootfs}}', name) == 'true'
            assert json.loads(docker('inspect', '--format', '{{json .HostConfig.CapDrop}}', name)) == ['ALL']
            assert 'no-new-privileges' in docker('inspect', '--format', '{{json .HostConfig.SecurityOpt}}', name)
            uids = docker('top', name, '-eo', 'uid').splitlines()[1:]
            assert uids and all(uid.strip() == '1001234567' for uid in uids), uids
            print('Container: arbitrary UID 1001234567, read-only root, dropped capabilities verified.', flush=True)
            print('Container: probes, authenticated Samba download, unauthorized share and symlink denial passed.', flush=True)
            docker('stop', '--time', '5', name)
            assert docker('inspect', '--format', '{{.State.ExitCode}}', name) == '0'
            print('Container: SIGTERM exited cleanly with code 0.', flush=True)
        finally:
            subprocess.run(['docker', 'rm', '-f', name], check=True, stdout=subprocess.DEVNULL)
            assert not docker('ps', '-aq', '--filter', f'name=^/{name}$')
            print('Container: fixture removed.', flush=True)


if __name__ == '__main__':
    main()
