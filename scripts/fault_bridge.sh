#!/bin/sh
# Private D4 test fixture. Keep beside a temporary Continuum binary on the
# remote test host. Marker files simulate SSH bridge loss/latency without
# changing any host networking or touching an installed daemon.
set -eu
fixture_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -e "$fixture_dir/offline" ]; then
  exit 75
fi
if [ -e "$fixture_dir/drop_once" ] && mv "$fixture_dir/drop_once" "$fixture_dir/dropped" 2>/dev/null; then
  exit 75
fi
if [ -e "$fixture_dir/delay" ]; then
  sleep 2
fi
exec "$fixture_dir/continuum" "$@"
