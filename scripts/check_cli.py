#!/usr/bin/env python3
"""Exercise the real binary in private state, with no providers or live services."""
import argparse
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, default=ROOT / 'bin/continuum')
    opts = parser.parse_args()
    binary = opts.binary.resolve()
    with tempfile.TemporaryDirectory(prefix='continuum-cli-') as directory:
        root = Path(directory)
        state = root / 'state'
        env = {'HOME': str(root), 'PATH': '/usr/bin:/bin', 'TMPDIR': str(root), 'TERM': 'xterm-256color'}
        log = open(root / 'daemon.log', 'wb')
        daemon = None
        followers = []

        def cli(command, *args, code=0, data=None):
            result = subprocess.run([str(binary), command, '--state', str(state), *args],
                                    env=env, input=data, capture_output=True, timeout=12)
            assert result.returncode == code, (command, result.returncode, result.stdout, result.stderr)
            return result

        def rpc(command, *args, code=0, data=None):
            return json.loads(cli(command, '--json', *args, code=code, data=data).stdout)

        def start():
            process = subprocess.Popen([str(binary), 'serve', '--state', str(state)], env=env,
                                       stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise AssertionError('daemon exited before ready')
                if (state / 'endpoint').exists():
                    try:
                        probe = subprocess.run([str(binary), 'status', '--state', str(state), '--json'], env=env, capture_output=True, timeout=2)
                    except subprocess.TimeoutExpired:
                        continue  # a hung probe must not orphan the daemon; the deadline kills it
                    if probe.returncode == 0:
                        return process
                time.sleep(.02)
            process.kill()
            raise AssertionError('daemon readiness deadline')

        try:
            help_text = subprocess.check_output([str(binary), 'help'], env=env, timeout=3)
            assert b'First use' in help_text
            assert rpc('status', code=5)['code'] == 'daemon_not_running'
            daemon = start()
            initial = rpc('status')['result']
            assert initial['total'] == 0
            second = subprocess.run([str(binary), 'serve', '--state', str(state)], env=env,
                                    capture_output=True, timeout=3)
            assert second.returncode != 0
            opened = rpc('open', '--request-id', 'launch-once', '--cwd', str(root), '--', '/bin/cat')
            block = opened['result']['block_id']
            again = rpc('open', '--request-id', 'launch-once', '--cwd', str(root), '--', '/bin/cat')
            assert again['result'] == opened['result']
            assert rpc('open', '--request-id', 'launch-once', '--cwd', str(root), '--', '/bin/sh', code=6)['class'] == 'conflict'
            assert rpc('open', '--observer', '--', '/bin/cat', code=3)['class'] == 'access_denied'
            assert rpc('acquire', '--block', block)['result']['lease_saved']
            for number in range(2):
                output = open(root / f'viewer-{number}.jsonl', 'wb')
                process = subprocess.Popen([str(binary), 'events', '--state', str(state), '--block', block,
                                            '--observer', '--follow'], env=env, stdout=output, stderr=log)
                followers.append((process, output))
            rpc('input', '--block', block, data=b'two-viewers\n')
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                seen = []
                for n in range(2):
                    raw = (root / f'viewer-{n}.jsonl').read_bytes()
                    text = b''.join(base64.b64decode(e['payload']['data']) for e in
                                    (json.loads(line) for line in raw.splitlines()) if e['type'] == 'output')
                    seen.append(b'two-viewers' in text)
                if all(seen):
                    break
                time.sleep(.03)
            assert all(seen), 'both independent viewers must receive output'
            for process, output in followers:
                process.send_signal(signal.SIGINT)
                process.wait(timeout=3)
                output.close()
            followers.clear()

            # Child emits only after all clients have disconnected.
            trigger = root / 'trigger'
            # The marker rides inside a clipboard OSC and a colour SGR: --text keeps
            # the text and colour, drops the OSC; --raw replays every byte.
            shell = ('while [ ! -f "$1" ]; do sleep 0.02; done; '
                     'printf "\\033]52;c;c2VjcmV0\\007\\033[1;32moffline-marker\\033[0m\\n"; exec /bin/cat')
            offline = rpc('open', '--cwd', str(root), '--', '/bin/sh', '-c', shell, 'test-shell', str(trigger))['result']
            trigger.touch()
            time.sleep(.15)
            replay = cli('events', '--block', offline['block_id'], '--text').stdout
            assert b'\x1b[1;32moffline-marker\x1b[0m' in replay, replay
            assert b']52;' not in replay and b'c2VjcmV0' not in replay, replay
            raw = cli('events', '--block', offline['block_id'], '--raw').stdout
            assert b'\x1b]52;c;c2VjcmV0\x07' in raw, raw
            cli('events', '--block', offline['block_id'], '--text', '--raw', code=2)
            assert any(b['pid'] == offline['pid'] for b in rpc('status')['result']['blocks'])

            rpc('resize', '--block', block, '--cols', '120', '--rows', '40')
            rpc('stop', '--block', block)
            # Crash tests actual record recovery, not just a graceful close.
            daemon.kill()
            daemon.wait(timeout=3)
            daemon = start()
            recovered = rpc('status')['result']
            assert recovered['host_id'] == initial['host_id']
            old = next(b for b in recovered['blocks'] if b['id'] == offline['block_id'])
            assert old['state'] == 'interrupted'
            uncertain = rpc('events', '--block', offline['block_id'], code=8)
            assert uncertain['result']['incomplete']
            replay = b''.join(base64.b64decode(e['payload']['data']) for e in uncertain['result']['events'] if e['type'] == 'output')
            assert b'offline-marker' in replay
            assert not (root / '.menagerie').exists(), 'modern mode wrote legacy home'
            print('PASS: first run, auth, one writer, idempotency, two observers, control, offline replay, crash recovery, isolated capture')
        finally:
            for process, output in followers:
                process.kill()
                process.wait(timeout=3)
                output.close()
            if daemon is not None and daemon.poll() is None:
                daemon.send_signal(signal.SIGTERM)
                try:
                    daemon.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(daemon.pid, signal.SIGKILL)
                    daemon.wait(timeout=3)
            log.close()

if __name__ == '__main__':
    main()
