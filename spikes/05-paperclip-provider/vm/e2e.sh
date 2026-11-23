#!/usr/bin/env bash
# Spike 05 end-to-end scenarios against Paperclip with the wsm provider plugin.
# Usage (as root, through run.sh): bash e2e.sh file|hostile-1|hostile-2|duplex
#
# One company, one wsm environment and one claude_local agent are shared by all
# scenarios (state in /root/s05/e2e.env). Each scenario gets its own Paperclip
# project and host repository, except hostile-2, which reuses hostile-1's.
# Raw run logs stay in /root/s05/raw and never go into results/.
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

sc=${1:-}
case $sc in
file | hostile-1 | hostile-2 | duplex) ;;
*)
	echo "usage: e2e.sh file|hostile-1|hostile-2|duplex" >&2
	exit 2
	;;
esac

OUT=$RESULTS/e2e-$sc
RAW=$STATE/raw/$sc
rm -rf "$OUT" "$RAW"
mkdir -p "$OUT" "$RAW"
chmod 0700 "$STATE" "$STATE/raw"
HOST_ADDR=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')
SENTINEL_FILE=$STATE/sentinel
if [[ ! -s $SENTINEL_FILE ]]; then
	(umask 077 && head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32 >"$SENTINEL_FILE")
fi
CANARY_SHA=$(sha256sum <"$CANARY" | cut -c1-64)
ACTIVE='queued|running|starting|preparing|finalizing|scheduled_retry'

# call <METHOD> <path> [json-file]: body to $RAW/resp.json, prints the status.
call() {
	local m=$1 p=$2 f=${3:-}
	if [[ -n $f ]]; then
		curl -sS -o "$RAW/resp.json" -w '%{http_code}' -X "$m" "$API$p" -H 'content-type: application/json' --data-binary "@$f"
	else
		curl -sS -o "$RAW/resp.json" -w '%{http_code}' -X "$m" "$API$p"
	fi
}
# jcall <METHOD> <path> <json>: like call, with an inline JSON body (no secrets).
jcall() {
	printf '%s' "$3" >"$RAW/req.json"
	call "$1" "$2" "$RAW/req.json"
}
runs_json() {
	curl -sS "$API/companies/$CID/heartbeat-runs?agentId=$AID" |
		jq -c '(if type=="array" then . else (.items // .runs // .data // []) end)'
}

# res_sticky <ID> PASS|FAIL <detail>: a FAIL from an earlier scenario stays.
res_sticky() {
	local id=$1 status=$2 detail=$3 prev
	prev=$(grep -P "^${id}\t" "$RESULTS/summary.txt" | head -1)
	if [[ $prev == *$'\tFAIL\t'* && $status == PASS ]]; then
		res "$id" FAIL "${prev#*$'\tFAIL\t'}; $sc: $detail"
	elif [[ $prev == *$'\tPASS\t'* && $status == PASS ]]; then
		res "$id" PASS "${prev#*$'\tPASS\t'}; $sc: $detail"
	else
		res "$id" "$status" "$sc: $detail"
	fi
}

