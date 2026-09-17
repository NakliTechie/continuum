#!/usr/bin/env python3
"""Opt-in three-server WAN drill. Uses only temporary daemons and trusted SSH."""
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import signal
import statistics
import subprocess
import tempfile
import time

from check_two_host import ROOT, SSH_OPTS, cli, ready, remote, source

HOST = re.compile(r"[A-Za-z0-9._-]+@[A-Za-z0-9.-]+\Z")
REMOTE_DIR = re.compile(r"/tmp/continuum-wan\.[A-Za-z0-9]+\Z")
PLATFORMS = {
    ("Darwin", "arm64"): ("darwin", "arm64"),
    ("Darwin", "x86_64"): ("darwin", "amd64"),
    ("Linux", "aarch64"): ("linux", "arm64"),
    ("Linux", "x86_64"): ("linux", "amd64"),
}
CASES = ["three distinct daemon identities", "remote any-winner and retained replay",
         "three-source all with one dropped bridge request and isolated peer outage",
         "coordinator SIGKILL/restart during a pending wait", "per-source committed order",
         "two-second bridge delay and bounded application RTT samples", "verified temporary cleanup"]


def validate_hosts(hosts):
    if len(hosts) != 2 or hosts[0] == hosts[1] or not all(HOST.fullmatch(h) for h in hosts):
        raise ValueError("--run requires two distinct, narrow USER@HOST SSH destinations")


def platform(host):
    lines = remote(host, "uname -s && uname -m").splitlines()
    if len(lines) != 2 or tuple(lines) not in PLATFORMS:
        raise RuntimeError(f"unsupported remote platform on {host}: {lines!r}")
    return PLATFORMS[tuple(lines)]


def build(binary, goos, goarch):
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/continuum"], cwd=ROOT, env=env, check=True)


def launch_remote(host, directory):
    binary, state = directory + "/continuum", directory + "/state"
    command = ("nohup " + shlex.quote(binary) + " serve --state " + shlex.quote(state)
               + " --listen 127.0.0.1:0 > " + shlex.quote(directory + "/serve.log")
               + " 2>&1 < /dev/null & echo $!")
    pid = remote(host, command)
    if not pid.isdecimal():
        raise RuntimeError(f"temporary daemon PID unavailable on {host}")
    return pid


def remote_command(host, directory, marker, action="touch"):
    path = shlex.quote(directory + "/" + marker)
    if action == "touch":
        remote(host, "touch " + path)
    elif action == "clear":
        remote(host, "mv " + path + " " + shlex.quote(directory + "/" + marker + ".cleared"))
    else:
        raise ValueError(action)


def stopped(host, pid, directory):
    # Never signal an unrelated process if a PID has been reused.
    command = "ps -p " + pid + " -o stat= -o command="
    probe = subprocess.run(["ssh", "-T", *SSH_OPTS, "--", host, command],
                           capture_output=True, text=True, timeout=12)
    row = probe.stdout.strip()
    if probe.returncode != 0 or not row or row.startswith("Z"):
        return True
    if directory + "/continuum" not in row:
        return True
    remote(host, "kill -TERM " + pid)
    end = time.monotonic() + 15
    while time.monotonic() < end:
        probe = subprocess.run(["ssh", "-T", *SSH_OPTS, "--", host, command],
                               capture_output=True, text=True, timeout=12)
        row = probe.stdout.strip()
        if probe.returncode != 0 or not row or row.startswith("Z") or directory + "/continuum" not in row:
            return True
        time.sleep(.25)
    return False


def assert_wait(result, mode, names, winner=None):
    if result.get("state") != "satisfied" or result.get("mode") != mode or result.get("completion_order", 0) < 1:
        raise RuntimeError(f"{mode} wait not durably satisfied: {result.get('state')}")
    members = result.get("sources", [])
    if {m.get("name") for m in members} != set(names):
        raise RuntimeError(f"{mode} wait lost a source")
    matched = [m for m in members if m.get("matched")]
    if mode == "all" and (len(matched) != len(names) or len({m.get("completion_order") for m in matched}) != len(names)):
        raise RuntimeError("all wait lost a match or committed completion ordering")
    if winner is not None and result.get("winner") != winner:
        raise RuntimeError(f"wrong any winner: {result.get('winner')}")


def wait_result(binary, state, identifier):
    answer = cli(binary, "wait", "attach", "--state", state, "--id", identifier,
                 "--timeout", "90s", timeout=105)
    if answer.get("class") != "ok":
        raise RuntimeError(f"wait {identifier} returned {answer.get('class')}")
    return answer["result"]


def open_block(binary, state, cwd, seconds, host=None, remote_binary=None):
    args = ["open", "--json"]
    if host:
        args += ["--host", host, "--remote-binary", remote_binary]
    args += ["--state", state, "--cwd", cwd, "--", "/bin/sh", "-c", f"sleep {seconds}; printf wan-done"]
    result = cli(binary, *args, timeout=30)
    if result.get("class") != "ok":
        raise RuntimeError("temporary block open failed")
    return result["result"]["block_id"]


