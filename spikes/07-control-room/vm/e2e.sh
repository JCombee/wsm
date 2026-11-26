#!/usr/bin/env bash
# Spike 07 runs against the contained control room, through the tenant API.
# Usage (as root, through run.sh): bash e2e.sh file|duplex|symlink-1|symlink-2
#
# One company, one wsm environment and one claude_local agent serve every
# scenario (state in /root/s07/e2e.env). Each scenario gets its own Paperclip
# project and repository in the control room's volume, except symlink-2, which
# reuses symlink-1's. Raw run logs stay in /root/s07/raw, never in results/.
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

sc=${1:-}
case $sc in
file | duplex | symlink-1 | symlink-2) ;;
*)
	echo "usage: e2e.sh file|duplex|symlink-1|symlink-2" >&2
	exit 2
	;;
esac

OUT=$RESULTS/e2e-$sc
RAW=$RAWDIR/$sc
rm -rf "$OUT" "$RAW"
mkdir -p "$OUT" "$RAW"
ACTIVE='queued|running|starting|preparing|finalizing|scheduled_retry'
CANARY=/paperclip/s07-canary.txt
PATTERNS=$STATE/patterns

runs_json() {
	pc -sS "$API/companies/$CID/heartbeat-runs?agentId=$AID" |
		jq -c '(if type=="array" then . else (.items // .runs // .data // []) end)'
}

# res_sticky <ID> PASS|FAIL <detail>: a criterion that several scenarios
# check is PASS only while every scenario's last result is PASS.
res_sticky() {
	local id=$1 status=$2 detail=$3 f=$STATE/sticky-$1 agg=PASS all
	touch "$f"
	{
		grep -vP "^\Q$sc\E\t" "$f"
		printf '%s\t%s\t%s\n' "$sc" "$status" "$detail"
	} >"$f.tmp"
	mv "$f.tmp" "$f"
	grep -qP '\tFAIL\t' "$f" && agg=FAIL
	all=$(awk -F'\t' '{printf "%s%s: %s %s", (NR>1 ? "; " : ""), $1, $2, $3}' "$f")
	res "$id" "$agg" "$all"
}

# new_value <file> writes a random value to a root-only file once.
new_value() {
	[[ -s $1 ]] && return 0
	(umask 077 && head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32 >"$1")
}

