#!/bin/bash
export ROBOREV_TELEMETRY_ENABLED=0 GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
SUITES="c2 c3" bash /tmp/allsuites.sh
git -C /tmp/r5-basetests rev-parse --short HEAD > /tmp/r5-basetests-head2.ref
bash /tmp/basetests.sh /tmp/r5-basetests head2
cd /tmp/roborev
for c in r5-c1 r5-c2 r5-c3; do
  rm -rf /tmp/r5-bld && git worktree add -q --detach /tmp/r5-bld $c && (cd /tmp/r5-bld && go build -o /tmp/r5-bin/roborev-$c ./cmd/roborev); git worktree remove --force /tmp/r5-bld
  bash /tmp/golden_cli.sh /tmp/r5-bin/roborev-$c /tmp/r5-cli-final-$c.txt
done
touch /tmp/r5-chain.done
python3 /tmp/mutate_r5.py > /tmp/r5-mutate-final.txt 2>&1; touch /tmp/r5-mutate-final.done
cd /tmp/r5-c3 && for p in agent review daemon; do go test -c -o /tmp/r5-bin/$p.test ./internal/$p; done
for m in neither detected 1 0; do bash /tmp/flake5.sh $m; done
touch /tmp/r5-flake.done
