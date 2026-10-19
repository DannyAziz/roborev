#!/bin/bash
# usage: suite5.sh <dir> <tag> [modes...]; modes: neither detected 1 0
cd "$1" || exit 1
tag=$2; shift 2
modes=${*:-neither detected 1 0}
export ROBOREV_TELEMETRY_ENABLED=0
# VM global git config signs commits via an agent that can stall; isolate tests from it.
export GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_)' | sed 's/^/-u /' | tr '\n' ' ')
for mode in $modes; do
  log=/tmp/r5-$tag-$mode.log
  case $mode in
    neither)  env -u ROBOREV_HOST_AGENT $strip go test -count=1 ./... > $log 2>&1 ;;
    detected) env -u ROBOREV_HOST_AGENT CURSOR_AGENT=1 CLOUD_AGENT_SUITE=1 go test -count=1 ./... > $log 2>&1 ;;
    *)        ROBOREV_HOST_AGENT=$mode go test -count=1 ./... > $log 2>&1 ;;
  esac
  echo EXIT=$? >> $log
done