# patterns writes the watcher's pattern files: the agent's sentinel (a
# positive control: it must show up in the sandbox), the value under a denied
# name, the canary and the control room's own secrets (secrets.sh).
patterns() {
	install -d -m 0700 "$PATTERNS"
	new_value "$STATE/agent-sentinel"
	new_value "$STATE/denied-value"
	cp "$STATE/agent-sentinel" "$PATTERNS/agent"
	cp "$STATE/denied-value" "$PATTERNS/denied"
	[[ -s $STATE/canary ]] && cp "$STATE/canary" "$PATTERNS/canary"
	bash "$VM_DIR/secrets.sh" "$PATTERNS/server" >"$OUT/secret-names.txt" 2>&1
	# Every value scrubs results/ at collect.
	cat "$PATTERNS"/* | sed '/^$/d' >"$STATE/secret-values"
}

ensure_agent() {
	CID= EID= AID=
	if [[ -f $STATE/e2e.env ]]; then
		# shellcheck disable=SC1091
		source "$STATE/e2e.env"
		[[ -n $CID && -n $AID ]] && pc -fsS -o /dev/null "$API/agents/$AID" || CID=
	fi
	[[ -n $CID ]] && return 0
	log "create company and agent"
	local code i st last=""
	# The stack's company, seeded by the operator's trusted headers (a cloud
	# instance refuses POST /companies).
	CID=$(company_id)
	info e2e-company "stack company ${CID:-none}"
	# The environment is the instance's managed sandbox environment, which the
	# managed config declares and the boot provisions; nobody creates it.
	code=$(call GET "/companies/$CID/environments")
	EID=$(jq -r '[.[]? | select(.driver=="sandbox" and (.config.provider // "")=="wsm" and (.status // "active")=="active")]
		| (map(select(.name=="wsm")) + .) | first | .id // empty' "$RAW/resp.json")
	info e2e-environment "GET environments -> $code; visible: $(jq -c '[.[]? | {name, driver, status, provider: (.config.provider // null)}]' "$RAW/resp.json" 2>/dev/null | head -c 300); using ${EID:-none}"
	# On a cloud instance a claude_local agent becomes a Paperclip Runner agent
	# (paperclip_runner, provider acpx) unless it asks for the legacy runner,
	# and Paperclip Runner needs its own runtime in the sandbox. The legacy
	# runner runs the claude CLI through exec, as in spike 05.
	# The agent JSON holds the sentinel and the denied value, so it goes through
	# a 0600 file, never argv. GH_TOKEN is on env_deny: the tenant API must strip it.
	(umask 077 && jq -n --arg env "$EID" --rawfile s "$STATE/agent-sentinel" --rawfile d "$STATE/denied-value" '{name:"s07-mock-claude", adapterType:"claude_local", runner:"legacy",
		adapterConfig:{command:"claude", engine:"cli", dangerouslySkipPermissions:true, timeoutSec:600, maxTurnsPerRun:10,
		env:{ANTHROPIC_BASE_URL:"http://127.0.0.1:8787/anthropic", ANTHROPIC_API_KEY:"mock-key", DISABLE_TELEMETRY:"1",
			DISABLE_ERROR_REPORTING:"1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:"1", DISABLE_AUTOUPDATER:"1",
			S07_SENTINEL:($s|rtrimstr("\n")), GH_TOKEN:($d|rtrimstr("\n"))}}}
		+ (if $env == "" then {} else {defaultEnvironmentId:$env} end)' >"$STATE/agent.json")
	code=$(call POST "/companies/$CID/agents" "$STATE/agent.json")
	AID=$(jq -r '.id // empty' "$RAW/resp.json")
	# Without an explicit environment the run still lands on the managed one:
	# the instance default, or the local-to-managed redirect.
	info e2e-agent "POST agents -> $code status=$(jq -r .status "$RAW/resp.json") environment=$(jq -r '.defaultEnvironmentId // "none (instance default)"' "$RAW/resp.json")"
	if [[ -z $CID || -z $AID ]]; then
		echo "e2e: could not create the company or the agent; see the INFO lines" >&2
		rm -f "$STATE/e2e.env"
		exit 1
	fi
	printf 'CID=%s\nEID=%s\nAID=%s\n' "$CID" "$EID" "$AID" >"$STATE/e2e.env"
	# A new agent passes a lifecycle check (the hello probe in its environment).
	local t0
	t0=$(date +%s)
	for i in $(seq 1 90); do
		st=$(pc -sS "$API/agents/$AID" | jq -r '[.status, (.lifecycleOperation.phase // "none")] | join("/")')
		[[ $st != "$last" ]] && {
			log "agent $st after $(($(date +%s) - t0))s"
			last=$st
		}
		[[ $st == */none ]] && break
		sleep 2
	done
	info agent-lifecycle "$last after $(($(date +%s) - t0))s"
}

# The canary lives in the control room's volume; only its hash leaves it.
ensure_canary() {
	crx sh -c 'test -s "$1" || (umask 077 && head -c 48 /dev/urandom | base64 | tr -d "\n" >"$1")' sh "$CANARY"
	(umask 077 && crx cat "$CANARY" >"$STATE/canary")
	CANARY_SHA=$(sha256sum <"$STATE/canary" | cut -c1-64)
}

setup_repo() {
	REPO=/paperclip/repos/repo-$sc
	[[ $sc == symlink-2 ]] && REPO=/paperclip/repos/repo-symlink-1
	if [[ $sc != symlink-2 ]]; then
		crx sh -c 'set -e; rm -rf "$1"; mkdir -p "$1/.s05"; git init -q -b main "$1"' sh "$REPO"
		crx sh -c 'cat >"$1/.s05/run.sh" && printf "# s07 %s\n" "$2" >"$1/README.md"' sh "$REPO" "$sc" <"$VM_DIR/scenario.sh"
		crx git -C "$REPO" add README.md .s05/run.sh
		crx git -C "$REPO" -c user.name=owner -c user.email=owner@example.invalid commit -q -m "Initial commit"
		# Under enableManagedSandboxOnly Paperclip refuses a workspace cwd (a
		# host folder). A git repository by file URL is allowed: Paperclip clones
		# it into its own checkout in the volume, and the plugin syncs that
		# checkout into the sandbox and back.
		local code
		code=$(jcall POST "/companies/$CID/projects" "{\"name\":\"s07-$sc-$(date +%s)\",\"status\":\"in_progress\",\"workspace\":{\"name\":\"repo-$sc\",\"sourceType\":\"git_repo\",\"repoUrl\":\"file://$REPO\",\"isPrimary\":true}}")
		PID_=$(jq -r '.id // empty' "$RAW/resp.json")
		echo "$PID_" >"$STATE/project-$sc"
		info "project-$sc" "POST projects (repoUrl file://$REPO) -> $code $([[ $code == 2* ]] || resp_brief 200)"
	else
		PID_=$(cat "$STATE/project-symlink-1")
	fi
	FIRST=$(crx git -C "$REPO" rev-list --max-parents=0 HEAD | tail -1)
}

