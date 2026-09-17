#!/usr/bin/env python3
"""Real daemon/CLI/stdio-bridge contract with a local SSH shim, not a network test."""
import argparse
import base64
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
sys.dont_write_bytecode = True
from check_terminal import View, wait_until


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    binary = parser.parse_args().binary.resolve()
    with tempfile.TemporaryDirectory(prefix='continuum-remote-') as directory:
        root = Path(directory).resolve()
        home, remote_home = root / 'client-home', root / 'host-home'
        home.mkdir()
        remote_home.mkdir()
        state = root / "remote state ' $(false)"
        local_state = root / 'local-state'
        browse = root / "workspace ' & $ test"
        browse.mkdir()
        (browse / 'alpha').mkdir()
        (browse / 'beta').write_text('private file contents')
        (browse / 'gamma').symlink_to('alpha')
        (browse / 'outside').symlink_to(root)
        remote_binary = root / "remote binary ' $(false)"
        shutil.copy2(binary, remote_binary)
        shim_dir = root / 'shim'
        shim_dir.mkdir()
        # Run the actual remote bridge via the same quoted shell command SSH
        # receives. Strictly assert every SSH option and preserve stdin bytes.
        shim = shim_dir / 'ssh'
        shim.write_text(f'''#!{sys.executable}
import os, subprocess, sys
args = sys.argv[1:]
assert args[:9] == ['-T', '-oBatchMode=yes', '-oStrictHostKeyChecking=yes', '-oConnectTimeout=8', '-oClearAllForwardings=yes', '-oForwardAgent=no', '-oForwardX11=no', '--', 'fixture-host'], args
assert len(args) == 10, args
env = dict(os.environ, HOME=os.environ['TEST_REMOTE_HOME'])
mode = os.environ.get('TEST_SSH_MODE', '')
if mode == 'invalid':
    print('{{}} garbage'); sys.exit(0)
if mode == 'oversized':
    print('x' * (2 << 20)); sys.exit(0)
result = subprocess.run(['/bin/sh', '-c', args[9]], input=sys.stdin.buffer.read(), capture_output=True, env=env)
if mode == 'drop':
    sys.exit(255)
sys.stdout.buffer.write(result.stdout)
sys.stderr.buffer.write(result.stderr)
sys.exit(result.returncode)
''')
        shim.chmod(0o700)
        env = dict(os.environ, HOME=str(home), XDG_CONFIG_HOME=str(home / 'config'),
                   TMPDIR=str(root), TEST_REMOTE_HOME=str(remote_home),
                   PATH=str(shim_dir) + os.pathsep + os.environ['PATH'], TERM='xterm-256color')
        daemons = []
        views = []
        log = open(root / 'daemon.log', 'wb')

        def cli(command, *args, code=0, data=None, remote=True, extra_env=None, selected=None):
            selected = selected or state
            target = ['--host', 'fixture-host', '--remote-binary', str(remote_binary)] if remote else []
            result = subprocess.run([str(binary), command, '--state', str(selected), *target, *args],
                                    env=dict(env, **(extra_env or {})), input=data, capture_output=True, timeout=20)
            assert result.returncode == code, (command, result.returncode, result.stdout, result.stderr)
            return result

        def rpc(command, *args, **kwargs):
            return json.loads(cli(command, '--json', *args, **kwargs).stdout)

        def start(selected, allowed):
            process = subprocess.Popen([str(binary), 'serve', '--state', str(selected),
                                        '--browse-root', str(allowed)], env=env,
                                       stdout=log, stderr=log, start_new_session=True)
            daemons.append(process)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise AssertionError('daemon exited before ready')
                if (selected / 'v1.sock').exists():
                    result = subprocess.run([str(binary), 'status', '--state', str(selected), '--json'],
                                            env=env, capture_output=True, timeout=2)
                    if result.returncode == 0:
                        return
                time.sleep(.03)
            raise AssertionError('daemon readiness deadline')

        try:
            start(state, browse)
            start(local_state, home)
            remote_status = rpc('status')['result']
            assert remote_status['host_id'] != rpc('status', remote=False, selected=local_state)['result']['host_id']
            assert 'directories_v1' in rpc('contract')['result']['capabilities']['experimental']
            assert rpc('directories') == rpc('directories', remote=False)
            assert rpc('directories')['result']['roots'] == [str(browse)]
            assert rpc('directories', '--observer', code=3)['code'] == 'operator_required'
            first = rpc('directories', '--path', str(browse), '--limit', '2')
            assert 'request_id' not in first
            assert first == rpc('directories', '--path', str(browse), '--limit', '2', remote=False)
            page = first['result']
            assert [e['name'] for e in page['entries']] == ['alpha', 'beta']
            second = rpc('directories', '--path', str(browse), '--cursor', page['next_cursor'])['result']
            assert [e['name'] for e in second['entries']] == ['gamma', 'outside'] and not second['next_cursor']
            assert rpc('directories', '--path', str(root), code=3)['code'] == 'directory_scope'
            assert rpc('directories', '--path', str(browse / 'outside'), code=3)['code'] == 'directory_unavailable'
            assert rpc('directories', '--path', str(browse / 'missing'), code=2)['code'] == 'directory_missing'
            (browse / 'new').touch()
            assert rpc('directories', '--path', str(browse), '--cursor', page['next_cursor'], code=6)['code'] == 'directory_changed'

            assert rpc('open', '--', '/bin/cat', code=2)['code'] == 'remote_cwd'
            argv_marker = "ARG ' ; $(false) & \" exactly"
            open_args = ['--request-id', 'remote-open-once', '--cwd', str(browse), '--terminal', 'screen-v1',
                         '--', '/bin/sh', '-c', 'printf "%s\\n%s\\n" "$PWD" "$1"; exec /bin/cat',
                         'remote-argv-test', argv_marker]
            # Response lost after execution: reconcile with the same ID, not a
            # second process. Confirm daemon block count and PID stay identical.
            lost = rpc('open', *open_args, code=8, extra_env={'TEST_SSH_MODE': 'drop'})
            assert lost['class'] == 'indeterminate' and lost['request_id'] == 'remote-open-once'
            opened = rpc('open', *open_args)['result']
            block = opened['block_id']
            assert rpc('open', *open_args)['result'] == opened
            assert rpc('status')['result']['total'] == 1
            assert rpc('status', selected=local_state, remote=False)['result']['total'] == 0
            assert rpc('screen', '--observer', '--block', block)['result']['host_id'] == remote_status['host_id']
            assert rpc('open', '--observer', '--cwd', str(browse), '--', '/bin/cat', code=3)['code'] == 'operator_required'
            assert rpc('acquire', '--block', block)['result']['lease_saved'] is True
            assert not (state / ('lease-' + block)).exists(), 'must not save a remote lease at the local state path'
            leases = list(home.rglob('lease-' + block))
            assert len(leases) == 1 and leases[0].stat().st_mode & 0o077 == 0
            rpc('input', '--block', block, data=b'REMOTE_INPUT\n')
            rpc('resize', '--block', block, '--cols', '100', '--rows', '30')
            rpc('renew', '--block', block)
            deadline = time.monotonic() + 4
            while time.monotonic() < deadline:
                events = [json.loads(line) for line in cli('events', '--block', block).stdout.splitlines()]
                output = b''.join(base64.b64decode(e['payload']['data']) for e in events if e['type'] == 'output')
                if b'REMOTE_INPUT' in output and argv_marker.encode() in output:
                    break
                time.sleep(.03)
            assert str(browse).encode() in output and argv_marker.encode() in output and b'REMOTE_INPUT' in output, output
            cast = [json.loads(line) for line in cli('export', '--block', block).stdout.splitlines()]
            assert cast[0]['version'] == 3 and len(cast) > 1
            rpc('release', '--block', block)
            assert not leases[0].exists()
            assert rpc('input', '--block', block, data=b'no lease', code=6)['code'] == 'control_required'
            # Derived terminal contexts, including deferred control release,
            # must keep the remote target rather than dialing a local socket.
            target = ['--host', 'fixture-host', '--remote-binary', str(remote_binary)]
            controller = View(binary, state, block, env, extra_args=target)
            views.append(controller)
            wait_until('remote controller did not render', lambda: b'REMOTE_INPUT' in controller.poll())
            viewer = View(binary, state, block, env, observer=True, extra_args=target)
            views.append(viewer)
            wait_until('remote observer did not render', lambda: b'Observer' in viewer.poll())
            controller.send(b'REMOTE_ATTACH\n')
            wait_until('remote terminal input did not arrive', lambda: b'REMOTE_ATTACH' in viewer.poll())
            viewer.send(b'\x1d'); viewer.finish()
            controller.send(b'\x1d'); controller.finish()
            assert rpc('acquire', '--block', block)['class'] == 'ok', 'attach failed to release its remote lease'
            rpc('release', '--block', block)
            rpc('takeover', '--block', block)
            rpc('stop', '--block', block)
            for mode in ['invalid', 'oversized']:
                failure = rpc('status', code=5, extra_env={'TEST_SSH_MODE': mode})
                assert failure['class'] == 'unreachable'
            # Observer path, authentication failures and disabled browse preserve
            # the daemon's envelope; SSH stderr is not mistaken for a response.
            assert rpc('status', '--observer')['class'] == 'ok'
            assert rpc('status', selected=root / 'absent', code=5)['code'] == 'daemon_not_running'
            print(json.dumps({'class': 'ok', 'check': 'remote-directory-contract',
                              'transport': 'local SSH shim + real stdio bridge, no real network',
                              'checks': ['two independent host states', 'local/remote directory parity',
                                         'scoped roots and cursor changes', 'observer denial',
                                         'quoted remote cwd/argv/binary/state', 'mutation response-loss reconciliation',
                                         'remote lease cache and lifecycle', 'screen/events/export and two terminal clients',
                                         'bounded malformed transport responses']}))
        finally:
            for view in views:
                view.close()
            for process in daemons:
                if process.poll() is None:
                    process.send_signal(signal.SIGTERM)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
            log.close()


if __name__ == '__main__':
    main()
