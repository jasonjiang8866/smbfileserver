#!/usr/bin/env python3
"""Run integration tests against an isolated, loopback-only Samba fixture.
Requires smbd, smbpasswd, testparm and root or passwordless sudo (CI runner).
Never uses the host's default Samba config/passdb or enterprise credentials.
"""
import os
import pathlib
import pwd
import secrets
import shutil
import socket
import subprocess
import tempfile
import time


def main():
    for tool in ('smbd', 'smbpasswd', 'testparm', 'go'):
        if not shutil.which(tool):
            raise SystemExit(f'{tool} is required; install Samba and Go first')
    root_command = [] if os.geteuid() == 0 else ['sudo', '-n']
    subprocess.run(root_command + ['true'], check=True)
    username = pwd.getpwuid(os.getuid()).pw_name if os.geteuid() else 'nobody'
    password = secrets.token_urlsafe(24)
    root = pathlib.Path(tempfile.mkdtemp(prefix='smbfileserver-samba-'))
    root.chmod(0o755)
    process = None
    log = None
    try:
        for name in ('share', 'private', 'state', 'lock', 'cache', 'pid', 'logs', 'ncalrpc'):
            (root / name).mkdir(mode=0o700 if name == 'private' else 0o755)
            if name != 'share':
                subprocess.run(root_command + ['chown', 'root:root', str(root / name)], check=True)
        share = root / 'share'
        (share / 'hello.txt').write_bytes(b'SMB integration fixture\n')
        (share / 'empty.txt').write_bytes(b'')
        (root / 'outside-secret.txt').write_bytes(b'outside share must never be returned\n')
        (share / 'escape-link.txt').symlink_to(root / 'outside-secret.txt')
        (share / 'escape-dir').symlink_to(root, target_is_directory=True)
        (share / 'nested').mkdir()
        (share / 'nested' / '工艺 报表.csv').write_bytes('lot,value\n晶圆,42\n'.encode())
        (share / ('a' * 200 + '.txt')).write_bytes(b'long name\n')
        (share / 'large.bin').write_bytes(bytes(range(256)) * 16384)  # 4 MiB
        with (share / 'stream.bin').open('wb') as stream:
            block = bytes(range(256)) * 4096
            for _ in range(64):
                stream.write(block)  # 64 MiB without a whole-file fixture allocation.
        (share / 'denied.txt').write_bytes(b'not accessible\n')
        (share / 'denied.txt').chmod(0)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        config = root / 'smb.conf'
        config.write_text(f'''[global]
    workgroup = WORKGROUP
    netbios name = SMBFIXTURE
    server role = standalone server
    security = user
    map to guest = Never
    interfaces = 127.0.0.1
    bind interfaces only = yes
    smb ports = {port}
    disable netbios = yes
    server min protocol = SMB2_02
    server max protocol = SMB3_11
    server signing = mandatory
    load printers = no
    disable spoolss = yes
    printing = bsd
    printcap name = /dev/null
    passdb backend = tdbsam:{root}/private/passdb.tdb
    private dir = {root}/private
    state directory = {root}/state
    lock directory = {root}/lock
    cache directory = {root}/cache
    pid directory = {root}/pid
    ncalrpc dir = {root}/ncalrpc
    log file = {root}/logs/log.%m
    log level = 0
[readonly]
    path = {share}
    valid users = {username}
    read only = yes
    follow symlinks = no
    wide links = no
[encrypted]
    path = {share}
    valid users = {username}
    read only = yes
    smb encrypt = required
    follow symlinks = no
    wide links = no
''')
        subprocess.run(root_command + ['testparm', '-s', str(config)], check=True, stdout=subprocess.DEVNULL)
        subprocess.run(root_command + ['smbpasswd', '-s', '-a', '-c', str(config), username],
                       input=password + '\n' + password + '\n', text=True,
                       check=True, stdout=subprocess.DEVNULL)
        subprocess.run(['smbd', '--version'], check=True)
        log = (root / 'smbd.log').open('w')
        # Start a dedicated process group; stop only this fixture and its children.
        # Foreground smbd exits on stdin EOF. Keep a pipe open throughout the
        # tests, since CI's inherited stdin is already at EOF.
        process = subprocess.Popen(root_command + ['smbd', '-F', '--no-process-group', '--debug-stdout', '-d', '1', '-s', str(config)],
                                   stdin=subprocess.PIPE, stdout=log, stderr=log, start_new_session=True)
        for attempt in range(100):
            if process.poll() is not None:
                raise RuntimeError(f'isolated smbd exited before readiness (status {process.returncode})')
            try:
                with socket.create_connection(('127.0.0.1', port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError('isolated smbd did not listen')
        env = dict(os.environ, SMB_INTEGRATION='1', SMB_TEST_HOST='127.0.0.1',
                   SMB_TEST_PORT=str(port), SMB_TEST_USER=username, SMB_TEST_PASSWORD=password)
        subprocess.run(['go', 'test', '-race', '-tags=integration', '-count=1', '-timeout=90s',
                        '-v', '-run=TestSMBIntegration', './...'], env=env, check=True)
    except BaseException:
        # Only synthetic fixture logs at log level zero; never print its password.
        if log:
            log.flush()
            print((root / 'smbd.log').read_text()[-6000:])
        raise
    finally:
        if process:
            subprocess.run(root_command + ['kill', '-TERM', '--', f'-{process.pid}'],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                subprocess.run(root_command + ['kill', '-KILL', '--', f'-{process.pid}'], check=False)
                process.wait(timeout=5)
            process.stdin.close()
        if log:
            log.close()
        # Samba's private files may be owned by root; root is a fresh tempfile path.
        subprocess.run(root_command + ['rm', '-rf', '--', str(root)], check=True)
        if process:
            for attempt in range(50):
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=0.1):
                        time.sleep(0.1)
                except OSError:
                    break
            else:
                raise RuntimeError('fixture port is still accepting connections after cleanup')
        print('Isolated Samba fixture stopped and temporary files removed.')


if __name__ == '__main__':
    main()
