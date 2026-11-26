#!/usr/bin/env bash
# Spike 07 containment checks against the locked control room (plan §13.2):
# nothing an agent or a board user configures may start a process in the
# control room. Usage (as root, through run.sh): bash contain.sh
#
# Evidence per check: a marker file the command would create in the control
# room, a process sampler on the container (podman top, from the host), the
# API's answer and, for runs, where the run went (plugin lease lines).
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

# contain.sh [ws]: ws repeats only WS-SERVICE, after the runs (their
# execution workspaces exist then).
MODE=${1:-all}
case $MODE in
all) sfx= ;;
ws) sfx=-ws ;;
*)
	echo "usage: contain.sh [ws]" >&2
	exit 2
	;;
esac
OUT=$RESULTS/contain$sfx
RAW=$RAWDIR/contain$sfx
# Names in the stack's one company must differ between runs.
RUNID=$(date +%s)
rm -rf "$OUT" "$RAW"
mkdir -p "$OUT" "$RAW"
SAMPLE=$RAW/procs.txt

# markers prints the marker files present in the control room.
markers() { crx sh -c 'ls -1 /tmp/s07-marker-* /paperclip/s07-marker-* 2>/dev/null' | tr '\n' ' '; }

# The sampler records the container's processes every second (podman top
# reads /proc from the host side, so the image needs no ps).
start_sampler() {
	rm -f "$RAW/sample.stop"
	(
		while [[ ! -e $RAW/sample.stop ]]; do
			cr top "$CTR" pid,args 2>/dev/null | sed 1d >>"$SAMPLE"
			sleep 1
		done
	) &
	SAMPLER=$!
}
stop_sampler() {
	touch "$RAW/sample.stop"
	wait "$SAMPLER" 2>/dev/null
}
# seen <ERE> prints the distinct sampled command lines that match.
seen() { grep -E "$1" "$SAMPLE" 2>/dev/null | awk '{$1=""; print substr($0, 2, 120)}' | sort -u | tr '\n' ';'; }

agent_runs() {
	pc -sS "$API/companies/$CID/heartbeat-runs?agentId=$1" |
		jq -c '(if type=="array" then . else (.items // .runs // .data // []) end)'
}

# wait_runs <agent> <seconds> waits until the agent has a run and none is
# active, then prints "status:error" per run.
wait_runs() {
	local aid=$1 secs=$2 i r=""
	for ((i = 0; i < secs; i += 5)); do
		r=$(agent_runs "$aid" | jq -r 'map(.status) | join(",")')
		[[ -n $r ]] && ! grep -qE 'queued|running|starting|preparing|finalizing|scheduled_retry' <<<"$r" && break
		sleep 5
	done
	agent_runs "$aid" | jq -r '.[] | "\(.status):\((.error // "") | tostring | .[0:160])"' | tr '\n' ';'
}

# leases_since <epoch> counts the plugin's lease acquires since then.
leases_since() { cr_log_since "$1" | grep -F '[plugin]' | grep -c 's07 lease acquire'; }

