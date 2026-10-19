#!/bin/bash
# Runs the 00316ee7 test set, marker unset and Cursor vars stripped, on a tree.
# usage: basetests.sh <dir> <tag>
cd "$1" || exit 1
export ROBOREV_TELEMETRY_ENABLED=0
# VM global git config signs commits via an agent that can stall; isolate tests from it.
export GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_)' | sed 's/^/-u /' | tr '\n' ' ')
env -u ROBOREV_HOST_AGENT $strip go test -count=1 -v ./... > /tmp/r5-basetests-$2.log 2>&1
echo EXIT=$? >> /tmp/r5-basetests-$2.log
