#!/bin/bash
export ROBOREV_TELEMETRY_ENABLED=0 GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
while [ ! -f /tmp/r5-mutate-final.done ]; do sleep 10; done
tmux -f /exec-daemon/tmux.portal.conf kill-session -t r5-chain2 2>/dev/null
sleep 2
cd /tmp/r5-c3 && for p in agent review daemon; do go test -c -o /tmp/r5-bin/$p.test ./internal/$p || exit 1; done
git -C /tmp/r5-c3 rev-parse --short HEAD > /tmp/r5-flake-bin.ref
for m in neither detected 1 0; do bash /tmp/flake5.sh $m & done
wait
touch /tmp/r5-flake.done
