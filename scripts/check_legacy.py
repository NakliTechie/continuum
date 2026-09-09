#!/usr/bin/env python3
"""Build the pinned relay in temporary storage and run black-box conformance."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, default=ROOT.parent / 'menagerie',
                        help='local Menagerie Git repository; no fetch or working-tree edits')
    options = parser.parse_args()
    source = options.source.resolve()
    baseline = json.loads((ROOT / 'docs/compatibility-baseline.json').read_text())
    revision = baseline['revision']
    for item in baseline['files']:
        data = subprocess.check_output(['git', '-C', str(source), 'show',
                                        revision + ':' + item['path']])
        if len(data) != item['bytes'] or hashlib.sha256(data).hexdigest() != item['sha256']:
            raise SystemExit('Baseline mismatch: ' + item['path'])
    print(f"Verified {len(baseline['files'])} pinned source/fixture hashes at {revision[:7]}", flush=True)

    # The archive is from the verified local Git revision, never a moving checkout.
    with tempfile.TemporaryDirectory(prefix='continuum-conformance-') as directory:
        temp = Path(directory)
        archive = temp / 'relay.tar'
        run(['git', '-C', str(source), 'archive', '--format=tar', '-o', str(archive),
             revision, 'relay-go'])
        run(['tar', '-xf', str(archive), '-C', str(temp)])
        relay = temp / 'relay'
        fake = temp / 'fake-acp'
        run(['go', 'build', '-o', str(relay), './cmd/menagerie-relay'], cwd=temp / 'relay-go')
        run(['go', 'build', '-o', str(fake), './internal/server/testdata/fakeagent'],
            cwd=temp / 'relay-go')
        env = dict(os.environ, CONTINUUM_TEST_RELAY=str(relay), CONTINUUM_TEST_ACP=str(fake))
        run(['go', 'test', '-race', '-count=1', '-tags=legacyintegration',
             '-timeout=90s', '-v', './compat'], cwd=ROOT, env=env)


if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.returncode)
