#!/bin/bash
# usage: golden_cli.sh <binary> <out>
# Runs user-facing commands with the marker unset and Cursor vars stripped,
# for several existing configs, and writes normalized output to <out>.
export ROBOREV_TELEMETRY_ENABLED=0
bin=$1 out=$2
strip=$(env | cut -d= -f1 | grep -E '^(CURSOR_AGENT|CLOUD_AGENT_|ROBOREV_)' | sed 's/^/-u /' | tr '\n' ' ')
: > $out
stubs=$(mktemp -d /tmp/r5-gstubs.XXXXXX)
for c in codex claude; do printf '#!/bin/sh\nexit 1\n' > $stubs/$c; chmod +x $stubs/$c; done
run_case() {
  local name=$1 cfg=$2 path=$3
  local work; work=$(mktemp -d /tmp/r5-golden.XXXXXX)
  local data=$work/data repo=$work/repo
  mkdir -p $data $repo
  printf 'server_addr = "127.0.0.1:7498"\n%b' "$cfg" > $data/config.toml
  git -C $repo init -q && git -C $repo -c user.name=p -c user.email=p@x commit -q --allow-empty -m init
  local e="env $strip ROBOREV_DATA_DIR=$data PATH=$path HOME=$work"
  $e $bin daemon run > $work/daemon.log 2>&1 &
  local dpid=$!
  for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:7498/api/status && break; sleep 0.2; done
  {
    echo "===== case: $name"
    echo "--- doctor"; (cd $repo && $e $bin doctor --server http://127.0.0.1:7498 2>&1); echo "exit=$?"
    echo "--- check-agents"; (cd $repo && $e $bin check-agents --timeout 2 2>&1); echo "exit=$?"
    echo "--- check-agents --agent bogus"; (cd $repo && $e $bin check-agents --agent bogus 2>&1); echo "exit=$?"
    echo "--- completion --agent"; (cd $repo && $e $bin __complete review --agent "" 2>&1 | sort); echo "exit=$?"
    echo "--- enqueue"; curl -s -w '\nHTTP=%{http_code}\n' -X POST http://127.0.0.1:7498/api/enqueue \
      -H 'Content-Type: application/json' -d "{\"repo_path\":\"$repo\",\"git_ref\":\"HEAD\"}"
    sleep 0.5
    echo "--- review --agent bogus"; (cd $repo && $e $bin review --agent bogus --server http://127.0.0.1:7498 2>&1); echo "exit=$?"
  } >> $out 2>&1
  kill $dpid 2>/dev/null; wait $dpid 2>/dev/null
  sed -i -e "s|$work|<work>|g" -e "s|$stubs|<stubs>|g" $out
  rm -rf $work
}
run_case "default config, no agents" "" /usr/bin:/bin
run_case "default config, codex+claude" "" $stubs:/usr/bin:/bin
run_case "named agent claude" 'default_agent = "claude"\n' $stubs:/usr/bin:/bin
run_case "backup agent" 'default_agent = "codex"\ndefault_backup_agent = "claude"\ndefault_backup_model = "sonnet"\n' $stubs:/usr/bin:/bin
run_case "backup agent, nothing installed" 'default_agent = "codex"\ndefault_backup_agent = "claude"\n' /usr/bin:/bin
run_case "fix pin" 'fix_agent = "codex"\nfix_backup_agent = "claude"\n' $stubs:/usr/bin:/bin
run_case "unknown agent" 'default_agent = "bogus"\n' $stubs:/usr/bin:/bin
run_case "broken config" 'default_agent = [broken\n' $stubs:/usr/bin:/bin
rm -rf $stubs
# timings, ids, timestamps, versions and ports vary run to run
sed -i -E -e 's/[0-9]+(\.[0-9]+)?(ms|s)\b/<dur>/g' -e 's/"(id|job_id)":[0-9]+/"\1":<n>/g' \
  -e 's/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+(Z|[+-][0-9:]+)/<ts>/g' -e 's/pid [0-9]+/pid <n>/g' \
  -e 's/"uuid":"[^"]*"/"uuid":"<u>"/g' -e 's/r5-g[a-z]*\.[A-Za-z0-9]+/<tmp>/g' $out