ensure_agent() {
	CID= EID= AID=
	if [[ -f $STATE/e2e.env ]]; then
		# shellcheck disable=SC1091
		source "$STATE/e2e.env"
		[[ -n $CID && -n $EID && -n $AID ]] && curl -fsS -o /dev/null "$API/agents/$AID" || CID=
	fi
	[[ -n $CID ]] && return 0
	log "create company, environment and agent"
	local code envname i st last=""
	code=$(jcall POST /companies '{"name":"S05 spike"}')
	CID=$(jq -r .id "$RAW/resp.json")
	info e2e-company "POST /companies -> $code"
	envname=wsm-$(date +%s)
	code=$(jcall POST "/companies/$CID/environments" "{\"name\":\"$envname\",\"driver\":\"sandbox\",\"config\":{\"provider\":\"wsm\",\"project\":\"$PROJECT\"}}")
	EID=$(jq -r '.id // empty' "$RAW/resp.json")
	info e2e-environment "POST environments -> $code name=$envname $(jq -c '{status, error}' "$RAW/resp.json" 2>/dev/null | head -c 200)"
	# The agent JSON holds the sentinel, so it goes through a 0600 file, never argv.
	(umask 077 && jq -n --arg env "$EID" --rawfile s "$SENTINEL_FILE" '{name:"s05-mock-claude", adapterType:"claude_local", defaultEnvironmentId:$env,
		adapterConfig:{command:"claude", engine:"cli", dangerouslySkipPermissions:true, timeoutSec:600, maxTurnsPerRun:10,
		env:{ANTHROPIC_BASE_URL:"http://127.0.0.1:8787/anthropic", ANTHROPIC_API_KEY:"mock-key", DISABLE_TELEMETRY:"1",
			DISABLE_ERROR_REPORTING:"1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:"1", DISABLE_AUTOUPDATER:"1",
			S05_SENTINEL:($s|rtrimstr("\n"))}}}' >"$STATE/agent.json")
	code=$(call POST "/companies/$CID/agents" "$STATE/agent.json")
	AID=$(jq -r '.id // empty' "$RAW/resp.json")
	info e2e-agent "POST agents -> $code status=$(jq -r .status "$RAW/resp.json")"
	if [[ -z $CID || -z $EID || -z $AID ]]; then
		echo "e2e: could not create the company, environment or agent; see the INFO lines" >&2
		rm -f "$STATE/e2e.env"
		exit 1
	fi
	printf 'CID=%s\nEID=%s\nAID=%s\n' "$CID" "$EID" "$AID" >"$STATE/e2e.env"
	# A new agent passes a lifecycle check (the hello probe in its environment).
	local t0
	t0=$(date +%s)
	for i in $(seq 1 90); do
		st=$(curl -sS "$API/agents/$AID" | jq -r '[.status, (.lifecycleOperation.phase // "none")] | join("/")')
		[[ $st != "$last" ]] && {
			log "agent $st after $(($(date +%s) - t0))s"
			last=$st
		}
		[[ $st == */none ]] && break
		sleep 2
	done
	info agent-lifecycle "$last after $(($(date +%s) - t0))s"
}

setup_repo() {
	REPO=/home/pclip/repo-$sc
	[[ $sc == hostile-2 ]] && REPO=/home/pclip/repo-hostile-1
	if [[ $sc != hostile-2 ]]; then
		rm -rf "$REPO"
		as_pc git init -q -b main "$REPO"
		mkdir -p "$REPO/.s05"
		install -m 0644 "$VM_DIR/scenario.sh" "$REPO/.s05/run.sh"
		echo "$HOST_ADDR" >"$REPO/.s05/host"
		printf '# s05 %s\n' "$sc" >"$REPO/README.md"
		chown -R pclip:pclip "$REPO"
		as_pc git -C "$REPO" add README.md .s05/run.sh .s05/host
		as_pc git -C "$REPO" -c user.name=owner -c user.email=owner@example.invalid commit -q -m "Initial commit"
		local code
		code=$(jcall POST "/companies/$CID/projects" "{\"name\":\"s05-$sc-$(date +%s)\",\"status\":\"in_progress\",\"workspace\":{\"name\":\"repo-$sc\",\"cwd\":\"$REPO\",\"isPrimary\":true}}")
		PID_=$(jq -r '.id // empty' "$RAW/resp.json")
		echo "$PID_" >"$STATE/project-$sc"
		info "project-$sc" "POST projects -> $code"
	else
		PID_=$(cat "$STATE/project-hostile-1")
	fi
	HEAD0=$(as_pc git -C "$REPO" rev-parse HEAD)
	FIRST=$(as_pc git -C "$REPO" rev-list --max-parents=0 HEAD | tail -1)
}

start_watch() {
	node "$VM_DIR/watch.mjs" "$SENTINEL_FILE" "$OUT/watcher.txt" &
	WATCH_PID=$!
	start_listener
}

# start_listener runs a control listener on 0.0.0.0:3199 as pclip.
start_listener() {
	systemctl stop s05-listen 2>/dev/null
	systemctl reset-failed s05-listen 2>/dev/null
	systemd-run --unit=s05-listen --uid=pclip --gid=pclip /usr/local/bin/node -e \
		"require('http').createServer((q,s)=>s.end('host-ok')).listen(3199,'0.0.0.0')" >/dev/null
}

stop_watch() {
	kill -TERM "$WATCH_PID" 2>/dev/null
	wait "$WATCH_PID" 2>/dev/null
	systemctl stop s05-listen 2>/dev/null
}

