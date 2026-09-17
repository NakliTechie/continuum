# WAN and multi-server drill

The committed local gate (`python3 scripts/verify.py verify wan-local`) checks the
runner's input, plan and result assertions without SSH. It is **not** WAN evidence.
The real drill is opt-in and needs two distinct, already trusted SSH hosts in
addition to the local coordinator:

```sh
python3 scripts/check_wan_multi_server.py --plan
python3 scripts/check_wan_multi_server.py --run --host user@host-a --host user@host-b
```

No DNS name, key, network setting or paid server is created by the runner. SSH
uses BatchMode, strict known-host checking, disabled forwarding and no agent
forwarding. The runner probes OS/architecture first, cross-builds a temporary
Continuum binary, copies it and `fault_bridge.sh` into `/tmp/continuum-wan.*`
on each remote host, and starts foreground-style private daemons on loopback
with separate temporary state. It does not install/restart the live service or
touch its state. It verifies three distinct daemon IDs before pairing peers.

The suite asserts:

1. Three-source `any` gives the deliberately staggered remote winner and an
   unchanged retained result after coordinator SIGKILL/restart.
2. Three-source `all` stays pending through a single dropped bridge request and
   an isolated outage of one peer, then satisfies after reconnection. Every
   source has a distinct committed completion order and replay is unchanged.
3. An injected two-second bridge delay is tolerated by peer identity recheck.
4. A small number of application-level status probes per remote report median,
   maximum and failures. The default is five; `--probes N` accepts 1–20.
5. Temporary remote daemons stop before their narrow test directories are
   removed. A failed stop leaves the directory intact and reports its path.

Run this only on hosts where temporary binaries, shell blocks and state under
`/tmp` are acceptable. The shell blocks are finite `sleep`/`printf` commands;
they use no ACP provider or model credits. If a run fails, inspect any reported
retained temporary path and private `serve.log` before manual cleanup. Capture
the JSON result, host OS/architecture, test date and any failure or cleanup
error in a dated local record. Do not claim a lossy-WAN benchmark from five
status probes or the synthetic bridge faults; this measures an existing path,
not every network or Linux systemd installation.
