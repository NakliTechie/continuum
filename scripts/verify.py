#!/usr/bin/env python3
"""Worktree-safe verifier: doctor | verify [core|cli|terminal|legacy|upgrade]. No live endpoint reuse."""
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

def packages():
    """Tracked Go packages only. `./...` would also compile untracked Go under the
    gitignored plan/ directory, so the gate's evidence would depend on local files."""
    out = subprocess.check_output(['git', 'ls-files', '-z', '--', '*.go'], cwd=ROOT)
    dirs = sorted({str(Path(f.decode()).parent) for f in out.split(b'\0') if f})
    dirs = ['./' + d if d != '.' else '.' for d in dirs]
    # Tag-gated packages (compat/) have no files without their tag; every other
    # load error must reach the gate, so only that one message is filtered.
    listed = subprocess.run(['go', 'list', '-e', '-f', '{{.ImportPath}}\t{{if .Error}}{{.Error.Err}}{{end}}']
                            + dirs, cwd=ROOT, capture_output=True, text=True, check=True).stdout
    return [line.split('\t')[0] for line in listed.splitlines()
            if 'build constraints exclude all Go files' not in line]

def stray_go_dirs():
    """Directories where `go list ./...` fails to load, e.g. evidence snapshots under plan/."""
    result = subprocess.run(['go', 'list', './...'], cwd=ROOT, capture_output=True, text=True)
    if result.returncode == 0:
        return []
    return sorted({line.rsplit(' in ', 1)[-1].strip() for line in result.stderr.splitlines()
                   if ' in ' in line and 'found packages' in line} or {'see: go list ./...'})

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('command', choices=['doctor', 'verify'])
    p.add_argument('feature', nargs='?', choices=['core', 'cli', 'terminal', 'legacy', 'upgrade'])
    opts = p.parse_args()
    missing = [name for name in ['go', 'git', 'python3'] if not shutil.which(name)]
    if missing:
        raise SystemExit('Install required tools: ' + ', '.join(missing))
    if not (ROOT / 'go.mod').is_file():
        raise SystemExit('Run the committed verifier from a Continuum checkout.')
    if opts.command == 'doctor':
        version = subprocess.check_output(['go', 'version'], text=True).strip()
        report = {'class': 'ok', 'check': 'doctor', 'checkout': str(ROOT),
                  'go': version, 'mode': 'fresh temporary binaries/state per run',
                  'packages': len(packages()),
                  'browser': 'manual Menagerie compatibility journey; not covered by this script'}
        stray = stray_go_dirs()
        if stray:
            report['warning'] = ('`go list ./...` cannot load these directories; the gate tests tracked '
                                 'packages only, but raw `go test ./...` fails here. Move the files or add '
                                 'an empty go.mod (`module plan`) at plan/ so Go skips it.')
            report['stray_go_dirs'] = stray
        print(json.dumps(report))
        return
    features = [opts.feature] if opts.feature else ['core', 'cli', 'terminal', 'legacy', 'upgrade']
    with tempfile.TemporaryDirectory(prefix='continuum-verify-') as directory:
        temp = Path(directory)
        home, tmp = temp / 'home', temp / 'tmp'
        home.mkdir()
        tmp.mkdir()
        # Inherited capture defaults use HOME. Keep every fake capture away
        # from the installed Menagerie directory while reusing only Go caches.
        go_env = json.loads(subprocess.check_output(
            ['go', 'env', '-json', 'GOCACHE', 'GOMODCACHE', 'GOPATH'], cwd=ROOT, text=True))
        env = dict(os.environ, **go_env, HOME=str(home), TMPDIR=str(tmp))
        if 'core' in features:
            pkgs = packages()
            # internal/server is intentionally load-heavy and takes ~125s on
            # the reference macOS host. Keep a real deadline without making
            # normal scheduler variance fail the whole repository gate.
            run(['go', 'test', '-race', '-count=1', '-timeout=180s'] + pkgs, env=env)
            run(['go', 'vet'] + pkgs, env=env)
            if shutil.which('govulncheck'):
                run(['govulncheck'] + pkgs, env=env)
            else:
                print(json.dumps({'class': 'warning', 'check': 'govulncheck',
                                  'message': 'govulncheck is not installed; known-vulnerability scan skipped'}))
        if 'cli' in features:
            binary = temp / 'continuum'
            run(['go', 'build', '-o', str(binary), './cmd/continuum'], env=env)
            run(['python3', 'scripts/check_cli.py', '--binary', str(binary)], env=env)
        if 'terminal' in features:
            binary = temp / 'continuum-terminal'
            run(['go', 'build', '-o', str(binary), './cmd/continuum'], env=env)
            run(['python3', 'scripts/check_terminal.py', '--binary', str(binary), '--renewal'], env=env)
        if 'legacy' in features:
            relay, fake = temp / 'menagerie-relay', temp / 'fake-acp'
            run(['go', 'build', '-o', str(relay), './cmd/menagerie-relay'], env=env)
            run(['go', 'build', '-o', str(fake), './internal/server/testdata/fakeagent'], env=env)
            env = dict(env, CONTINUUM_TEST_RELAY=str(relay), CONTINUUM_TEST_ACP=str(fake))
            run(['go', 'test', '-race', '-tags=legacyintegration', '-count=1',
                 '-timeout=90s', './compat'], env=env)
        if 'upgrade' in features:
            binary = temp / 'continuum-upgrade-candidate'
            run(['go', 'build', '-o', str(binary), './cmd/continuum'], env=env)
            run(['python3', 'scripts/check_upgrade.py', '--binary', str(binary)], env=env)
    print(json.dumps({'class': 'ok', 'check': 'verify', 'features': features, 'checkout': str(ROOT)}))

if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        print(json.dumps({'class': 'failed', 'exit_code': error.returncode}))
        raise SystemExit(error.returncode)