run_issue() {
	local before code t0 i r last="" stable=0 dup
	dup=false
	[[ $sc == duplex ]] && dup=true
	code=$(jcall PATCH /instance/settings/experimental "{\"enableSandboxDuplexBridge\": $dup}")
	info "duplex-setting-$sc" "enableSandboxDuplexBridge=$dup -> $code"
	before=$(runs_json | jq -r '[.[].id] | join(" ")')
	T_START=$(date +%s)
	start_watch
	code=$(jcall POST "/companies/$CID/issues" "$(jq -nc --arg p "$PID_" --arg a "$AID" --arg t "[s05-$sc] S1 scenario $sc" \
		'{title:$t, description:"Run the scenario script .s05/run.sh.", status:"todo", projectId:$p, assigneeAgentId:$a}')")
	IID=$(jq -r '.id // empty' "$RAW/resp.json")
	info "issue-$sc" "POST issues -> $code"
	t0=$(date +%s)
	for i in $(seq 1 120); do
		runs_json | jq -c --arg before "$before" '[.[] | select(.id as $id | ($before | split(" ") | index($id)) | not)]' >"$RAW/runs.json"
		r=$(jq -r 'map(.status) | join(",")' "$RAW/runs.json")
		[[ $r != "$last" ]] && {
			log "runs [$r] after $(($(date +%s) - t0))s"
			last=$r
		}
		if [[ -n $r ]] && ! grep -qE "$ACTIVE" <<<"$r"; then
			stable=$((stable + 1))
			((stable >= 3)) && break
		else
			stable=0
		fi
		sleep 5
	done
	T_END=$(date +%s)
	stop_watch
	ISSUE_STATUS=$(curl -sS "$API/issues/$IID" | jq -r .status)
	if [[ $ISSUE_STATUS != done ]]; then
		code=$(jcall PATCH "/issues/$IID" '{"status":"cancelled"}')
		info "issue-$sc-cancel" "status was $ISSUE_STATUS; PATCH cancelled -> $code"
	fi
	jq -r 'sort_by(.createdAt // .startedAt // "") | .[] | [.id, .status, (.invocationSource // ""), (.startedAt // ""), (.finishedAt // ""), ((.error // "") | tostring | .[0:240])] | @tsv' \
		"$RAW/runs.json" >"$OUT/runs.tsv"
	for rid in $(jq -r 'sort_by(.createdAt // .startedAt // "") | .[].id' "$RAW/runs.json"); do
		curl -sS "$API/heartbeat-runs/$rid/log?offset=0&limitBytes=8000000" >"$RAW/log-$rid.json"
	done
	node "$VM_DIR/runlog.mjs" "$RAW"/log-*.json >"$OUT/runlog.txt" 2>/dev/null
	journalctl -u s05-paperclip --since "@$T_START" --no-pager -o cat | grep -F '[plugin]' | grep -F 's05 ' | cut -c1-400 >"$OUT/plugin-log.txt"
	info "time-$sc" "issue to quiet: $((T_END - T_START))s; runs: $(awk -F'\t' '{printf "%s/%s ", $2, $3}' "$OUT/runs.tsv")"
}

kv() { grep -m1 "^kv $1=" "$OUT/runlog.txt" | cut -d= -f2-; }

# net_probe runs the probes from the preflight in a fresh sandbox, outside Paperclip.
net_probe() {
	local slug="np-$(head -c 5 /dev/urandom | od -An -tx1 | tr -d ' \n')" puser pod ctr ctl code
	puser=$(project_user)
	wsmapi "/projects/$PROJECT/sandboxes" -o "$RAW/np.json" -H 'content-type: application/json' -d "{\"slug\":\"$slug\"}" >/dev/null
	wsmapi "/projects/$PROJECT/sandboxes/$slug/start" -X POST >/dev/null
	pod=$(jq -r .pod "$RAW/np.json")
	ctr=$pod-work
	start_listener
	ctl=$(curl -sS -m 3 -o /dev/null -w '%{http_code}' "http://$HOST_ADDR:3199/")
	as_pc podman --remote --url "unix:///run/wsm-podman/$puser/podman.sock" exec --user 1000:1000 "$ctr" sh -c '
		for u in "http://$1:3199/" "http://$1:3100/api/health" http://169.254.1.2:3199/ http://169.254.1.2:3100/api/health \
			http://169.254.1.2:54329/ http://169.254.1.2:8787/ http://host.containers.internal:3199/ http://127.0.0.1:3199/; do
			code=$(curl -sS -m 5 -o /dev/null -w "%{http_code}" "$u" 2>/dev/null); echo "$u $code rc=$?"
		done' sh "$HOST_ADDR" >"$OUT/net-probe.txt" 2>&1
	systemctl stop s05-listen 2>/dev/null
	wsmapi "/projects/$PROJECT/sandboxes/$slug" -X DELETE >/dev/null
	echo "control-from-host http://$HOST_ADDR:3199/ $ctl" >>"$OUT/net-probe.txt"
	NP_CTL=$ctl
	NP_OK=true
	grep -vE '^control' "$OUT/net-probe.txt" | grep -qvE ' 000 rc=(7|28)$' && NP_OK=false
	[[ $(grep -c ' rc=' "$OUT/net-probe.txt") -ge 8 ]] || NP_OK=false
}