# checkout prints Paperclip's checkout of the scenario's repository
# (<instance>/projects/<company>/<project>/<repo name>[-<hash>]), or the
# source repository while there is none yet.
checkout() {
	local d
	d=$(crx sh -c 'find /paperclip/pc -maxdepth 9 -type d -path "*/projects/*/$1/*" -name "$2*" -prune 2>/dev/null | head -1' sh "$PID_" "${REPO##*/}")
	echo "${d:-$REPO}"
}

start_watch() {
	rm -f "$STATE/watch.stop"
	bash "$VM_DIR/watch.sh" "$PATTERNS" "$OUT/watcher.txt" "$STATE/watch.stop" &
	WATCH_PID=$!
}

stop_watch() {
	touch "$STATE/watch.stop"
	wait "$WATCH_PID" 2>/dev/null
	rm -f "$STATE/watch.stop"
}

# runlog <log.json>... prints "kv key=value" for the scenario script's
# KEY=value lines in Paperclip run logs (the first one wins). The log's
# content is NDJSON with {stream, chunk}; a chunk may hold stream-json.
runlog() {
	jq -r '
		(.content // "") | split("\n") | map(fromjson? | .chunk? | strings) | join("")
		| split("\n")[]
		| if startswith("{") then ((fromjson? | [.. | strings] | .[] | split("\n")[]) // .) else . end
		| sub("^\\s+"; "")' "$@" 2>/dev/null |
		grep -oE '^(scenario|ws-head|issue-get|issue-patch|env-names|env-count|env-chain|bridge-me|commit|leak-[a-z-]+|s07-end)=.*' |
		awk -F= '!seen[$1]++ {print "kv " $0}'
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
	code=$(jcall POST "/companies/$CID/issues" "$(jq -nc --arg p "$PID_" --arg a "$AID" --arg t "[s07-$sc] S2 scenario $sc $(date +%s)" \
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
	usage "run-$sc"
	ISSUE_STATUS=$(pc -sS "$API/issues/$IID" | jq -r .status)
	if [[ $ISSUE_STATUS != done ]]; then
		code=$(jcall PATCH "/issues/$IID" '{"status":"cancelled"}')
		info "issue-$sc-cancel" "status was $ISSUE_STATUS; PATCH cancelled -> $code"
	fi
	jq -r 'sort_by(.createdAt // .startedAt // "") | .[] | [.id, .status, (.invocationSource // ""), (.startedAt // ""), (.finishedAt // ""), ((.error // "") | tostring | .[0:240])] | @tsv' \
		"$RAW/runs.json" >"$OUT/runs.tsv"
	for rid in $(jq -r 'sort_by(.createdAt // .startedAt // "") | .[].id' "$RAW/runs.json"); do
		pc -sS "$API/heartbeat-runs/$rid/log?offset=0&limitBytes=8000000" >"$RAW/log-$rid.json"
	done
	runlog "$RAW"/log-*.json >"$OUT/runlog.txt"
	cr_log_since "$T_START" >"$RAW/cr.log"
	grep -F '[plugin]' "$RAW/cr.log" | grep -F 's07 ' | cut -c1-400 >"$OUT/plugin-log.txt"
	info "run-duration-$sc" "issue to quiet: $((T_END - T_START))s; runs: $(awk -F'\t' '{printf "%s/%s ", $2, $3}' "$OUT/runs.tsv")"
}

kv() { grep -m1 "^kv $1=" "$OUT/runlog.txt" | cut -d= -f2-; }
run_ok() { jq -e 'any(.[]; .status == "succeeded")' "$RAW/runs.json" >/dev/null; }
# The sandbox's commit comes back into Paperclip's checkout, not the source.
cr_git_log() {
	local d
	d=$(checkout)
	info "checkout-$sc" "$d" >/dev/null
	crx git -C "$d" log --format=%s -5 | tr '\n' '|'
}

# run_sandboxes prints the tenant sandbox IDs this scenario's runs used.
run_sandboxes() { grep -oP 's07 lease acquire sandbox=\Kt[a-z0-9]{10}' "$OUT/plugin-log.txt" | sort -u; }

# audit_since prints the tenant audit lines since the scenario started.
audit_since() { jq -c --arg t "$(date -u -d "@$T_START" +%Y-%m-%dT%H:%M:%S)" 'select(.time >= $t)' "$AUDIT" 2>/dev/null; }

check_file() {
	local glog opens
	opens=$(grep -c 's07 duplex open' "$OUT/plugin-log.txt")
	glog=$(cr_git_log)
	if run_ok && [[ $ISSUE_STATUS == done && $(kv bridge-me) == 200 && $opens == 0 ]] && grep -q "S2 spike commit from the sandbox (file)" <<<"$glog"; then
		res RUN-FILE PASS "run succeeded, issue done, bridge-me=200, duplex opens=0; control room git log: $glog"
	else
		res RUN-FILE FAIL "runs=$(cut -f2 "$OUT/runs.tsv" | tr '\n' ',') issue=$ISSUE_STATUS bridge-me=$(kv bridge-me) duplex-opens=$opens git log: $glog errors: $(cut -f6 "$OUT/runs.tsv" | tr '\n' ' ' | cut -c1-300)"
	fi
	info env-names "$(kv env-count) names: $(kv env-names)"
}

check_duplex() {
	local opens fallback glog code
	opens=$(grep -c 's07 duplex open' "$OUT/plugin-log.txt")
	fallback=$(grep -ohE '(Could not open the sandbox duplex channel|Sandbox duplex readiness failed) \([a-z_]+\)' "$RAW"/log-*.json 2>/dev/null | sort -u | tr '\n' ' ')
	glog=$(cr_git_log)
	if run_ok && [[ $ISSUE_STATUS == done && $(kv bridge-me) == 200 && -z $fallback ]] && ((opens > 0)) && grep -q "S2 spike commit from the sandbox (duplex)" <<<"$glog"; then
		res RUN-DUPLEX PASS "run succeeded, issue done, duplex opens=$opens, no fallback, bridge-me=200; control room git log: $glog"
	else
		res RUN-DUPLEX FAIL "runs=$(cut -f2 "$OUT/runs.tsv" | tr '\n' ',') issue=$ISSUE_STATUS opens=$opens fallback=${fallback:-none} bridge-me=$(kv bridge-me) git log: $glog"
	fi
	info duplex-lines "$(grep -oP 's07 duplex (exit|terminate|close) [^\x1b{]{0,160}' "$OUT/plugin-log.txt" | head -4 | tr '\n' ' ')"
	code=$(jcall PATCH /instance/settings/experimental '{"enableSandboxDuplexBridge": false}')
	info duplex-setting-reset "enableSandboxDuplexBridge=false -> $code"
}

# check_env: ENV-DENY and ENV-SERVER-SECRETS for the run of this scenario.
# ENV-DENY judges the agent process as the exec started it: the top of the
# scenario's env-chain, and the names the tenant API passed on (the audit's
# env_names are recorded after env_deny). A name that only a process inside
# the sandbox set for its own children (the claude CLI sets its
# CLAUDE_CODE_MESSAGING_* pair for its tools) did not come from the control
# room; it is reported, not counted.
check_env() {
	local names present="" n dropped line uid class kind cmdline_server=0 env_server_sbx=0 \
		env_denied_sbx=0 env_agent_sbx=0 lu luid in_audit=0 in_results=0 chain top forwarded \
		env_server_unknown=0 srv_passed srv_dropped
	names=",$(kv env-names),"
	for n in "${ENV_DENY[@]}"; do
		[[ $names == *",$n,"* ]] && present+="$n "
	done
	chain=$(kv env-chain)
	top=${chain##*,}
	forwarded=$(audit_since | jq -r 'select(.exec.env_names != null) | .exec.env_names[]' | sort -u |
		grep -xF -f <(printf '%s\n' "${ENV_DENY[@]}") | tr '\n' ' ')
	dropped=$(audit_since | jq -r 'select(.exec.env_dropped != null) | .exec.env_dropped[]' | sort | uniq -c | awk '{printf "%s(%s) ", $2, $1}')
	lu=$(project_field linux_user)
	luid=$(id -u "$lu")
	while read -r line; do
		[[ $line == hit* ]] || continue
		class=$(grep -oP 'class=\K\S+' <<<"$line")
		kind=$(grep -oP 'kind=\K\S+' <<<"$line")
		uid=$(grep -oP 'uid=\K\S+' <<<"$line")
		# A sandbox process runs as the project user or one of its subordinate IDs.
		local sub
		sub=$(awk -F: -v u="$lu" '$1==u {print $2, $3}' /etc/subuid)
		if [[ $uid == "$luid" ]] || { [[ -n $sub && $uid =~ ^[0-9]+$ ]] && ((uid >= ${sub% *} && uid < ${sub% *} + ${sub#* })); }; then
			case $class/$kind in
			server/environ) env_server_sbx=$((env_server_sbx + 1)) ;;
			denied/environ) env_denied_sbx=$((env_denied_sbx + 1)) ;;
			agent/environ) env_agent_sbx=$((env_agent_sbx + 1)) ;;
			esac
		fi
		[[ $class/$kind == server/cmdline ]] && cmdline_server=$((cmdline_server + 1))
		# A hit whose owner the watcher could not read cannot be cleared.
		[[ $class/$kind == server/environ && ! $uid =~ ^[0-9]+$ ]] && env_server_unknown=$((env_server_unknown + 1))
	done <"$OUT/watcher.txt"
	# Race-free: the names of the control room's own secrets (cr.env) in any
	# exec the tenant API passed on (env_names are recorded after env_deny),
	# and how often it dropped them.
	srv_passed=$(audit_since | jq -r 'select(.exec.env_names != null) | .exec.env_names[]' | sort -u |
		grep -xF -f <(sed -n 's/=.*//p' "$ENVDIR/cr.env") | tr '\n' ' ')
	srv_dropped=$(audit_since | jq -r 'select(.exec.env_dropped != null) | .exec.env_dropped[]' |
		grep -xF -f <(sed -n 's/=.*//p' "$ENVDIR/cr.env") | sort | uniq -c | awk '{printf "%s(%s) ", $2, $1}')
	# The run must succeed without the dropped names. symlink-1's run is meant
	# to fail, and only on the plugin's refusal to archive the symlink (that is
	# SYMLINK-BUNDLE's defence); any other failure still counts.
	local run_state=failed
	if run_ok; then
		run_state=succeeded
	elif [[ $sc == symlink-1 ]] && cut -f6 "$OUT/runs.tsv" | grep -q 'archive refused: symlink'; then
		run_state="failed as intended (the plugin refused the symlink)"
	fi
	if [[ -n $chain && ${top#*:} == - && -z $forwarded && $run_state != failed ]] && ((env_denied_sbx == 0)) && grep -q 'GH_TOKEN' <<<"$dropped"; then
		res_sticky ENV-DENY PASS "the agent process as exec'd (${top%%:*}) holds none of the ${#ENV_DENY[@]} env_deny names and the tenant API passed none on; dropped: $dropped; set inside the sandbox for its children: [${present% }] (chain from the script up: $chain); denied value in 0 sandbox processes; run $run_state"
	else
		res_sticky ENV-DENY FAIL "exec'd process: ${top:-unknown} passed on: [$forwarded] in the script's env: [$present] dropped: [$dropped] chain: ${chain:-none} denied value in $env_denied_sbx sandbox processes; run $run_state"
	fi
	if [[ -s $PATTERNS/server ]]; then
		grep -qF -f "$PATTERNS/server" "$AUDIT" && in_audit=1
		grep -rqF -f "$PATTERNS/server" "$RESULTS" && in_results=1
	fi
	if [[ -s $PATTERNS/server && -z $srv_passed ]] && ((cmdline_server == 0 && env_server_sbx == 0 && env_server_unknown == 0 && in_audit == 0 && in_results == 0 && env_agent_sbx > 0)); then
		res_sticky ENV-SERVER-SECRETS PASS "$(grep -c . "$PATTERNS/server") server secrets: no exec passed their names on (the tenant API dropped: ${srv_dropped:-none}); values in 0 sandbox environ (0 unattributed), 0 argv, 0 in the audit and results/; control: agent sentinel in $env_agent_sbx sandbox environ ($(grep -oP '^scans=\K[0-9]+' "$OUT/watcher.txt") scans)"
	else
		res_sticky ENV-SERVER-SECRETS FAIL "secrets=$(grep -c . "$PATTERNS/server" 2>/dev/null) names passed on: [$srv_passed] dropped: [$srv_dropped] sandbox environ=$env_server_sbx unattributed=$env_server_unknown argv=$cmdline_server audit=$in_audit results=$in_results control(agent in sandbox environ)=$env_agent_sbx"
	fi
	info "watcher-$sc" "$(grep -oP '^scans=\K[0-9]+' "$OUT/watcher.txt") scans; $(grep '^hit ' "$OUT/watcher.txt" | awk '{print $2"/"$3"/"$5}' | sort | uniq -c | awk '{printf "%s x%s ", $2, $1}')"
}

check_symlink_1() {
	local state glog
	state=$(crx sh -c 'cd "$1" && if [ -L leak ]; then echo "symlink:$(readlink leak)"; elif [ -e leak ]; then echo file; else echo absent; fi' sh "$(checkout)" 2>&1)
	glog=$(cr_git_log)
	echo "$state" >"$STATE/symlink-1-state"
	info symlink-1 "commit=$(kv commit) run=$(cut -f2 "$OUT/runs.tsv" | tr '\n' ',') leak in the control room's repo: $state; git log: $glog"
}

check_symlink_2() {
	local read present target in_logs=0 in_results=0 state
	read=$(kv leak-read)
	present=$(kv leak-present)
	target=$(kv leak-target)
	state=$(cat "$STATE/symlink-1-state" 2>/dev/null)
	grep -rqF -f "$STATE/canary" "$RAWDIR" && in_logs=1
	grep -rqF -f "$STATE/canary" "$RESULTS" && in_results=1
	if [[ -n $read && $read != "$CANARY_SHA" && $in_logs == 0 && $in_results == 0 ]]; then
		res SYMLINK-BUNDLE PASS "control room repo after run 1: ${state:-?}; sandbox in run 2: leak=$present${target:+ -> $target}, read=$read (not the canary), cr path=$(kv leak-cr-path); canary in no run log or result"
	else
		res SYMLINK-BUNDLE FAIL "control room repo: ${state:-?}; sandbox: leak=$present read=${read:-missing} canary-sha-match=$([[ $read == "$CANARY_SHA" ]] && echo yes || echo no) in-logs=$in_logs in-results=$in_results"
	fi
}

check_lease_cycle() {
	local i left acq rel missing=""
	for i in $(seq 1 24); do
		left=$(wsmapi "/sandboxes?project=$PROJECT" | jq -r '[.[] | .slug] | join(" ")')
		[[ -z $left ]] && break
		sleep 5
	done
	acq=$(grep -oP 's07 lease acquire sandbox=\Kt[a-z0-9]{10}' "$OUT/plugin-log.txt" | sort -u)
	rel=$(grep -oP 's07 lease (release|destroy) sandbox=\Kt[a-z0-9]{10}' "$OUT/plugin-log.txt" | sort -u)
	for s in $acq; do grep -qx "$s" <<<"$rel" || missing+="$s "; done
	info "lease-cycle-$sc" "acquires=$(wc -w <<<"$acq") without release: [$missing] left: [$left] execs=$(audit_since | jq -r 'select(.route=="exec" and .decision=="allow") | .sandbox' | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}')"
}

log "scenario $sc"
patterns
ensure_agent
[[ $sc == symlink-* ]] && ensure_canary
patterns
setup_repo
run_issue
case $sc in
file) check_file ;;
duplex) check_duplex ;;
symlink-1) check_symlink_1 ;;
symlink-2) check_symlink_2 ;;
esac
check_env
check_lease_cycle
grep -E "^kv " "$OUT/runlog.txt" | grep -vE '^kv env-names=' | head -20
log "scenario $sc done"
