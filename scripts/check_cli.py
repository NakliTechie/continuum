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
                if (state / 'v1.sock').exists():
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
            assert rpc('status')['result']['process_restart_survival'] is True
            # A block that keeps talking while no daemon is alive: its holder must
            # keep the process and the unobserved output.
            ticker = rpc('open', '--cwd', str(root), '--', '/bin/sh', '-c',
                         'i=0; while :; do i=$((i+1)); echo tick $i; sleep 0.05; done')['result']
            # A block that exits while no daemon is alive leaves an exit record.
            leaver = rpc('open', '--cwd', str(root), '--', '/bin/sh', '-c', 'sleep 0.4; echo late-words; exit 7')['result']
            time.sleep(.2)
            # Crash tests actual record recovery, not just a graceful close: SIGKILL
            # to the daemon alone; holders and their children are on their own.
            daemon.kill()
            daemon.wait(timeout=3)
            time.sleep(.8)
            assert os.path.exists(f'/proc/{ticker["pid"]}') if os.path.isdir('/proc') else os.kill(ticker['pid'], 0) is None
            daemon = start()
            recovered = rpc('status')['result']
            assert recovered['host_id'] == initial['host_id']
            old = next(b for b in recovered['blocks'] if b['id'] == offline['block_id'])
            assert old['state'] == 'active' and old['pid'] == offline['pid'], old
            assert old['history_incomplete'] is True
            uncertain = rpc('events', '--block', offline['block_id'], code=8)
            assert uncertain['result']['incomplete']
            kinds = [e['type'] for e in uncertain['result']['events']]
            assert 'adopted' in kinds and 'exited' not in kinds, kinds
            replay = b''.join(base64.b64decode(e['payload']['data']) for e in uncertain['result']['events'] if e['type'] == 'output')
            assert b'offline-marker' in replay
            # The adopted block still takes input from the new daemon.
            rpc('acquire', '--block', offline['block_id'])
            rpc('input', '--block', offline['block_id'], data=b'after-restart\n')
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                page = rpc('events', '--block', offline['block_id'], code=8)['result']['events']
                if any(b'after-restart' in base64.b64decode(e['payload']['data']) for e in page if e['type'] == 'output'):
                    break
                time.sleep(.03)
            else:
                raise AssertionError('adopted block did not echo input after restart')
            live = next(b for b in recovered['blocks'] if b['id'] == ticker['block_id'])
            assert live['state'] == 'active' and live['pid'] == ticker['pid'], live
            ticks = rpc('events', '--block', ticker['block_id'], code=8)['result']['events']
            adopted = next(e for e in ticks if e['type'] == 'adopted')
            assert adopted['payload']['held'] and adopted['payload']['gap_bytes'] > 0 and adopted['payload']['dropped_bytes'] == 0, adopted
            numbers = [int(n) for e in ticks if e['type'] == 'output'
                       for n in base64.b64decode(e['payload']['data']).decode().replace('\r', '').split()
                       if n.isdigit()]
            assert numbers == list(range(1, len(numbers) + 1)), f'ticks not contiguous across the restart: {numbers[:40]}'
            gone = next(b for b in recovered['blocks'] if b['id'] == leaver['block_id'])
            assert gone['state'] == 'exited', gone
            left = rpc('events', '--block', leaver['block_id'], code=8)['result']['events']
            assert [e['type'] for e in left][-2:] == ['adopted', 'exited'] and left[-1]['payload']['exit_code'] == 7, left[-3:]
            unobserved = [e for e in left if e['type'] == 'output' and e['payload'].get('unobserved')]
            assert unobserved and b'late-words' in b''.join(base64.b64decode(e['payload']['data']) for e in unobserved), 'downtime tail must be journaled unobserved'
            rpc('acquire', '--block', ticker['block_id'])
            rpc('stop', '--block', ticker['block_id'])
            rpc('stop', '--block', offline['block_id'])
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline and any(b['state'] == 'active' for b in rpc('status')['result']['blocks']):
                time.sleep(.03)
            assert not any(b['state'] == 'active' for b in rpc('status')['result']['blocks'])
            assert not any(p.suffix == '.sock' for p in (state / 'holders').iterdir()), 'holders left sockets behind'
            assert not (root / '.menagerie').exists(), 'modern mode wrote legacy home'
            print('PASS: first run, auth, one writer, idempotency, two observers, control, offline replay, crash survival with holders, isolated capture')
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
