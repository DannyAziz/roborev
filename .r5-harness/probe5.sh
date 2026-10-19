#!/bin/bash
# usage: probe5.sh <name> <daemon-mode> <default_agent> <default_backup_agent> <shell-mode> <action>
# modes: neither detected 1 0; action: enqueue | doctor | check-agents | check-agents-host
export ROBOREV_TELEMETRY_ENABLED=0
name=$1 dmode=$2 dagent=$3 dbackup=$4 smode=$5 action=$6
bin=/tmp/r5-bin/roborev
out=/tmp/r5-probe-$name.txt
work=$(mktemp -d /tmp/r5-probe.XXXXXX)
data=$work/data hostdir=$work/hostdir repo=$work/repo
mkdir -p $data $repo
cat > $data/config.toml <<EOF
server_addr = "127.0.0.1:7499"
default_agent = "$dagent"
default_backup_agent = "$dbackup"
EOF
git -C $repo init -q && git -C $repo -c user.name=p -c user.email=p@x commit -q --allow-empty -m init
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_)' | sed 's/^/-u /' | tr '\n' ' ')
modeenv() {
  case $1 in
    neither)  echo "-u ROBOREV_HOST_AGENT $strip" ;;
    detected) echo "-u ROBOREV_HOST_AGENT CURSOR_AGENT=1 CLOUD_AGENT_PROBE=1" ;;
    *)        echo "$strip ROBOREV_HOST_AGENT=$1" ;;
  esac
}
stubdir=$work/stubs; mkdir -p $stubdir
for c in ${PROBE_STUBS:-}; do printf '#!/bin/sh\necho "%s: simulated failure" >&2\nexit 1\n' $c > $stubdir/$c; chmod +x $stubdir/$c; done
base="ROBOREV_DATA_DIR=$data ROBOREV_HOST_DIR=$hostdir PATH=$stubdir:/usr/bin:/bin HOME=$work ROBOREV_HOST_TIMEOUT=3s"
{
  echo "# daemon mode=$dmode shell mode=$smode default_agent=\"$dagent\" default_backup_agent=\"$dbackup\" binary $(git -C /tmp/r5-c3 rev-parse --short HEAD)"
  echo "# stub CLIs (always fail): [${PROBE_STUBS:-}]"
  echo "# agent CLIs on PATH: [$(for c in codex claude gemini copilot cursor-agent opencode droid pi kilo kiro; do command -v $c >/dev/null 2>&1 && echo -n "$c "; done)]"
} > $out
env $(modeenv $dmode) $base $bin daemon run > $work/daemon.log 2>&1 &
dpid=$!
for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:7499/api/status && break; sleep 0.2; done
case $action in
  enqueue)
    curl -s -w '\nHTTP=%{http_code}\n' -X POST http://127.0.0.1:7499/api/enqueue \
      -H 'Content-Type: application/json' -d "{\"repo_path\":\"$repo\",\"git_ref\":\"HEAD\"}" >> $out
    sleep 1
    echo "# jobs:" >> $out
    curl -s http://127.0.0.1:7499/api/jobs >> $out; echo >> $out ;;
  failover)
    curl -s -w '\nHTTP=%{http_code}\n' -X POST http://127.0.0.1:7499/api/enqueue \
      -H 'Content-Type: application/json' -d "{\"repo_path\":\"$repo\",\"git_ref\":\"HEAD\"}" >> $out
    for i in $(seq 1 120); do
      j=$(curl -s http://127.0.0.1:7499/api/jobs)
      echo "$j" | grep -q '"agent":"host"\|"status":"failed"' && break; sleep 0.5
    done
    sleep 1
    echo "# jobs after failover:" >> $out
    curl -s http://127.0.0.1:7499/api/jobs | python3 -c 'import json,sys; d=json.load(sys.stdin); [print({k:j.get(k) for k in ("agent","status","retry_count","error")}) for j in d.get("jobs",[])]' >> $out ;;
  doctor)
    (cd $repo && env $(modeenv $smode) $base $bin doctor --server http://127.0.0.1:7499) >> $out 2>&1; echo "exit=$?" >> $out ;;
  check-agents)
    (cd $repo && env $(modeenv $smode) $base $bin check-agents) >> $out 2>&1; echo "exit=$?" >> $out ;;
  check-agents-host)
    (cd $repo && env $(modeenv $smode) $base $bin check-agents --agent host) >> $out 2>&1; echo "exit=$?" >> $out ;;
esac
echo "# host handoff dir: $(ls -A $hostdir 2>&1 | sed "s|$work|<work>|g")" >> $out
kill $dpid 2>/dev/null; wait $dpid 2>/dev/null
echo "# daemon stopped: $(kill -0 $dpid 2>/dev/null && echo no || echo yes)" >> $out
sed -i "s|$work|<work>|g" $out
rm -rf $work
cat $out