setup_company() {
	local code
	# A cloud instance creates no company through the API; the stack's
	# company comes from the operator's trusted headers.
	code=$(jcall POST /companies '{"name":"S07 contain"}')
	info contain-company-create "POST /companies -> $code $(resp_brief 120)"
	CID=$(company_id)
	info contain-company "stack company ${CID:-none}"
	crx sh -c 'set -e; d=/paperclip/repos/contain; rm -rf "$d"; mkdir -p "$d"; git init -q -b main "$d"; echo "# s07 contain" >"$d/README.md"; git -C "$d" add README.md; git -C "$d" -c user.name=owner -c user.email=owner@example.invalid commit -q -m init'
	# A host folder as workspace: enableManagedSandboxOnly refuses it (422).
	code=$(jcall POST "/companies/$CID/projects" '{"name":"s07-contain-cwd","status":"in_progress","workspace":{"name":"contain-cwd","cwd":"/paperclip/repos/contain","isPrimary":true}}')
	info contain-project-cwd "POST projects with a workspace cwd -> $code $(resp_brief 160)"
	# The workspace is the repository by file URL, and carries a runtime
	# service that would leave a marker.
	code=$(jcall POST "/companies/$CID/projects" '{"name":"s07-contain","status":"in_progress","workspace":{"name":"contain","sourceType":"git_repo","repoUrl":"file:///paperclip/repos/contain","isPrimary":true,
		"runtimeConfig":{"workspaceRuntime":{"services":[{"name":"s07-svc","command":"touch /tmp/s07-marker-wsservice; exec sleep 300","cwd":".","port":{"type":"auto"},"lifecycle":"shared"}]}}}}')
	PRJ=$(jq -r '.id // empty' "$RAW/resp.json")
	info contain-project "POST projects (repoUrl, runtime service) -> $code project=${PRJ:-none} $([[ $code == 2* ]] || resp_brief 200)"
	if [[ -z $PRJ ]]; then
		code=$(jcall POST "/companies/$CID/projects" '{"name":"s07-contain-plain","status":"in_progress","workspace":{"name":"contain","sourceType":"git_repo","repoUrl":"file:///paperclip/repos/contain","isPrimary":true}}')
		PRJ=$(jq -r '.id // empty' "$RAW/resp.json")
		info contain-project-plain "POST projects (repoUrl only) -> $code project=${PRJ:-none}"
	fi
	WSID=$(jq -r '(.primaryWorkspace.id // .workspaces[0].id // .workspace.id // empty)' "$RAW/resp.json")
	[[ -z $WSID && -n $PRJ ]] && WSID=$(pc -sS "$API/projects/$PRJ" | jq -r '(.primaryWorkspace.id // .workspaces[0].id // empty)')
	info contain-workspace "project=${PRJ:-none} workspace=${WSID:-none}"
}

# check_lock: the board cannot lift the lock through the API (INFO lines; the
# criteria below test what the lock does).
check_lock() {
	local code id
	code=$(jcall PATCH /instance/settings/experimental '{"enableManagedSandboxOnly": false}')
	info lock-managed-only "PATCH enableManagedSandboxOnly=false -> $code $(resp_brief 160)"
	id=$(plugin_record | jq -r '.id // empty')
	if [[ -n $id ]]; then
		code=$(jcall POST "/plugins/$id/disable" '{}')
		info lock-plugin-disable "POST plugins/<wsm>/disable -> $code $(resp_brief 120)"
		# Had it worked, the later checks would run without the provider.
		[[ $code == 2* ]] && info lock-plugin-reenable "POST plugins/<wsm>/enable -> $(jcall POST "/plugins/$id/enable" '{}')"
	fi
}

# issue_for <agent> <title> assigns an issue to wake the agent.
issue_for() {
	jcall POST "/companies/$CID/issues" "$(jq -nc --arg p "$PRJ" --arg a "$1" --arg t "$2" \
		'{title:$t, description:"S2 containment check.", status:"todo", projectId:$p, assigneeAgentId:$a}')" >/dev/null
}

check_local_process() {
	local code aid t0 runs="" m procs leases
	log "LOCAL-PROCESS"
	[[ -n ${CID:-} ]] || {
		res LOCAL-PROCESS FAIL "not measured: no company"
		return
	}
	t0=$(date +%s)
	code=$(jcall POST "/companies/$CID/agents" '{"name":"s07-process","adapterType":"process","adapterConfig":{"command":"touch /tmp/s07-marker-process; touch /paperclip/s07-marker-process; sleep 20","timeoutSec":60}}')
	aid=$(jq -r '.id // empty' "$RAW/resp.json")
	CREATED+=("$aid")
	info local-process-agent "POST agents -> $code status=$(jq -r '.status // empty' "$RAW/resp.json") $(resp_brief 160)"
	if [[ -n $aid ]]; then
		pc -sS -X POST "$API/agents/$aid/resume" -o /dev/null 2>&1
		issue_for "$aid" "[s07-contain] process adapter"
		runs=$(wait_runs "$aid" 120)
	fi
	m=$(markers)
	procs=$(seen 's07-marker-process|sleep 20')
	leases=$(leases_since "$t0")
	if [[ -z $m && -z $procs && ( -n $aid || $code == 4[0-9][0-9] ) ]]; then
		res LOCAL-PROCESS PASS "agent: POST -> $code; no marker and no process in the control room; runs: ${runs:-none}; wsm leases since: $leases"
	else
		res LOCAL-PROCESS FAIL "markers: [$m] processes: [$procs] runs: ${runs:-none}; wsm leases since: $leases"
	fi
}

