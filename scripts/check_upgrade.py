#!/usr/bin/env python3
"""Exercise old/new clients, schema migration and backup-based rollback privately.

The service install steps are dry runs into a temporary HOME. Daemons run in
the foreground against temporary state, so this checker never registers an OS
service or reads the installed Continuum/Menagerie state.
"""
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

ROOT = Path(__file__).resolve().parents[1]
SCHEMA1_REF = "7939a3a"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/continuum")
    parser.add_argument("--baseline-ref", default=SCHEMA1_REF)
    opts = parser.parse_args()
    candidate = opts.binary.resolve()

    with tempfile.TemporaryDirectory(prefix="continuum-upgrade-") as directory:
        root = Path(directory)
        home, tmp = root / "home", root / "tmp"
        home.mkdir(mode=0o700)
        tmp.mkdir(mode=0o700)
        env = dict(os.environ, HOME=str(home), TMPDIR=str(tmp), TERM="xterm-256color")
        source, baseline = root / "baseline-source", root / "continuum-schema1"
        source.mkdir()
        archive = root / "baseline.tar"
        subprocess.run(["git", "archive", "--format=tar", "-o", str(archive), opts.baseline_ref],
                       cwd=ROOT, env=env, check=True, timeout=20)
        subprocess.run(["tar", "-xf", str(archive), "-C", str(source)],
                       env=env, check=True, timeout=20)
        subprocess.run(["go", "build", "-o", str(baseline), "./cmd/continuum"],
                       cwd=source, env=env, check=True, timeout=120)

        state = root / "state"
        backup = root / "schema1-backup.db"
        daemon = None
        logs = []
        child_pids = set()

        def cli(binary, command, *args, code=0, data=None, timeout=12):
            result = subprocess.run([str(binary), command, "--state", str(state), *args],
                                    env=env, input=data, capture_output=True, timeout=timeout)
            assert result.returncode == code, (
                binary.name, command, result.returncode, result.stdout, result.stderr)
            return result

        def rpc(binary, command, *args, code=0, data=None):
            return json.loads(cli(binary, command, "--json", *args, code=code, data=data).stdout)

        def service_dry_run(binary):
            dry = dict(env, CONTINUUM_SERVICE_DRYRUN="1")
            result = subprocess.run(
                [str(binary), "service", "install", "--state", str(state),
                 "--listen", "127.0.0.1:58750"],
                env=dry, capture_output=True, timeout=12)
            assert result.returncode == 0, (binary.name, result.stdout, result.stderr)
            assert b"dry run" in result.stdout.lower(), result.stdout
            if sys.platform == "darwin":
                unit = home / "Library/LaunchAgents/com.naklitechie.continuum.plist"
            elif sys.platform.startswith("linux"):
                unit = home / ".config/systemd/user/continuum.service"
            else:
                raise AssertionError("service matrix requires macOS or Linux")
            body = unit.read_text()
            assert str(binary.resolve()) in body, (binary, body)
            return body

        def start(binary):
            nonlocal daemon
            log = open(root / f"daemon-{len(logs)}-{binary.name}.log", "wb")
            logs.append(log)
            daemon = subprocess.Popen([str(binary), "serve", "--state", str(state)],
                                      env=env, stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic() + 7
            while time.monotonic() < deadline:
                if daemon.poll() is not None:
                    log.flush()
                    raise AssertionError(
                        f"{binary.name} exited before ready: {Path(log.name).read_text(errors='replace')}")
                probe = subprocess.run([str(binary), "status", "--state", str(state), "--json"],
                                       env=env, capture_output=True, timeout=2)
                if probe.returncode == 0:
                    return daemon
                time.sleep(0.03)
            raise AssertionError(f"{binary.name} readiness deadline")

        def stop_daemon(replacement=False):
            nonlocal daemon
            if daemon is None:
                return
            if daemon.poll() is None:
                # A service replacement must terminate only the daemon. SIGTERM
                # is the foreground "stop my work" path; SIGKILL models the
                # supervisor handoff and leaves holder sessions running.
                daemon.send_signal(signal.SIGKILL if replacement else signal.SIGTERM)
                try:
                    daemon.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    os.killpg(daemon.pid, signal.SIGKILL)
                    daemon.wait(timeout=3)
            daemon = None

        def blocks(binary):
            return rpc(binary, "status")["result"]["blocks"]

        def output(binary, block):
            result = subprocess.run([str(binary), "events", "--state", str(state),
                                     "--block", block, "--json"], env=env,
                                    capture_output=True, timeout=12)
            assert result.returncode in (0, 8), (
                binary.name, "events", result.returncode, result.stdout, result.stderr)
            events = [json.loads(line) for line in result.stdout.splitlines()]
            if len(events) == 1 and "result" in events[0]:
                events = events[0]["result"]["events"]
            return b"".join(base64.b64decode(event["payload"]["data"])
                            for event in events if event["type"] == "output")

        def wait_for(description, predicate, timeout=5):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if predicate():
                    return
                time.sleep(0.04)
            raise AssertionError(description)

        try:
            # Install cell: the schema-1 service unit is rendered exactly as it
            # would be installed, but under private HOME and without registering it.
            old_unit = service_dry_run(baseline)
            assert str(baseline) in old_unit
            start(baseline)
            initial = rpc(baseline, "status")["result"]
            assert rpc(candidate, "directories", code=9)["code"] == "directories_v1", "new directory client must fail closed on old daemon"
            opened = rpc(baseline, "open", "--cwd", str(root), "--", "/bin/cat")["result"]
            block, child_pid = opened["block_id"], opened["pid"]
            child_pids.add(child_pid)
            rpc(baseline, "acquire", "--block", block)
            rpc(baseline, "input", "--block", block, data=b"before-upgrade\n")
            wait_for("schema-1 daemon did not journal PTY output",
                     lambda: b"before-upgrade" in output(baseline, block))
            stop_daemon(replacement=True)

            # A rollback point must be taken while the database lock is free.
            shutil.copy2(state / "state.db", backup)

            # Upgrade cell: rewrite the same service unit to the candidate, then
            # run that binary against the schema-1 state. The holder and child stay
            # alive across the daemon replacement.
            new_unit = service_dry_run(candidate)
            assert str(candidate) in new_unit and new_unit != old_unit
            start(candidate)
            upgraded = rpc(baseline, "status")["result"]  # old client / new daemon
            same = next(item for item in upgraded["blocks"] if item["id"] == block)
            assert same["state"] == "active" and same["pid"] == child_pid, same
            assert rpc(candidate, "contract")["result"]["schema_version"] == 1
            rpc(baseline, "acquire", "--block", block)
            rpc(baseline, "input", "--block", block, data=b"after-upgrade\n")
            wait_for("old client could not drive the preserved process on the new daemon",
                     lambda: b"after-upgrade" in output(baseline, block))
            stop_daemon(replacement=True)

            # Blind downgrade must fail: schema 2 protects recording semantics
            # even though the public /v1 response schema remains version 1.
            refused = subprocess.run([str(baseline), "serve", "--state", str(state)],
                                     env=env, capture_output=True, timeout=8)
            assert refused.returncode == 5, (refused.returncode, refused.stdout, refused.stderr)
            assert b"unsupported state schema" in refused.stderr, refused.stderr

            # Rollback cell: restore the matching schema-1 database first, then
            # restore the old unit/binary. The same holder-owned process is adopted.
            restore = root / "state.db.restore"
            shutil.copy2(backup, restore)
            os.replace(restore, state / "state.db")
            rolled_unit = service_dry_run(baseline)
            assert rolled_unit == old_unit
            start(baseline)
            rolled = rpc(candidate, "status")["result"]  # new client / old daemon
            assert rolled["host_id"] == initial["host_id"], rolled
            same = next(item for item in rolled["blocks"] if item["id"] == block)
            assert same["state"] == "active" and same["pid"] == child_pid, same
            wait_for("rollback did not recover the holder's post-backup output tail",
                     lambda: b"after-upgrade" in output(candidate, block))

            # A new client may use the old daemon's shared contract, but must fail
            # closed before mutation when it requests a privacy-sensitive feature
            # the old daemon does not advertise.
            before = rpc(candidate, "status")["result"]["total"]
            denied = rpc(candidate, "open", "--recording", "none", "--cwd", str(root),
                         "--", "/bin/sh", "-c", "printf should-not-run", code=9)
            assert denied["class"] == "unsupported" and denied["code"] == "recording_policy", denied
            assert rpc(candidate, "status")["result"]["total"] == before
            compatible = rpc(candidate, "open", "--cwd", str(root), "--",
                             "/bin/sh", "-c", "printf new-client-old-daemon")["result"]
            child_pids.add(compatible["pid"])
            wait_for("new client default open failed against old daemon",
                     lambda: any(item["id"] == compatible["block_id"] and item["state"] == "exited"
                                 for item in blocks(candidate)))
            assert b"new-client-old-daemon" in output(candidate, compatible["block_id"])
            child_pids.discard(compatible["pid"])

            rpc(candidate, "acquire", "--block", block)
            rpc(candidate, "input", "--block", block, data=b"after-rollback\n")
            wait_for("new client could not drive the preserved process after rollback",
                     lambda: b"after-rollback" in output(candidate, block))
            rpc(candidate, "stop", "--block", block)
            wait_for("preserved block did not exit after rollback", lambda: all(
                item["id"] != block or item["state"] == "exited" for item in blocks(candidate)))
            child_pids.discard(child_pid)
            stop_daemon()
            print("PASS: private service install/update/rollback units, schema-1 migration, "
                  "blind-downgrade refusal, matching-backup rollback, same-PID holder continuity, "
                  "old-client/new-daemon and new-client/old-daemon compatibility")
        finally:
            stop_daemon()
            for pid in child_pids:
                try:
                    os.killpg(pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass
            for log in logs:
                log.close()


if __name__ == "__main__":
    main()
