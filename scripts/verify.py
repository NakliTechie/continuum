#!/usr/bin/env python3
"""Worktree-safe verifier: doctor | verify [core|cli|legacy]. No live endpoint reuse."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]

def run(args, **kwargs):
    subprocess.run(args, cwd=ROOT, check=True, **kwargs)

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('command', choices=['doctor', 'verify'])
    p.add_argument('feature', nargs='?', choices=['core', 'cli', 'legacy'])
    opts = p.parse_args()
    missing = [name for name in ['go', 'git', 'python3'] if not shutil.which(name)]
    if missing:
        raise SystemExit('Install required tools: ' + ', '.join(missing))
    if not (ROOT / 'go.mod').is_file():
        raise SystemExit('Run the committed verifier from a Continuum checkout.')
    if opts.command == 'doctor':
        version = subprocess.check_output(['go', 'version'], text=True).strip()
        print(json.dumps({'class': 'ok', 'check': 'doctor', 'checkout': str(ROOT),
                          'go': version, 'mode': 'fresh temporary binaries/state per run',
                          'browser': 'manual Menagerie compatibility journey; not covered by this script'}))
        return
    features = [opts.feature] if opts.feature else ['core', 'cli', 'legacy']
    with tempfile.TemporaryDirectory(prefix='continuum-verify-') as directory:
        temp = Path(directory)
        if 'core' in features:
            run(['go', 'test', '-race', '-count=1', '-timeout=120s', './...'])
            run(['go', 'vet', './...'])
        if 'cli' in features:
            binary = temp / 'continuum'
            run(['go', 'build', '-o', str(binary), './cmd/continuum'])
            run(['python3', 'scripts/check_cli.py', '--binary', str(binary)])
        if 'legacy' in features:
            relay, fake = temp / 'menagerie-relay', temp / 'fake-acp'
            run(['go', 'build', '-o', str(relay), './cmd/menagerie-relay'])
            run(['go', 'build', '-o', str(fake), './internal/server/testdata/fakeagent'])
            env = dict(os.environ, CONTINUUM_TEST_RELAY=str(relay), CONTINUUM_TEST_ACP=str(fake))
            run(['go', 'test', '-race', '-tags=legacyintegration', '-count=1',
                 '-timeout=90s', './compat'], env=env)
    print(json.dumps({'class': 'ok', 'check': 'verify', 'features': features, 'checkout': str(ROOT)}))

if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        print(json.dumps({'class': 'failed', 'exit_code': error.returncode}))
        raise SystemExit(error.returncode)