check_local_adapter() {
	local code aid t0 runs="" procs leases
	log "LOCAL-ADAPTER"
	[[ -n ${CID:-} ]] || {
		res LOCAL-ADAPTER FAIL "not measured: no company"
		return
	}
	t0=$(date +%s)
	# No defaultEnvironmentId: without the lock this is the local environment.
	code=$(jcall POST "/companies/$CID/agents" '{"name":"s07-local-claude","adapterType":"claude_local","runner":"legacy","adapterConfig":{"command":"claude","engine":"cli","dangerouslySkipPermissions":true,"timeoutSec":300,"maxTurnsPerRun":3,
		"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8787/anthropic","ANTHROPIC_API_KEY":"mock-key","DISABLE_TELEMETRY":"1","DISABLE_AUTOUPDATER":"1"}}}')
	aid=$(jq -r '.id // empty' "$RAW/resp.json")
	CREATED+=("$aid")
	info local-adapter-agent "POST agents -> $code status=$(jq -r '.status // empty' "$RAW/resp.json") env=$(jq -r '.defaultEnvironmentId // "none"' "$RAW/resp.json")"
	if [[ -n $aid ]]; then
		issue_for "$aid" "[s07-contain] claude_local without environment"
		runs=$(wait_runs "$aid" 180)
	fi
	procs=$(seen '(^|/| )claude( |$)')
	leases=$(leases_since "$t0")
	if [[ -z $procs ]] && { [[ -z $aid && $code == 4[0-9][0-9] ]] || ((leases > 0)) || [[ $runs == *failed* || $runs == *cancelled* ]]; }; then
		res LOCAL-ADAPTER PASS "agent: POST -> $code; no claude process in the control room; wsm leases since: $leases; runs: ${runs:-none}"
	else
		res LOCAL-ADAPTER FAIL "claude processes: [$procs] wsm leases: $leases runs: ${runs:-none}"
	fi
}

check_ws_service() {
	local code m procs ews ew c ewcodes=""
	log "WS-SERVICE"
	# The project workspace has no host folder under the lock (repoUrl only).
	if [[ -n ${PRJ:-} && -n ${WSID:-} ]]; then
		code=$(jcall POST "/projects/$PRJ/workspaces/$WSID/runtime-services/start" '{}')
		info ws-service-start "project workspace start -> $code $(resp_brief 240)"
	else
		code=none
	fi
	# A run's execution workspace is Paperclip's checkout in the control room,
	# which the plugin syncs into the sandbox. Paperclip refuses workspace
	# commands only for remote-storage workspaces, so this route is the one
	# that could start the service in the control room.
	# A board user with runtime:manage may also write an execution workspace's
	# own runtime config (PATCH config.workspaceRuntime), so the check writes
	# the marker service into it first and then starts it.
	ews=$(pc -sS "$API/companies/$CID/execution-workspaces" | jq -r '(if type=="array" then . else (.items // .data // []) end) | .[].id' 2>/dev/null | head -3)
	for ew in $ews; do
		c=$(jcall PATCH "/execution-workspaces/$ew" '{"config":{"workspaceRuntime":{"services":[{"name":"s07-svc","command":"touch /tmp/s07-marker-wsservice; exec sleep 300","cwd":".","port":{"type":"auto"},"lifecycle":"shared"}]}}}')
		info "ws-service-ew-${ew:0:8}-config" "PATCH config.workspaceRuntime -> $c $(resp_brief 160)"
		ewcodes+="patch=$c,"
		c=$(jcall POST "/execution-workspaces/$ew/runtime-services/start" '{}')
		ewcodes+="start=$c "
		info "ws-service-ew-${ew:0:8}" "execution workspace start -> $c $(resp_brief 200)"
	done
	sleep 10
	m=$(markers)
	procs=$(seen 's07-marker-wsservice|sleep 300')
	if [[ -z $m && -z $procs ]]; then
		res WS-SERVICE PASS "project workspace start -> $code; execution workspaces ($(wc -w <<<"$ews")) start -> ${ewcodes:-none}; no marker and no service process in the control room"
	else
		res WS-SERVICE FAIL "project workspace start -> $code; execution workspaces start -> ${ewcodes:-none}; markers: [$m] processes: [$procs]"
	fi
	[[ -n ${PRJ:-} && -n ${WSID:-} ]] && jcall POST "/projects/$PRJ/workspaces/$WSID/runtime-services/stop" '{}' >/dev/null
	for ew in $ews; do jcall POST "/execution-workspaces/$ew/runtime-services/stop" '{}' >/dev/null; done
}

