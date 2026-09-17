#!/usr/bin/env python3
"""Private two-host D4 drill over existing trusted SSH; never installs a service."""
import argparse
import json
import os
import pathlib
import re
import shlex
import signal
import subprocess
import tempfile
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
SSH_OPTS = ["-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=8", "-oClearAllForwardings=yes", "-oForwardAgent=no"]


def remote(host, command, timeout=20):
    return subprocess.run(["ssh", "-T", *SSH_OPTS, "--", host, command], text=True, capture_output=True, timeout=timeout, check=True).stdout.strip()


def cli(binary, *args, allowed=(0,), timeout=30):
    p = subprocess.run([binary, *args], text=True, capture_output=True, timeout=timeout)
    if p.returncode not in allowed:
        raise RuntimeError(f"continuum {args[0]} returned {p.returncode}: {p.stderr[:300]}")
    try:
        return json.loads(p.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError(f"continuum {args[0]} did not return JSON") from error


def ready(binary, state, remote_host=None, remote_binary=None):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        args = ["status", "--json", "--state", state]
        if remote_host:
            args += ["--host", remote_host, "--remote-binary", remote_binary]
        try:
            if cli(binary, *args, timeout=4)["class"] == "ok":
                return
        except (RuntimeError, subprocess.TimeoutExpired):
            pass
        time.sleep(.15)
    raise RuntimeError("private daemon did not become ready")


def source(name, peer, block):
    return json.dumps({"name": name, "peer": peer, "block_id": block, "until": ["exited"]}, separators=(",", ":"))


def wait_for(binary, state, identifier, timeout="35s"):
    result = cli(binary, "wait", "attach", "--state", state, "--id", identifier, "--timeout", timeout, timeout=45)
    if result["class"] != "ok":
        raise RuntimeError(f"wait {identifier} returned {result['class']}")
    return result["result"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", required=True, help="existing trusted USER@HOST")
    args = parser.parse_args()
    host = args.host
    if not re.fullmatch(r"[A-Za-z0-9._-]+@[A-Za-z0-9.:-]+", host):
        raise SystemExit("a narrow USER@HOST SSH destination is required")
    remote_dir = ""
    remote_pid = ""
    local_server = None
    outcome = None
    with tempfile.TemporaryDirectory(prefix="continuum-d4-local-") as temp:
        root = pathlib.Path(temp)
        binary = root / "continuum"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/continuum"], cwd=ROOT, check=True)
        remote_dir = remote(host, "mktemp -d /tmp/continuum-d4.XXXXXX")
        if not re.fullmatch(r"/tmp/continuum-d4\.[A-Za-z0-9]+", remote_dir):
            raise RuntimeError("remote temp directory was not narrow and verifiable")
        try:
            for source_path, name in [(binary, "continuum"), (ROOT / "scripts/fault_bridge.sh", "fault_bridge.sh")]:
                subprocess.run(["scp", *SSH_OPTS, str(source_path), f"{host}:{remote_dir}/{name}"], check=True, capture_output=True, timeout=30)
            remote(host, "chmod 700 " + shlex.quote(remote_dir + "/fault_bridge.sh"))
            remote_bin, remote_state = remote_dir + "/continuum", remote_dir + "/state"
            launch = "nohup " + shlex.quote(remote_bin) + " serve --state " + shlex.quote(remote_state) + " --listen 127.0.0.1:0 > " + shlex.quote(remote_dir + "/serve.log") + " 2>&1 < /dev/null & echo $!"
            remote_pid = remote(host, launch)
            if not remote_pid.isdigit():
                raise RuntimeError("remote daemon PID unavailable")
            local_state = str(root / "state")
            with (root / "serve.log").open("w") as local_log:
                local_server = subprocess.Popen([str(binary), "serve", "--state", local_state, "--listen", "127.0.0.1:0"], stdout=subprocess.DEVNULL, stderr=local_log)
                ready(str(binary), local_state)
                ready(str(binary), remote_state, host, remote_bin)
                pair = cli(str(binary), "wait", "peer-add", "--state", local_state, "--request-id", "d4-pair", "--name", "studio", "--peer-host", host, "--peer-state", remote_state, "--peer-binary", remote_dir + "/fault_bridge.sh")
                if pair["class"] != "ok":
                    raise RuntimeError("private peer pairing failed")
                local = cli(str(binary), "open", "--json", "--state", local_state, "--cwd", str(root), "--", "/bin/sh", "-c", "sleep 18; printf local")
                remote_block = cli(str(binary), "open", "--json", "--host", host, "--remote-binary", remote_bin, "--state", remote_state, "--cwd", remote_dir, "--", "/bin/sh", "-c", "sleep 8; printf remote")
                local_id, remote_id = local["result"]["block_id"], remote_block["result"]["block_id"]
                created = cli(str(binary), "wait", "create", "--state", local_state, "--request-id", "d4-any", "--mode", "any", "--deadline", "35s", "--source", source("local", "local", local_id), "--source", source("remote", "studio", remote_id))
                if created["result"]["state"] != "pending":
                    raise RuntimeError("any wait did not persist pending state")
                remote(host, "touch " + shlex.quote(remote_dir + "/drop_once"))
                for _ in range(30):
                    if remote(host, "if test -f " + shlex.quote(remote_dir + "/dropped") + "; then echo yes; fi") == "yes":
                        break
                    time.sleep(.1)
                else:
                    raise RuntimeError("single dropped bridge request was not exercised")
                remote(host, "touch " + shlex.quote(remote_dir + "/offline"))
                time.sleep(1.2)
                pending = cli(str(binary), "wait", "output", "--state", local_state, "--id", "d4-any", allowed=(6,))
                if pending["code"] != "wait_not_ready":
                    raise RuntimeError("SSH outage fabricated completion")
                remote(host, "mv " + shlex.quote(remote_dir + "/offline") + " " + shlex.quote(remote_dir + "/recovered"))
                any_result = wait_for(str(binary), local_state, "d4-any")
                if any_result["state"] != "satisfied" or any_result["winner"] != "remote":
                    raise RuntimeError("cross-host any did not preserve remote winner")
                next_remote = cli(str(binary), "open", "--json", "--host", host, "--remote-binary", remote_bin, "--state", remote_state, "--cwd", remote_dir, "--", "/bin/sh", "-c", "sleep 7; printf resumed")
                next_id = next_remote["result"]["block_id"]
                cli(str(binary), "wait", "create", "--state", local_state, "--request-id", "d4-restart", "--mode", "all", "--deadline", "30s", "--source", source("remote", "studio", next_id))
                local_server.kill()
                local_server.wait(timeout=5)
                local_server = subprocess.Popen([str(binary), "serve", "--state", local_state, "--listen", "127.0.0.1:0"], stdout=subprocess.DEVNULL, stderr=local_log)
                ready(str(binary), local_state)
                retained_any = cli(str(binary), "wait", "output", "--state", local_state, "--id", "d4-any")["result"]
                if retained_any["winner"] != any_result["winner"] or retained_any["completion_order"] != any_result["completion_order"]:
                    raise RuntimeError("coordinator restart changed retained winner or completion order")
                resumed = wait_for(str(binary), local_state, "d4-restart")
                if resumed["state"] != "satisfied":
                    raise RuntimeError("coordinator restart lost remote wait")
                remote(host, "touch " + shlex.quote(remote_dir + "/delay"))
                delayed = cli(str(binary), "wait", "peer-add", "--state", local_state, "--request-id", "d4-pair-recheck", "--name", "studio", "--peer-host", host, "--peer-state", remote_state, "--peer-binary", remote_dir + "/fault_bridge.sh", timeout=15)
                if delayed["class"] != "ok":
                    raise RuntimeError("2s bridge latency broke identity pin")
                remote(host, "mv " + shlex.quote(remote_dir + "/delay") + " " + shlex.quote(remote_dir + "/delayed"))
                outcome = {"class": "ok", "check": "real_two_host", "hosts": 2, "cross_host_any_winner": any_result["winner"], "completion_order": any_result["completion_order"], "retained_result_replayed": True, "coordinator_restart": resumed["state"], "faults": ["one dropped SSH bridge request", "SSH bridge outage", "coordinator SIGKILL/restart", "2s bridge latency"]}
        finally:
            if local_server is not None and local_server.poll() is None:
                local_server.send_signal(signal.SIGTERM)
                try:
                    local_server.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    local_server.kill()
                    local_server.wait()
            if remote_pid.isdigit():
                try:
                    remote(host, "kill -TERM " + remote_pid)
                except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
                    pass
                deadline=time.monotonic()+15
                while time.monotonic()<deadline:
                    probe=subprocess.run(["ssh","-T",*SSH_OPTS,"--",host,"ps -p "+remote_pid+" -o stat="],capture_output=True,text=True,timeout=10)
                    if probe.returncode!=0 or not probe.stdout.strip() or probe.stdout.strip().startswith("Z"):
                        break
                    time.sleep(.2)
                else:
                    raise RuntimeError("private remote daemon did not stop; temporary state retained at "+remote_dir)
            if re.fullmatch(r"/tmp/continuum-d4\.[A-Za-z0-9]+", remote_dir):
                remote(host, "rm -r -- " + shlex.quote(remote_dir))
    if outcome is not None:
        outcome["remote_temp_removed"]=True
        print(json.dumps(outcome))


if __name__ == "__main__":
    main()