check_net() {
	local bad
	bad=$(grep -E '^kv net-[^=]*-rc=' "$OUT/runlog.txt" | grep -vE '=(7|28)$' | cut -c4- | tr '\n' ' ')
	bad+=$(grep -E '^kv net-' "$OUT/runlog.txt" | grep -vE -- '-rc=' | grep -vE '=000$' | cut -c4- | tr '\n' ' ')
	local n
	n=$(grep -cE '^kv net-[^=]*-rc=' "$OUT/runlog.txt")
	net_probe
	if [[ -z $bad ]] && ((n >= 6)) && [[ $(kv bridge-me) == 200 ]] && $NP_OK && [[ $NP_CTL == 200 ]]; then
		res_sticky NET-ISOLATION PASS "$n net probes 000 (refused/timeout) with bridge-me=200; separate probe all refused, host control=$NP_CTL"
	else
		res_sticky NET-ISOLATION FAIL "bad=[$bad] n=$n bridge-me=$(kv bridge-me) separate-ok=$NP_OK host-control=$NP_CTL"
	fi
}

check_secrets() {
	local hits sentinel_in_plugin=0 sentinel_in_results=0
	hits=$(grep -oP '^hits=\K[0-9]+' "$OUT/watcher.txt" 2>/dev/null || echo "?")
	if [[ $hits == 0 ]]; then
		res_sticky SECRETS-ARGV PASS "watcher $(grep -oP '^scans=\K[0-9]+' "$OUT/watcher.txt") scans, 0 hits"
	else
		res_sticky SECRETS-ARGV FAIL "watcher hits=$hits: $(grep '^hit ' "$OUT/watcher.txt" | tr '\n' ' ')"
	fi
	journalctl -u s05-paperclip --no-pager -o cat | grep -F '[plugin]' | grep -qF -f "$SENTINEL_FILE" && sentinel_in_plugin=1
	grep -rqF -f "$SENTINEL_FILE" "$RESULTS" && sentinel_in_results=1
	if ((sentinel_in_plugin == 0 && sentinel_in_results == 0)); then
		res_sticky SECRETS-LOG PASS "sentinel not in plugin log lines or results/"
	else
		res_sticky SECRETS-LOG FAIL "sentinel in plugin log=$sentinel_in_plugin results=$sentinel_in_results"
		# Keep the value itself out of the download: drop the lines that hold it.
		local f
		grep -rlF -f "$SENTINEL_FILE" "$RESULTS" | while IFS= read -r f; do
			grep -vF -f "$SENTINEL_FILE" "$f" >"$f.tmp"
			mv "$f.tmp" "$f"
		done
	fi
	local j=0 r=0
	journalctl -u s05-paperclip --no-pager -o cat | grep -qF -f "$SENTINEL_FILE" && j=1
	grep -rqF -f "$SENTINEL_FILE" "$STATE/raw" --include='log-*.json' && r=1
	info "sentinel-elsewhere-$sc" "in s05-paperclip journal=$j in raw run logs=$r"
}

check_lease_cycle() {
	local i left acq rel missing=""
	for i in $(seq 1 24); do
		left=$(wsmapi "/sandboxes?project=$PROJECT" | jq -r '[.[] | select(.slug | startswith("pc-")) | .slug] | join(" ")')
		[[ -z $left ]] && break
		sleep 5
	done
	journalctl -u s05-paperclip --no-pager -o cat | grep -F '[plugin]' >"$RAW/plugin-all.txt"
	acq=$(grep -oP 's05 lease acquire sandbox=\Kpc-[0-9a-f]{12}' "$RAW/plugin-all.txt" | sort -u)
	rel=$(grep -oP 's05 lease (release|destroy) sandbox=\Kpc-[0-9a-f]{12}' "$RAW/plugin-all.txt" | sort -u)
	for s in $acq; do grep -qx "$s" <<<"$rel" || missing+="$s "; done
	local n
	n=$(wc -w <<<"$acq")
	if [[ -z $missing && -z $left ]] && ((n > 0)); then
		res LEASE-CYCLE PASS "$n acquires, each with a release/destroy; no pc- sandbox left (after $sc)"
	else
		res LEASE-CYCLE FAIL "acquires=$n without release: [$missing] left in API: [$left] (after $sc)"
	fi
	grep -E 's05 lease (acquire|release|destroy) ' "$RAW/plugin-all.txt" | grep -oE 's05 lease [a-z]+ .*ms=[0-9]+|s05 lease [a-z]+ sandbox=[^ ]+ project=[^ ]+ result=[a-z]+' >"$OUT/lease-lines.txt"
}