def probes(binary, state, host, remote_binary, count):
    timings = []
    errors = 0
    for _ in range(count):
        start = time.monotonic()
        try:
            answer = cli(binary, "status", "--json", "--host", host, "--remote-binary", remote_binary,
                         "--state", state, timeout=20)
            if answer.get("class") != "ok":
                errors += 1
            else:
                timings.append(round((time.monotonic() - start) * 1000))
        except (RuntimeError, subprocess.TimeoutExpired):
            errors += 1
    if not timings:
        raise RuntimeError(f"all application probes failed for {host}")
    return {"attempts": count, "failures": errors, "median_ms": round(statistics.median(timings)),
            "max_ms": max(timings)}


def run(hosts, probe_count):
    validate_hosts(hosts)
    # Platform and authentication are checked before any remote state is created.
    targets = [(host, platform(host)) for host in hosts]
    peers = []
    coordinator = None
    cleanup_errors = []
    result = None
    with tempfile.TemporaryDirectory(prefix="continuum-wan-local-") as temp:
        root = Path(temp)
        binaries = {}
        for _, target in targets:
            if target not in binaries:
                path = root / ("continuum-" + "-".join(target))
                build(path, *target)
                binaries[target] = path
        local_platform = subprocess.check_output(["go", "env", "GOOS", "GOARCH"], text=True).splitlines()
        local_binary = root / "continuum-local"
        build(local_binary, *local_platform)
        binary = str(local_binary)
        local_state = str(root / "state")
        try:
            for index, (host, target) in enumerate(targets):
                directory = remote(host, "mktemp -d /tmp/continuum-wan.XXXXXX")
                if not REMOTE_DIR.fullmatch(directory):
                    raise RuntimeError(f"remote temp path was not narrow on {host}")
                peer = {"host": host, "name": ("alpha", "bravo")[index], "dir": directory,
                        "state": directory + "/state", "binary": directory + "/continuum", "pid": ""}
                peers.append(peer)
                for path, name in ((binaries[target], "continuum"), (ROOT / "scripts/fault_bridge.sh", "fault_bridge.sh")):
                    subprocess.run(["scp", *SSH_OPTS, str(path), f"{host}:{directory}/{name}"],
                                   check=True, capture_output=True, timeout=45)
                remote(host, "chmod 700 " + shlex.quote(directory + "/fault_bridge.sh"))
                peer["pid"] = launch_remote(host, directory)
            with (root / "serve.log").open("w") as log:
                coordinator = subprocess.Popen([binary, "serve", "--state", local_state, "--listen", "127.0.0.1:0"],
                                               stdout=subprocess.DEVNULL, stderr=log)
                ready(binary, local_state)
                host_ids = {cli(binary, "status", "--json", "--state", local_state)["result"]["host_id"]}
                for peer in peers:
                    ready(binary, peer["state"], peer["host"], peer["binary"])
                    identity = cli(binary, "status", "--json", "--host", peer["host"],
                                   "--remote-binary", peer["binary"], "--state", peer["state"])["result"]["host_id"]
                    if identity in host_ids:
                        raise RuntimeError("remote hosts do not have three distinct daemon identities")
                    host_ids.add(identity)
                    added = cli(binary, "wait", "peer-add", "--state", local_state,
                                "--request-id", "wan-pair-" + peer["name"], "--name", peer["name"],
                                "--peer-host", peer["host"], "--peer-state", peer["state"],
                                "--peer-binary", peer["dir"] + "/fault_bridge.sh")
                    if added.get("class") != "ok":
                        raise RuntimeError("private peer pairing failed")
                metrics = {peer["name"]: probes(binary, peer["state"], peer["host"], peer["binary"], probe_count)
                           for peer in peers}
                # Deliberately stagger completion so the remote winner is unambiguous.
                a, b = peers
                ids = {
                    "bravo": open_block(binary, b["state"], b["dir"], 25, b["host"], b["binary"]),
                    "local": open_block(binary, local_state, str(root), 30),
                    "alpha": open_block(binary, a["state"], a["dir"], 12, a["host"], a["binary"]),
                }
                src = [source(name, "local" if name == "local" else name, ids[name])
                       for name in ("alpha", "bravo", "local")]
                created = cli(binary, "wait", "create", "--state", local_state, "--request-id", "wan-any",
                              "--mode", "any", "--deadline", "90s", *sum((["--source", item] for item in src), []))
                if created["result"]["state"] != "pending":
                    raise RuntimeError("any wait did not start pending")
                any_result = wait_result(binary, local_state, "wan-any")
                assert_wait(any_result, "any", ids, "alpha")
                ids = {
                    "alpha": open_block(binary, a["state"], a["dir"], 18, a["host"], a["binary"]),
                    "bravo": open_block(binary, b["state"], b["dir"], 22, b["host"], b["binary"]),
                    "local": open_block(binary, local_state, str(root), 26),
                }
                src = [source(name, "local" if name == "local" else name, ids[name])
                       for name in ("alpha", "bravo", "local")]
                created = cli(binary, "wait", "create", "--state", local_state, "--request-id", "wan-all",
                              "--mode", "all", "--deadline", "90s", *sum((["--source", item] for item in src), []))
                if created["result"]["state"] != "pending":
                    raise RuntimeError("all wait did not start pending")
                remote_command(b["host"], b["dir"], "drop_once")
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    if remote(b["host"], "if test -f " + shlex.quote(b["dir"] + "/dropped") + "; then echo yes; fi") == "yes":
                        break
                    time.sleep(.2)
                else:
                    raise RuntimeError("one dropped bridge request was not exercised")
                remote_command(b["host"], b["dir"], "offline")
                time.sleep(1)
                pending = cli(binary, "wait", "output", "--state", local_state, "--id", "wan-all", allowed=(6,))
                if pending.get("code") != "wait_not_ready":
                    raise RuntimeError("peer outage fabricated an all completion")
                coordinator.kill()
                coordinator.wait(timeout=8)
                coordinator = subprocess.Popen([binary, "serve", "--state", local_state, "--listen", "127.0.0.1:0"],
                                               stdout=subprocess.DEVNULL, stderr=log)
                ready(binary, local_state)
                retained = cli(binary, "wait", "output", "--state", local_state, "--id", "wan-any")["result"]
                if retained != any_result:
                    raise RuntimeError("coordinator restart changed the retained any result")
                remote_command(b["host"], b["dir"], "offline", "clear")
                all_result = wait_result(binary, local_state, "wan-all")
                assert_wait(all_result, "all", ids)
                replay = cli(binary, "wait", "output", "--state", local_state, "--id", "wan-all")["result"]
                if replay != all_result:
                    raise RuntimeError("all result changed on replay")
                remote_command(b["host"], b["dir"], "delay")
                start = time.monotonic()
                rechecked = cli(binary, "wait", "peer-add", "--state", local_state,
                                "--request-id", "wan-pair-recheck", "--name", "bravo", "--peer-host", b["host"],
                                "--peer-state", b["state"], "--peer-binary", b["dir"] + "/fault_bridge.sh", timeout=20)
                delay_seconds = time.monotonic() - start
                if rechecked.get("class") != "ok" or delay_seconds < 1.8:
                    raise RuntimeError("injected two-second bridge delay was not tolerated")
                remote_command(b["host"], b["dir"], "delay", "clear")
                result = {"class": "ok", "check": "wan_multi_server", "hosts": 3, "cases": CASES,
                          "any_winner": any_result["winner"], "all_completion_order": all_result["completion_order"],
                          "application_probes": metrics, "injected_delay_ms": round(delay_seconds * 1000)}
        finally:
            if coordinator is not None and coordinator.poll() is None:
                coordinator.send_signal(signal.SIGTERM)
                try:
                    coordinator.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    coordinator.kill()
                    coordinator.wait()
            for peer in peers:
                try:
                    if peer["pid"] and not stopped(peer["host"], peer["pid"], peer["dir"]):
                        raise RuntimeError("temporary daemon did not stop")
                    if REMOTE_DIR.fullmatch(peer["dir"]):
                        remote(peer["host"], "rm -r -- " + shlex.quote(peer["dir"]))
                except (RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
                    cleanup_errors.append(f"{peer['host']} temporary state retained at {peer['dir']}: {error}")
    if cleanup_errors:
        raise RuntimeError("; ".join(cleanup_errors))
    if result is None:
        raise RuntimeError("WAN suite did not complete")
    result["remote_temp_removed"] = True
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", action="store_true", help="print the cases; no network or state changes")
    parser.add_argument("--run", action="store_true", help="explicitly run against two existing SSH hosts")
    parser.add_argument("--host", action="append", default=[], help="repeat twice: existing trusted USER@HOST")
    parser.add_argument("--probes", type=int, default=5, help="application status probes per remote host, 1–20")
    args = parser.parse_args()
    if args.plan == args.run or not 1 <= args.probes <= 20:
        parser.error("choose exactly one of --plan or --run and 1–20 probes")
    if args.plan:
        if args.host:
            parser.error("--plan does not accept hosts")
        print(json.dumps({"class": "planned", "check": "wan_multi_server", "hosts": 3, "cases": CASES,
                          "remote_run_required": True}))
        return
    try:
        print(json.dumps(run(args.host, args.probes)))
    except (ValueError, RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from error


if __name__ == "__main__":
    main()
