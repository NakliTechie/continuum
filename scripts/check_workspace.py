#!/usr/bin/env python3
"""Isolated real-shell managed workspace cycle; no provider, live service or shared home."""
import argparse
import hashlib
import json
import pathlib
import subprocess
import tempfile
import time


def run(*args, cwd=None):
    return subprocess.run(args, cwd=cwd, text=True, capture_output=True, check=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    binary = str(pathlib.Path(parser.parse_args().binary).resolve())
    with tempfile.TemporaryDirectory(prefix="continuum-workspace-") as temp:
        root = pathlib.Path(temp)
        repo, state, log = root / "repo", root / "state", root / "lifecycle.log"
        repo.mkdir()
        for command in [
            ("git", "init", "-q", "-b", "main"),
            ("git", "config", "user.email", "cycle@example.invalid"),
            ("git", "config", "user.name", "Cycle"),
            ("git", "commit", "-q", "--allow-empty", "-m", "root"),
        ]:
            run(*command, cwd=repo)
        def stamp(label):
            return "printf '%s\\n' '" + label + "' >> '" + str(log) + "'"
        spec = {
            "spec": "menagerie.fleet.v1", "name": "cycle", "repo": ".", "topology": "one",
            "workspace": {
                "isolation": "worktree",
                "materialise": {"commands": [{"run": stamp("command")}],
                                "health": [{"probe": "command", "run": "test -f '" + str(log) + "'"}],
                                "hooks": {"on_start": stamp("start"), "on_stop": stamp("stop"), "on_destroy": stamp("destroy")}},
                "teardown": {"commands": [stamp("teardown")], "keep_branch": False},
            },
            "roster": [{"role": "worker", "agent": "fake", "count": 1}],
        }
        spec_path = repo / "fleet.json"
        raw = json.dumps(spec, separators=(",", ":")).encode()
        spec_path.write_bytes(raw)
        digest = hashlib.sha256(raw).hexdigest()
        denied = subprocess.run([binary, "workspace", "run", "--state", str(state), "--spec", str(spec_path), "--name", "w1"], capture_output=True, text=True)
        if denied.returncode == 0 or log.exists():
            raise RuntimeError("untrusted managed shell executed")
        server = subprocess.Popen([binary, "serve", "--state", str(state), "--listen", "127.0.0.1:0"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 10
            while not (state / "v1.sock").exists() and time.monotonic() < deadline:
                if server.poll() is not None:
                    raise RuntimeError("isolated daemon exited before ready")
                time.sleep(.05)
            if not (state / "v1.sock").exists():
                raise RuntimeError("isolated daemon did not create its API socket")
            result = run(binary, "workspace", "run", "--state", str(state), "--spec", str(spec_path), "--name", "w1", "--trust", digest)
            if json.loads(result.stdout)["state"] != "ready":
                raise RuntimeError("trusted materialisation was not ready")
            time.sleep(11)
            status = json.loads(run(binary, "workspace", "status", "--state", str(state), "--name", "w1").stdout)
            if not status["managed_armed"] or status["state"] != "ready":
                raise RuntimeError("managed cadence did not preserve healthy state")
            run(binary, "workspace", "stop", "--state", str(state), "--name", "w1")
            run(binary, "workspace", "destroy", "--state", str(state), "--name", "w1", "--confirm", "w1")
            if log.read_text().splitlines() != ["command", "start", "stop", "teardown", "destroy"]:
                raise RuntimeError("lifecycle order differed from the declared graph")
            if (state / "workspaces" / "w1").exists():
                raise RuntimeError("verified worktree remains after destroy")
        finally:
            server.terminate()
            try:
                server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
    print(json.dumps({"class": "ok", "check": "managed_workspace_cycle", "stages": ["trust", "materialise", "cadence", "stop", "teardown"]}))


if __name__ == "__main__":
    main()