run_ok() { jq -e 'any(.[]; .status == "succeeded")' "$RAW/runs.json" >/dev/null; }

check_file() {
	local me get patch deny bad gh duplex_opens glog
	me=$(kv bridge-me) get=$(kv issue-get) patch=$(kv issue-patch) deny=$(kv bridge-deny) bad=$(kv bridge-badtoken) gh=$(kv bridge-github)
	duplex_opens=$(grep -c 's05 duplex open' "$OUT/plugin-log.txt")
	if [[ $(kv ws-head) == "$FIRST" && -n $FIRST ]]; then
		res SYNC-IN PASS "ws-head=$(kv ws-head) = first commit"
	else
		res SYNC-IN FAIL "ws-head=$(kv ws-head) first=$FIRST"
	fi
	if [[ $me == 200 && $get == 200 && $patch == 200 && $duplex_opens == 0 ]]; then
		res BRIDGE-FILE PASS "bridge-me=$me issue-get=$get issue-patch=$patch duplex-opens=0"
	else
		res BRIDGE-FILE FAIL "bridge-me=$me issue-get=$get issue-patch=$patch duplex-opens=$duplex_opens"
	fi
	if [[ $deny =~ ^(403|404)$ && $bad =~ ^(401|403)$ ]]; then
		res BRIDGE-DENY PASS "bridge-deny=$deny bridge-badtoken=$bad"
	else
		res BRIDGE-DENY FAIL "bridge-deny=$deny bridge-badtoken=$bad"
	fi
	info bridge-github "POST /runtime-tools/github/credentials -> $gh"
	info env-names "$(kv env-names)"
	info env-multiline "$(kv env-multiline)"
	info api-url-host "$(kv api-url-host)"
	glog=$(as_pc git -C "$REPO" log --format=%s -5 | tr '\n' '|')
	if run_ok && [[ $ISSUE_STATUS == done ]] && grep -q "S1 spike commit from the sandbox" <<<"$glog"; then
		res RESTORE-COMMIT PASS "run succeeded, issue done, host git log: $glog"
	else
		res RESTORE-COMMIT FAIL "runs=$(cut -f2 "$OUT/runs.tsv" | tr '\n' ',') issue=$ISSUE_STATUS git log: $glog"
	fi
	local sin sout
	sin=$(grep -oP 's05 sync in .*files=\K[0-9]+' "$OUT/plugin-log.txt" | sort -n | tail -1)
	sout=$(grep -oP 's05 sync out .*files=\K[0-9]+' "$OUT/plugin-log.txt" | sort -n | tail -1)
	if [[ ${sin:-0} -gt 0 && ${sout:-0} -gt 0 ]] && grep -q $'^RESTORE-COMMIT\tPASS' "$RESULTS/summary.txt"; then
		res SYNC-NATIVE PASS "max files per sync in=$sin out=$sout; RESTORE-COMMIT PASS"
	else
		res SYNC-NATIVE FAIL "sync in files=${sin:-none} out=${sout:-none} (no native sync lines means the base64 fallback ran)"
	fi
	check_net
}