check_mcp_stdio() {
	local code en id hc cat imp before after new tpl tcode tid thc
	log "MCP-STDIO"
	before=$(sort -u "$SAMPLE" | awk '{$1=""; print}' | sort -u | wc -l)
	# Enabled, the approved template would start its server; then a disabled
	# one for the health check and the catalog.
	en=$(jcall POST "/companies/$CID/tools/connections" '{"name":"s07-stdio-on-'"$RUNID"'","transport":"local_stdio","config":{"templateId":"paperclip.echo-calculator-time"},"enabled":true}')
	info mcp-stdio-create-enabled "POST tools/connections (approved template, enabled) -> $en $(resp_brief 160)"
	code=$(jcall POST "/companies/$CID/tools/connections" '{"name":"s07-stdio-'"$RUNID"'","transport":"local_stdio","config":{"templateId":"paperclip.echo-calculator-time"},"enabled":false}')
	id=$(jq -r '.id // .connection.id // empty' "$RAW/resp.json")
	info mcp-stdio-create "POST tools/connections (approved template) -> $code $(resp_brief 160)"
	if [[ -n $id ]]; then
		hc=$(jcall POST "/tool-connections/$id/health-check" '{}')
		info mcp-stdio-health "health-check -> $hc $(jq -c '.connection | {healthStatus, healthMessage}' "$RAW/resp.json" 2>/dev/null | cut -c1-200)"
		cat=$(jcall POST "/tool-connections/$id/catalog/refresh" '{}')
		info mcp-stdio-catalog "catalog/refresh -> $cat $(resp_brief 160)"
	fi
	# An arbitrary command through the JSON import must not be accepted.
	imp=$(jcall POST "/companies/$CID/tools/mcp/import-json" '{"mcpJson":{"mcpServers":{"s07":{"command":"sh","args":["-c","touch /tmp/s07-marker-mcp; sleep 30"]}}}}')
	info mcp-stdio-import "import-json (raw command) -> $imp $(resp_brief 200)"
	# A tools admin can register a stdio command template of their own, with
	# any command; a connection on it is the direct way to a process.
	tpl=$(jcall POST "/companies/$CID/tools/stdio-templates" '{"templateId":"s07.marker.'"$RUNID"'","name":"s07 marker '"$RUNID"'","command":"sh","args":["-c","touch /tmp/s07-marker-mcp-template; exec sleep 30"]}')
	info mcp-stdio-template "POST tools/stdio-templates (own command) -> $tpl $(resp_brief 160)"
	tcode=$(jcall POST "/companies/$CID/tools/connections" '{"name":"s07-own-template-'"$RUNID"'","transport":"local_stdio","config":{"templateId":"s07.marker.'"$RUNID"'"},"enabled":true}')
	tid=$(jq -r '.id // .connection.id // empty' "$RAW/resp.json")
	info mcp-stdio-template-connection "create (enabled) -> $tcode $(resp_brief 160)"
	if [[ -n $tid ]]; then
		thc=$(jcall POST "/tool-connections/$tid/health-check" '{}')
		info mcp-stdio-template-health "health-check -> $thc $(jq -c '.connection | {healthStatus, healthMessage}' "$RAW/resp.json" 2>/dev/null | cut -c1-200)"
	fi
	sleep 5
	new=$(seen 'echo-calculator|synthetic-todo|mcp-server|s07-marker-mcp|stdio')
	after=$(markers)
	if [[ -z $new && $after != *s07-marker-mcp* ]]; then
		res MCP-STDIO PASS "approved template: enabled -> $en, disabled -> $code, health-check -> ${hc:-n/a}, catalog -> ${cat:-n/a}; import -> $imp; own template -> $tpl, connection -> $tcode, health-check -> ${thc:-n/a}; no stdio process and no marker in the control room"
	else
		res MCP-STDIO FAIL "approved template: enabled -> $en, disabled -> $code, health-check -> ${hc:-n/a}, catalog -> ${cat:-n/a}; import -> $imp; own template -> $tpl, connection -> $tcode, health-check -> ${thc:-n/a}; processes: [$new] markers: [$after]"
	fi
}

