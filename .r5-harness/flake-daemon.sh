#!/bin/bash
# Runs one internal/daemon test N times in one marker mode, from a checkout of
# host-backend-r5 (8964d812). Needs only go and git.
# usage: .r5-harness/flake-daemon.sh <TestName> <mode> [count]
#   mode: neither | detected | 1 | 0     count defaults to 3000
# Writes .r5-harness/out/<mode>-<TestName>.log and prints one summary line.
set -u
t=$1 mode=$2 count=${3:-3000}
root=$(git rev-parse --show-toplevel) || exit 1
here=$root/.r5-harness
mkdir -p "$here/out" "$here/bin"
export ROBOREV_TELEMETRY_ENABLED=0
# Isolate tests from any global git config that signs commits.
export GIT_CONFIG_GLOBAL=$here/r5-gitconfig
bin=$here/bin/daemon.test
[ -x "$bin" ] || (cd "$root" && go test -c -o "$bin" ./internal/daemon) || exit 1
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_)' | sed 's/^/-u /' | tr '\n' ' ')
case $mode in
  neither)  pre="env -u ROBOREV_HOST_AGENT $strip" ;;
  detected) pre="env -u ROBOREV_HOST_AGENT CURSOR_AGENT=1 CLOUD_AGENT_FLAKE=1" ;;
  1|0)      pre="env ROBOREV_HOST_AGENT=$mode" ;;
  *) echo "unknown mode $mode" >&2; exit 2 ;;
esac
log=$here/out/$mode-$t.log
(cd "$root/internal/daemon" && $pre "$bin" -test.run "^$t\$" -test.count="$count" -test.v -test.timeout 300m > "$log" 2>&1); rc=$?
echo "daemon $t mode=$mode runs=$count pass=$(grep -c "^--- PASS: $t " "$log") fail=$(grep -c "^--- FAIL: $t " "$log") exit=$rc"