check_duplex() {
	local me get patch opens glog
	me=$(kv bridge-me) get=$(kv issue-get) patch=$(kv issue-patch)
	opens=$(grep -c 's05 duplex open' "$OUT/plugin-log.txt")
	if ((opens > 0)) && [[ $me == 200 && $get == 200 && $patch == 200 ]]; then
		res BRIDGE-DUPLEX PASS "duplex-opens=$opens bridge-me=$me issue-get=$get issue-patch=$patch"
	else
		res BRIDGE-DUPLEX FAIL "duplex-opens=$opens bridge-me=$me issue-get=$get issue-patch=$patch"
	fi
	glog=$(as_pc git -C "$REPO" log --format=%s -5 | tr '\n' '|')
	if run_ok && [[ $ISSUE_STATUS == done ]] && grep -q "S1 spike commit from the sandbox (duplex)" <<<"$glog"; then
		res RESTORE-DUPLEX PASS "run succeeded, issue done, host git log: $glog"
	else
		res RESTORE-DUPLEX FAIL "runs=$(cut -f2 "$OUT/runs.tsv" | tr '\n' ',') issue=$ISSUE_STATUS git log: $glog"
	fi
	check_net
	code=$(jcall PATCH /instance/settings/experimental '{"enableSandboxDuplexBridge": false}')
	info duplex-setting-reset "enableSandboxDuplexBridge=false -> $code"
}

check_hostile_1() {
	local escapes="" leak_info="" l target real
	while IFS= read -r l; do
		target=$(readlink "$l")
		real=$(readlink -m "$l")
		if [[ $real != "$REPO" && $real != "$REPO"/* ]]; then
			if [[ $l == "$REPO/leak" ]]; then leak_info="leak -> $target"; else escapes+="${l#"$REPO"/} -> $target; "; fi
		fi
	done < <(find "$REPO" -path "$REPO/.git" -prune -o -type l -print)
	info leak-via-git "${leak_info:-leak not on the host}"
	local refusal
	refusal=$( (grep -hoE 's05 archive refused[^"\\]{0,160}' "$RAW"/log-*.json "$OUT/plugin-log.txt"; cut -f6 "$OUT/runs.tsv") 2>/dev/null | grep -v '^$' | sort -u | head -3 | tr '\n' ' ')
	info restore-refusal "${refusal:-none}"
	if [[ -z $escapes ]]; then
		res ARCHIVE-E2E PASS "no uncommitted escaping symlink on the host; restore error: ${refusal:-none}"
	else
		res ARCHIVE-E2E FAIL "escaping symlinks on the host: $escapes restore error: ${refusal:-none}"
	fi

	local hooks cfg before after
	hooks=$(find "$REPO/.git/hooks" -type f ! -name '*.sample' 2>/dev/null | tr '\n' ' ')
	cfg=$(grep -iE 'fsmonitor|hookspath' "$REPO/.git/config" | tr '\n' ' ')
	before=$(ls /tmp/s05-hook-* /tmp/s05-fsmonitor 2>/dev/null | tr '\n' ' ')
	as_pc git -C "$REPO" status --short >"$OUT/host-git-status.txt" 2>&1
	as_pc git -C "$REPO" log --oneline -5 >"$OUT/host-git-log.txt" 2>&1
	after=$(ls /tmp/s05-hook-* /tmp/s05-fsmonitor 2>/dev/null | tr '\n' ' ')
	if [[ -z $hooks && -z $cfg && -z $before && -z $after ]]; then
		res HOOK-ESCAPE PASS "no planted hook in .git/hooks, no fsmonitor/hooksPath in .git/config, no /tmp marker before or after git status/log as pclip"
	else
		res HOOK-ESCAPE FAIL "hooks=[$hooks] config=[$cfg] markers before=[$before] after=[$after]"
	fi
	info hostile-1-kv "$(grep -E '^kv (commit|planted|issue-patch)=' "$OUT/runlog.txt" | cut -c4- | tr '\n' ' ')"
}

check_hostile_2() {
	local read present in_logs=0
	read=$(kv leak-read)
	present=$(kv leak-present)
	grep -rqF -f "$CANARY" "$STATE/raw" && in_logs=1
	info leak-present "$present"
	if [[ -n $read && $read != "$CANARY_SHA" && $in_logs == 0 ]]; then
		res ARCHIVE-LEAK PASS "leak-read=$read (not the canary sha), canary content in no run log"
	else
		res ARCHIVE-LEAK FAIL "leak-read=${read:-missing} canary-sha-match=$([[ $read == "$CANARY_SHA" ]] && echo yes || echo no) canary-in-logs=$in_logs"
	fi
}

log "scenario $sc"
ensure_agent
setup_repo
run_issue
case $sc in
file) check_file ;;
duplex) check_duplex ;;
hostile-1) check_hostile_1 ;;
hostile-2) check_hostile_2 ;;
esac
check_lease_cycle
check_secrets
grep -E "^kv " "$OUT/runlog.txt" | grep -vE '^kv env-names=' | head -40
log "scenario $sc done"