check_plugin_install() {
	local before after hb ha cli api m
	log "PLUGIN-INSTALL"
	before=$(pc -sS "$API/plugins" | jq -c '[.. | objects | (.pluginKey? // empty)] | sort')
	hb=$(crx sh -c "find $PLUGIN_DIR -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum" | cut -c1-16)
	# A plugin a board user or agent could drop into the volume.
	crx sh -c 'set -e; d=/paperclip/s07-evil-plugin; mkdir -p "$d/dist"
		printf "%s\n" "{\"name\":\"s07-evil\",\"version\":\"0.0.1\",\"type\":\"module\",\"paperclipPlugin\":{\"manifest\":\"./dist/manifest.js\",\"worker\":\"./dist/worker.js\"}}" >"$d/package.json"
		printf "%s\n" "import {writeFileSync} from \"node:fs\"; writeFileSync(\"/tmp/s07-marker-plugin\", \"x\"); export default {id:\"s07.evil\",apiVersion:1,version:\"0.0.1\",displayName:\"evil\",capabilities:[],entrypoints:{worker:\"./dist/worker.js\"}};" >"$d/dist/manifest.js"
		cp "$d/dist/manifest.js" "$d/dist/worker.js"'
	cli=$(crx paperclipai plugin install s07-paperclip-plugin-test --api-base http://127.0.0.1:3100 2>&1 | tr -c '[:print:]' ' ' | tail -c 200)
	api=$(jcall POST /plugins/install '{"packageName":"/paperclip/s07-evil-plugin","isLocalPath":true}')
	info plugin-install-api "local path -> $api $(resp_brief 200)"
	sleep 3
	after=$(pc -sS "$API/plugins" | jq -c '[.. | objects | (.pluginKey? // empty)] | sort')
	ha=$(crx sh -c "find $PLUGIN_DIR -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum" | cut -c1-16)
	m=$(markers)
	if [[ $api != 2* && $cli != *nstalled* && $before == "$after" && $hb == "$ha" && $m != *s07-marker-plugin* ]]; then
		res PLUGIN-INSTALL PASS "CLI npm install refused ($cli); API local path -> $api; plugins unchanged $after; $PLUGIN_DIR unchanged ($ha)"
	else
		res PLUGIN-INSTALL FAIL "CLI: $cli; API -> $api; plugins $before -> $after; dir $hb -> $ha; markers: [$m]"
	fi
}

# end_agents terminates the agents the checks created: a process agent never
# passes verification in a sandbox environment (test_environment_unavailable)
# and Paperclip would retry it, with a lease each time, for good.
CREATED=()
end_agents() {
	local a codes=""
	for a in "${CREATED[@]}"; do
		[[ -n $a ]] && codes+="$(jcall POST "/agents/$a/terminate" '{}') "
	done
	info contain-agents-terminated "POST agents/<id>/terminate -> ${codes:-none}"
}

log "containment checks ($MODE)"
: >"$SAMPLE"
start_sampler
if [[ $MODE == ws ]]; then
	CID=$(company_id)
	check_ws_service
else
	setup_company
	check_lock
	check_local_process
	check_local_adapter
	check_ws_service
	check_mcp_stdio
	check_plugin_install
	end_agents
fi
stop_sampler
cp "$SAMPLE" "$OUT/procs-sampled.txt"
sort -u -k2 "$SAMPLE" | awk '{$1=""; print substr($0, 2, 160)}' | sort -u >"$OUT/procs-distinct.txt"
usage "contain$sfx"
log "containment checks done ($MODE)"
