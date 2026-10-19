#!/bin/bash
# usage: flake5.sh <mode>; modes: neither detected 1 0. Runs on /tmp/r5-c3 (host-r5 tip).
export ROBOREV_TELEMETRY_ENABLED=0
# VM global git config signs commits via an agent that can stall; isolate tests from it.
export GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
mode=$1
root=/tmp/r5-c3
bin=/tmp/r5-bin
out=/tmp/r5-flake-$mode.txt; : > $out
echo "HEAD=$(git -C $root rev-parse --short HEAD) mode=$mode" >> $out
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_)' | sed 's/^/-u /' | tr '\n' ' ')
case $mode in
  neither)  pre="env -u ROBOREV_HOST_AGENT $strip" ;;
  detected) pre="env -u ROBOREV_HOST_AGENT CURSOR_AGENT=1 CLOUD_AGENT_FLAKE=1" ;;
  *)        pre="env ROBOREV_HOST_AGENT=$mode" ;;
esac
for spec in \
 agent:TestHostAgentRoundTrip agent:TestHostAgentPlainReviewPassesRepoPath \
 agent:TestHostHandoffDirsAreUniqueAndContained agent:TestHostModelHint \
 agent:TestHandoffFilesArePublishedAtomically agent:TestHostAgentRefusesPlantedHandoffFile \
 agent:TestHostAnswerReplacedBeforeOpenIsRefused agent:TestHostAnswerFIFOIsRefusedWithoutBlocking \
 agent:TestHostAnswerErrorFailsBeforeDeadline agent:TestHostAnswerErrorAtDeadlineIsReported \
 agent:TestHostAgentCancel agent:TestHostIsLastResortOnlyWithExplicitMarker agent:TestBackupHostWithMarkerIsUsed \
 agent:TestResolveAutoBackup agent:TestReadHostMarker agent:TestHostMarkerAllows \
 review:TestRunAgentReviewHostRoundTrip review:TestRunAgentReviewHostTimeoutIsNonRetryable \
 daemon:TestHostRulingByMarkerMode daemon:TestHostFailoverByMarkerMode daemon:TestEmptyAutoPinIsFrozen \
 daemon:TestAutoPinnedHostIsRecheckedOnRestart daemon:TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry \
 daemon:TestSynthesisHostTimeoutSkipsRetries daemon:TestSynthesisNonHostErrorIsStillRetried daemon:TestWorkerNonHostGetAgentErrorIsStillRetried ; do
  pkg=${spec%%:*}; t=${spec##*:}
  log=/tmp/r5-flake-$mode-$t.log
  (cd $root/internal/$pkg && $pre $bin/$pkg.test -test.run "^$t\$" -test.count=3000 -test.v -test.timeout 120m > $log 2>&1); rc=$?
  echo "$pkg $t runs=3000 pass=$(grep -c "^--- PASS: $t " $log) fail=$(grep -c "^--- FAIL: $t " $log) exit=$rc" >> $out
done
echo DONE >> $out
