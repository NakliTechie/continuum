#!/usr/bin/env python3
"""Private real-binary stream, flow-control and Nushell journey. No model calls."""
import argparse
import json
import os
from pathlib import Path
import queue
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time


def wait_for(message, predicate, timeout=6):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.03)
    raise AssertionError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    binary = parser.parse_args().binary.resolve()
    nu = os.environ.get('CONTINUUM_TEST_NU') or shutil.which('nu')
    with tempfile.TemporaryDirectory(prefix='continuum-streams-') as directory:
        root = Path(directory).resolve()
        home, tmp, shim_dir = root / 'home', root / 'tmp', root / 'shim'
        for path in (home, tmp, shim_dir):
            path.mkdir(mode=0o700)
        states = [root / 'left-state', root / 'right-state']
        # Exercise D1 remote dispatch and the real owning-host bridge. Network
        # loss/RTT/auth are not established by this local SSH substitute.
        shim = shim_dir / 'ssh'
        shim.write_text(f'''#!{sys.executable}
import os, sys
args = sys.argv[1:]
assert args[:9] == ['-T', '-oBatchMode=yes', '-oStrictHostKeyChecking=yes', '-oConnectTimeout=8', '-oClearAllForwardings=yes', '-oForwardAgent=no', '-oForwardX11=no', '--', 'fixture-host']
assert len(args) == 10
os.execv('/bin/sh', ['/bin/sh', '-c', args[9]])
''')
        shim.chmod(0o700)
        env = dict(os.environ, HOME=str(home), TMPDIR=str(tmp),
                   XDG_CONFIG_HOME=str(home / 'config'), PATH=str(shim_dir) + ':' + os.environ['PATH'])
        daemons, readers, observers = [], [], []
        logs = open(root / 'daemons.log', 'wb')

        def rpc(index, command, *args):
            result = subprocess.run([str(binary), command, '--state', str(states[index]), '--json', *args],
                                    env=env, capture_output=True, timeout=12)
            assert result.returncode == 0, (command, result.returncode, result.stdout, result.stderr)
            return json.loads(result.stdout)['result']

        def source(name, index, block, after=0):
            value = dict(name=name, state=str(states[index]), block_id=block, after=after)
            if index == 1:
                value.update(host='fixture-host', remote_binary=str(binary))
            return json.dumps(value)

        def stream_args(sources, *args):
            command = [str(binary), 'interleave', *args]
            for value in sources:
                command += ['--source', value]
            return command

        def finite(sources, code=0, *args):
            result = subprocess.run(stream_args(sources, *args), env=env, capture_output=True, timeout=15)
            assert result.returncode == code, (result.returncode, result.stdout, result.stderr)
            return [json.loads(row) for row in result.stdout.splitlines()]

        try:
            for index, state in enumerate(states):
                daemon = subprocess.Popen([str(binary), 'serve', '--state', str(state)], env=env,
                                          stdout=logs, stderr=logs, start_new_session=True)
                daemons.append(daemon)
                wait_for('daemon not ready', lambda: (state / 'v1.sock').exists())
                rpc(index, 'status')
            blocks = []
            for index in range(2):
                block = rpc(index, 'open', '--cwd', str(root), '--', '/bin/sh', '-c', f'printf "source-{index}\\n"')['block_id']
                blocks.append(block)
                wait_for('finite job not exited', lambda: rpc(index, 'status', '--block', block)['blocks'][0]['state'] == 'exited')
            sources = [source('left', 0, blocks[0]), source('right', 1, blocks[1])]
            rows = finite(sources, 0, '--type', 'output')
            events = [r for r in rows if r['kind'] == 'event']
            assert {e['source'] for e in events} == {'left', 'right'}
            assert len({e['host_id'] for e in events}) == 2
            assert all(e['type'] == 'output' and e['event'][1] == 'o' for e in events)
            assert rows[-1]['kind'] == 'end' and all(s['state'] == 'completed' for s in rows[-1]['sources'])
            for name in ['left', 'right']:
                seq = [e['seq'] for e in events if e['source'] == name]
                assert seq == sorted(set(seq))
            # Resume the exact checkpoint: there is no second copy of an event.
            resume = [source('left', 0, blocks[0], rows[-1]['sources'][0]['cursor'])]
            assert not any(r['kind'] == 'event' for r in finite(resume))
            absent = json.dumps(dict(name='absent', state=str(root / 'absent'), block_id=blocks[0]))
            partial = finite([absent, sources[1]], 5)
            assert any(r['kind'] == 'event' and r['source'] == 'right' for r in partial)
            assert partial[-1]['exit_code'] == 5

            # Real structured-shell pipeline, isolated from user Nu config.
            if nu:
                nu_env = dict(env, CONTINUUM_TEST_BINARY=str(binary), SOURCE_LEFT=sources[0], SOURCE_RIGHT=sources[1])
                program = '''let rows = (^$env.CONTINUUM_TEST_BINARY interleave --source $env.SOURCE_LEFT --source $env.SOURCE_RIGHT | from json --objects)
let events = ($rows | where kind == event | where type == output)
if ($events | length) < 2 { error make {msg: "missing output events"} }
if ($rows | last | get kind) != "end" { error make {msg: "missing completion"} }
{sources: ($events | get source | uniq | sort), events: ($events | length), triple: ($events | first | get event)} | to json --raw'''
                result = subprocess.run([nu, '--no-config-file', '-c', program], env=nu_env, capture_output=True, timeout=20)
                assert result.returncode == 0, (result.stdout, result.stderr)
                parsed = json.loads(result.stdout)
                assert parsed['sources'] == ['left', 'right'] and len(parsed['triple']) == 3, parsed
                version = subprocess.check_output([nu, '--no-config-file', '--version'], env=env, text=True).strip()
                print(json.dumps({'class': 'ok', 'check': 'nushell-live-pipeline', 'version': version,
                                  'parser': 'from json --objects', 'events': parsed['events']}), flush=True)
            else:
                print(json.dumps({'class': 'warning', 'check': 'nushell-live-pipeline',
                                  'message': 'nu unavailable; set CONTINUUM_TEST_NU to verify interoperability'}), flush=True)

            ticking = []
            for index in range(2):
                ticking.append(rpc(index, 'open', '--cwd', str(root), '--', '/bin/sh', '-c',
                                   'i=0; while [ "$i" -lt 200 ]; do printf "tick-%s\\n" "$i"; i=$((i+1)); sleep .03; done; exec /bin/cat')['block_id'])
            live_sources = [source('left', 0, ticking[0]), source('right', 1, ticking[1])]
            process = subprocess.Popen(stream_args(live_sources, '--follow', '--control', '--timeout', '15s'), env=env,
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            observers.append(process)
            inbox = queue.Queue()
            seen = []

            def read_rows():
                for line in process.stdout:
                    inbox.put(json.loads(line))

            reader = threading.Thread(target=read_rows, daemon=True)
            readers.append(reader)
            reader.start()

            def receive(predicate, timeout=4):
                deadline = time.monotonic() + timeout
                while time.monotonic() < deadline:
                    try:
                        row = inbox.get(timeout=max(.01, deadline - time.monotonic()))
                    except queue.Empty:
                        break
                    seen.append(row)
                    if predicate(row):
                        return row
                raise AssertionError(('stream receive deadline', process.poll(), seen[-5:]))

            def control(identity, operation, name):
                process.stdin.write((json.dumps(dict(request_id=identity, operation=operation, source=name)) + '\n').encode())
                process.stdin.flush()
                return receive(lambda r: r.get('request_id') == identity)

            receive(lambda r: r['kind'] == 'event' and r['source'] == 'left')
            receive(lambda r: r['kind'] == 'event' and r['source'] == 'right')
            paused = control('pause-left', 'pause', 'left')
            assert paused['state'] == 'paused'
            boundary = len(seen)
            time.sleep(.3)
            receive(lambda r: r['kind'] == 'event' and r['source'] == 'right')
            while not inbox.empty():
                seen.append(inbox.get_nowait())
            assert not any(r['kind'] == 'event' and r['source'] == 'left' for r in seen[boundary:])
            recorded = rpc(0, 'status', '--block', ticking[0])['blocks'][0]
            assert recorded['state'] == 'active', 'pause stopped the workload'
            assert control('continue-left', 'continue', 'left')['state'] == 'running'
            resumed = receive(lambda r: r['kind'] == 'event' and r['source'] == 'left')
            assert resumed['seq'] > paused['cursor']
            assert control('cancel-left', 'cancel', 'left')['state'] == 'cancelled'
            assert control('cancel-left', 'cancel', 'left')['replayed'] is True
            assert control('cancel-right', 'cancel', 'right')['state'] == 'cancelled'
            end = receive(lambda r: r['kind'] == 'end')
            process.stdin.close()
            assert process.wait(timeout=3) == 0, process.stderr.read()
            assert all(s['state'] == 'cancelled' for s in end['sources'])
            for name in ['left', 'right']:
                seq = [r['seq'] for r in seen if r['kind'] == 'event' and r['source'] == name]
                assert seq == sorted(set(seq)), (name, seq)
            assert rpc(0, 'status', '--block', ticking[0])['blocks'][0]['pid'] == recorded['pid']

            # Blocked output is local backpressure, not a PTY stop. Prove the
            # producer finishes its 2 MiB burst while the consumer reads nothing.
            trigger, marker = root / 'trigger', root / 'burst-done'
            child = ('import os,time,pathlib; '
                     f't=pathlib.Path({str(trigger)!r}); '
                     '\nwhile not t.exists(): time.sleep(.01)\n'
                     f'os.write(1,b"x"*(2<<20)); pathlib.Path({str(marker)!r}).touch(); time.sleep(15)')
            burst = rpc(0, 'open', '--cwd', str(root), '--', sys.executable, '-c', child)['block_id']
            blocked = subprocess.Popen(stream_args([source('burst', 0, burst)], '--follow'), env=env,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            observers.append(blocked)
            trigger.touch()
            wait_for('backpressure stalled the PTY producer', marker.exists)
            time.sleep(.2)
            assert blocked.poll() is None
            blocked.send_signal(signal.SIGINT)
            assert blocked.wait(timeout=3) == 130, blocked.stderr.read()
            assert rpc(0, 'status', '--block', burst)['blocks'][0]['state'] == 'active'
            # A closed downstream pipe reports output failure, not success.
            broken = subprocess.Popen(stream_args([source('burst', 0, burst)]), env=env,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            observers.append(broken)
            broken.stdout.close()
            assert broken.wait(timeout=3) == 5, broken.stderr.read()
            print(json.dumps({'class': 'ok', 'check': 'stream-composition',
                              'transport': 'two private daemon states; local SSH shim, not real network',
                              'checks': ['label/order/filter', 'checkpoint resume', 'source failure isolation',
                                         'pause/continue/cancel and correlated replay', 'same-PID work survives',
                                         'blocked-output PTY independence and SIGINT', 'broken pipe exit']}), flush=True)
        finally:
            for process in observers + daemons:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
            for reader in readers:
                reader.join(timeout=1)
            logs.close()


if __name__ == '__main__':
    main()
