#!/usr/bin/env bash
# Spike 03: an MCP server (go-sdk, Streamable HTTP) as user wsm on
# 10.255.0.1:7443 with a bearer token per sandbox, reached from real wsm
# sandboxes by a probe, Claude Code and Codex. The agents get their "model"
# from a scripted mock inside the sandbox, so no credentials are needed
# (docs/plan/bouwplan-fase-1.md, sections 3.9, 3.10 and 5).
#
# Expects an installed wsm (deploy/install.sh) and the static mcpspike binary
# next to this script, or in $MCPSPIKE_BIN. Run only on a disposable VM, as
# root; see spikes/README.md. A failing check does not stop the script. Exit
# code: 0 when every criterion passed, 1 otherwise, 3/4 when guard refuses.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../lib.sh
source "$HERE/../lib.sh"

guard

PROJECT=mcp-a
MCP_IP=10.255.0.1
MCP_PORT=7443
UNIT=wsm-mcp-spike
STATE=/var/lib/wsm-mcp-spike
BIN_DIR=/usr/local/lib/wsm-mcp-spike
SRC_BIN=${MCPSPIKE_BIN:-$HERE/mcpspike}
MOCK_ADDR=127.0.0.1:18080
AGENT_TIMEOUT=240
PROMPT="Call the sandbox_info tool and report its result."
MODES=(stateful stateless)

results_init "$HERE/results"
TMP=$(mktemp -d)
# Tokens live only here (root, 0700) and in env files that exist for the
# duration of one exec. They are never printed or put in results/.
SECRETS=$(mktemp -d /root/wsm-mcp-secrets.XXXXXX)
chmod 0700 "$SECRETS"
TOKFILE=$SECRETS/tokens

cleanup() {
	systemctl stop "$UNIT" >/dev/null 2>&1
	rm -rf "$TMP" "$SECRETS"
	rm -rf /run/wsm-mcp-env.* 2>/dev/null
}
trap cleanup EXIT

log() { printf '== %s\n' "$*"; }

setup_fail() {
	report "SETUP-$1" FAIL "see $1.log"
	exit 1
}

# in_sbx <container> [podman exec options...] -- <cmd...> runs a command in
# a workspace container, through podman exec as the project user.
in_sbx() {
	local ctr=$1 opts=()
	shift
	while (($#)) && [[ $1 != -- ]]; do
		opts+=("$1")
		shift
	done
	shift
	as_user "$PUSER" podman exec "${opts[@]}" "$ctr" "$@"
}

# envfile_create <sandbox id> <token var> sets ENVF to a fresh 0600 env file,
# owned by the project user, with WSM_MCP_URL and WSM_MCP_TOKEN. The token
# goes through printf, a builtin, so it never appears on a command line.
# envfile_remove deletes it again right after the one exec that needs it.
envfile_create() {
	local id=$1 tokvar=$2 dir
	dir=$(mktemp -d /run/wsm-mcp-env.XXXXXX) || return 1
	chown "$PUSER:" "$dir" && chmod 0700 "$dir" || return 1
	ENVF=$dir/env
	(
		umask 077
		printf 'WSM_MCP_URL=http://%s:%s/mcp/%s\nWSM_MCP_TOKEN=%s\n' "$MCP_IP" "$MCP_PORT" "$id" "${!tokvar}" >"$ENVF"
	) || return 1
	chown "$PUSER:" "$ENVF"
}

envfile_remove() {
	rm -rf "$(dirname "$ENVF")"
	ENVF=
}

counter() {
	nft list counter inet wsm "$1" 2>/dev/null | grep -o 'packets [0-9]*' | awk '{print $2}'
}

# audit_after <mode> <line> prints the audit records after line <line>.
audit_after() {
	tail -n +"$(($2 + 1))" "$STATE/audit-$1.jsonl"
}

audit_lines() {
	wc -l <"$STATE/audit-$1.jsonl"
}

start_server() {
	local mode=$1 flag=() i
	[[ $mode == stateless ]] && flag=(-stateless)
	systemctl stop "$UNIT" >/dev/null 2>&1
	systemctl reset-failed "$UNIT" >/dev/null 2>&1
	install -o wsm -g wsm -m 0600 /dev/null "$STATE/audit-$mode.jsonl"
	install -o wsm -g wsm -m 0600 /dev/null "$STATE/server-$mode.log"
	systemd-run --unit="$UNIT" --uid=wsm --gid=wsm \
		-p NoNewPrivileges=yes -p ProtectSystem=strict -p ProtectHome=yes -p PrivateTmp=yes \
		-p ReadWritePaths="$STATE" \
		-p StandardOutput=append:"$STATE/server-$mode.log" \
		-p StandardError=append:"$STATE/server-$mode.log" \
		"$BIN_DIR/mcpspike" serve -addr "$MCP_IP:$MCP_PORT" \
		-config "$STATE/config.json" -audit "$STATE/audit-$mode.jsonl" "${flag[@]}" || return 1
	for i in $(seq 1 40); do
		ss -ltnH "sport = :$MCP_PORT" | grep -q "$MCP_IP:$MCP_PORT" && return 0
		sleep 0.25
	done
	return 1
}

stop_server() {
	systemctl stop "$UNIT" >/dev/null 2>&1
	cp "$STATE/server-$1.log" "$STATE/audit-$1.jsonl" "$RESULTS/"
}

# --- setup ------------------------------------------------------------------

log "checking wsm and tools"
(
	systemctl is-active wsm.service wsm-helper.socket || exit 1
	command -v jq >/dev/null || {
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -q && apt-get install -y -q jq
	} || exit 1
	[[ -x $SRC_BIN ]] || {
		echo "no mcpspike binary at $SRC_BIN"
		exit 1
	}
	file "$SRC_BIN" 2>/dev/null || true
	ldd "$SRC_BIN" 2>&1 | grep -q 'not a dynamic executable' || {
		echo "mcpspike is not static"
		exit 1
	}
	ip -4 addr show dev wsm0 | grep -q "inet $MCP_IP/" || exit 1
) >"$RESULTS/preconditions.log" 2>&1 || setup_fail preconditions

log "installing mcpspike and server state"
(
	install -d -o root -g root -m 0755 "$BIN_DIR" &&
		install -o root -g root -m 0755 "$SRC_BIN" "$BIN_DIR/mcpspike" &&
		install -d -o wsm -g wsm -m 0700 "$STATE" || exit 1
	stat -c '%n %s bytes' "$BIN_DIR/mcpspike"
) >"$RESULTS/install.log" 2>&1 || setup_fail install
note "mcpspike $(stat -c %s "$BIN_DIR/mcpspike") bytes, $(ldd "$BIN_DIR/mcpspike" 2>&1 | tr -d '\t')"

log "creating project $PROJECT with sandboxes s1 and s2 (builds the agent image)"
(
	systemctl stop "$UNIT" >/dev/null 2>&1
	wsm project delete "$PROJECT" 2>/dev/null
	wsm project create --slug "$PROJECT" --containerfile "$HERE/Containerfile" \
		--cpus 2 --memory 2g --pids 512 || exit 1
	for sb in s1 s2; do
		wsm sandbox create "$PROJECT/$sb" || exit 1
		wsm sandbox start "$PROJECT/$sb" || exit 1
	done
	wsm project list
	wsm sandbox list "$PROJECT"
) >"$RESULTS/sandboxes.log" 2>&1 || setup_fail sandboxes

PUSER=$(wsm project list | awk -v p="$PROJECT" '$1 == p { print $2 }')
S1=$(wsm sandbox list "$PROJECT" | awk -v t="$PROJECT/s1" '$1 == t { sub(/^sbx-/, "", $3); print $3 }')
S2=$(wsm sandbox list "$PROJECT" | awk -v t="$PROJECT/s2" '$1 == t { sub(/^sbx-/, "", $3); print $3 }')
C1=sbx-$S1-work
C2=sbx-$S2-work
if [[ -z $PUSER || -z $S1 || -z $S2 ]]; then
	echo "project user '$PUSER', s1 '$S1', s2 '$S2'" >>"$RESULTS/sandboxes.log"
	setup_fail sandboxes
fi
note "project $PROJECT user $PUSER, s1=$S1, s2=$S2"

log "waiting for exec in both sandboxes"
(
	for c in "$C1" "$C2"; do
		ok=0
		for _ in $(seq 1 60); do
			in_sbx "$c" -- true && ok=1 && break
			sleep 1
		done
		((ok)) || exit 1
	done
) >"$RESULTS/exec-wait.log" 2>&1 || setup_fail exec-wait

log "copying mcpspike into the sandboxes over exec stdin"
(
	for c in "$C1" "$C2"; do
		in_sbx "$c" -i -- dd of=/tmp/mcpspike status=none <"$BIN_DIR/mcpspike" || exit 1
		in_sbx "$c" -- chmod 0755 /tmp/mcpspike || exit 1
		in_sbx "$c" -- ls -l /tmp/mcpspike || exit 1
	done
) >"$RESULTS/copy.log" 2>&1 || setup_fail copy

log "generating tokens and the server config"
(
	umask 077
	gen() { head -c 32 /dev/urandom | base64 -w0 | tr '+/' '-_' | tr -d '='; }
	{
		gen
		echo
		gen
		echo
	} >"$TOKFILE"
	[[ $(grep -c . "$TOKFILE") == 2 ]] || exit 1
	h1=$(head -n1 "$TOKFILE" | tr -d '\n' | sha256sum | cut -d' ' -f1)
	h2=$(sed -n 2p "$TOKFILE" | tr -d '\n' | sha256sum | cut -d' ' -f1)
	jq -n --arg s1 "$S1" --arg h1 "$h1" --arg s2 "$S2" --arg h2 "$h2" \
		'{sandboxes: [{id: $s1, token_sha256: $h1}, {id: $s2, token_sha256: $h2}]}' >"$STATE/config.json" || exit 1
	chown wsm:wsm "$STATE/config.json" && chmod 0600 "$STATE/config.json" || exit 1
	ls -l "$STATE/config.json"
) >"$RESULTS/tokens.log" 2>&1 || setup_fail tokens
TOK1=$(sed -n 1p "$TOKFILE")
TOK2=$(sed -n 2p "$TOKFILE")
cp "$STATE/config.json" "$RESULTS/server-config.json" # hashes only

log "agent configuration in s1 and the mock model"
(
	jq -n '{mcpServers: {wsm: {type: "http", url: "${WSM_MCP_URL}", headers: {Authorization: "Bearer ${WSM_MCP_TOKEN}"}}}}' |
		in_sbx "$C1" -i -- sh -c 'cat >/workspace/.mcp.json' || exit 1
	cat <<EOF | in_sbx "$C1" -i -- sh -c 'mkdir -p /home/agent/.codex && cat >/home/agent/.codex/config.toml' || exit 1
model = "mock-model"
model_provider = "mock"

[model_providers.mock]
name = "mock"
base_url = "http://$MOCK_ADDR/openai/v1"
wire_api = "responses"
env_key = "MOCK_OPENAI_KEY"

[mcp_servers.wsm]
url = "http://$MCP_IP:$MCP_PORT/mcp/$S1"
bearer_token_env_var = "WSM_MCP_TOKEN"
default_tools_approval_mode = "approve"
EOF
	in_sbx "$C1" -- cat /workspace/.mcp.json /home/agent/.codex/config.toml
	in_sbx "$C1" -d -- sh -c "exec /tmp/mcpspike mockllm -addr $MOCK_ADDR >>/tmp/mockllm.log 2>&1" || exit 1
	in_sbx "$C1" -- bash -c 'for i in $(seq 1 40); do (echo >/dev/tcp/127.0.0.1/18080) 2>/dev/null && exit 0; sleep 0.25; done; exit 1' || exit 1
) >"$RESULTS/agent-setup.log" 2>&1 || setup_fail agent-setup

{
	echo "## node"
	in_sbx "$C1" -- node --version
	echo "## claude"
	in_sbx "$C1" -e HOME=/home/agent -- claude --version
	echo "## codex"
	in_sbx "$C1" -e HOME=/home/agent -- codex --version
	echo "## wsm"
	wsm version
	echo "## podman"
	podman --version
} >"$RESULTS/versions.txt" 2>&1
collect_versions "$RESULTS/os-versions.txt"
note "versions: $(grep -A1 -E '^## (node|claude|codex)' "$RESULTS/versions.txt" | grep -v -E '^(##|--)' | tr '\n' ' ')"

# --- per mode: probe, Claude Code, Codex ------------------------------------

declare -A PROBE_MARK PROBE_END CLAUDE_OK CODEX_OK SEEN LINES
LISTEN_OK=0
LISTEN_DETAIL="server did not start"
REACH_BEFORE=0
REACH_AFTER=0

run_probe() {
	local mode=$1
	PROBE_MARK[$mode]=$(audit_lines "$mode")
	envfile_create "$S1" TOK1 || return 1
	in_sbx "$C1" --env-file "$ENVF" -- /tmp/mcpspike probe \
		-other-url "http://$MCP_IP:$MCP_PORT/mcp/$S2" >"$RESULTS/probe-$mode.txt" 2>&1
	echo "exit code $?" >>"$RESULTS/probe-$mode.txt"
	envfile_remove
	PROBE_END[$mode]=$(audit_lines "$mode")
	audit_after "$mode" "${PROBE_MARK[$mode]}" | head -n "$((PROBE_END[$mode] - PROBE_MARK[$mode]))" >"$TMP/probe-audit-$mode.jsonl"
}

# client_info <name> <mode> <mark> notes protocol versions and sessions.
client_info() {
	local name=$1 mode=$2 mark=$3 slice
	slice=$(audit_after "$mode" "$mark")
	note "$name $mode: protocols [$(jq -r '.protocol // empty' <<<"$slice" | sort -u | tr '\n' ' ')]" \
		"methods [$(jq -r '"\(.method) \(.rpc_method // "-")"' <<<"$slice" | sort | uniq -c | awk '{printf "%s %s:%s, ", $2, $3, $1}')]" \
		"session-requests $(jq -s 'map(select(.session)) | length' <<<"$slice")," \
		"statuses [$(jq -r '.status' <<<"$slice" | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}')]"
}

# agent_ok <mode> <mark> <outfile> checks the audit and the agent output.
agent_ok() {
	local mode=$1 mark=$2 out=$3 calls
	calls=$(audit_after "$mode" "$mark" | jq -s --arg s "$S1" \
		'map(select(.rpc_method == "tools/call" and .tool == "sandbox_info" and .sandbox == $s and .decision == "allow" and .status == 200)) | length')
	echo "audited sandbox_info calls for s1: $calls" >>"$out"
	[[ $calls -ge 1 ]] && grep -qF "sandbox $S1" "$out"
}

run_claude() {
	local mode=$1 mark out=$RESULTS/claude-$mode.txt
	mark=$(audit_lines "$mode")
	envfile_create "$S1" TOK1 || return 1
	in_sbx "$C1" --env-file "$ENVF" \
		-e HOME=/home/agent \
		-e ANTHROPIC_BASE_URL="http://$MOCK_ADDR/anthropic" \
		-e ANTHROPIC_API_KEY=mock-key-not-a-secret \
		-e ENABLE_TOOL_SEARCH=false \
		-e DISABLE_AUTOUPDATER=1 \
		-e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
		-w /workspace -- \
		timeout "$AGENT_TIMEOUT" claude -p "$PROMPT" --mcp-config /workspace/.mcp.json --strict-mcp-config \
		--allowedTools mcp__wsm__sandbox_info </dev/null >"$out" 2>&1
	echo "exit code $?" >>"$out"
	envfile_remove
	if agent_ok "$mode" "$mark" "$out"; then CLAUDE_OK[$mode]=1; else CLAUDE_OK[$mode]=0; fi
	client_info "claude" "$mode" "$mark"
}

run_codex() {
	local mode=$1 mark out=$RESULTS/codex-$mode.txt
	mark=$(audit_lines "$mode")
	envfile_create "$S1" TOK1 || return 1
	in_sbx "$C1" --env-file "$ENVF" \
		-e HOME=/home/agent \
		-e MOCK_OPENAI_KEY=mock-key-not-a-secret \
		-w /workspace -- \
		timeout "$AGENT_TIMEOUT" codex exec --skip-git-repo-check "$PROMPT" </dev/null >"$out" 2>&1
	echo "exit code $?" >>"$out"
	envfile_remove
	if agent_ok "$mode" "$mark" "$out"; then CODEX_OK[$mode]=1; else CODEX_OK[$mode]=0; fi
	client_info "codex" "$mode" "$mark"
}

for mode in "${MODES[@]}"; do
	log "server $mode"
	if ! start_server "$mode" >"$RESULTS/server-start-$mode.log" 2>&1; then
		report "SETUP-SERVER-$mode" FAIL "see server-start-$mode.log"
		CLAUDE_OK[$mode]=0
		CODEX_OK[$mode]=0
		continue
	fi
	if [[ $mode == stateful ]]; then
		L=$RESULTS/MCP-LISTEN.log
		ss -ltnpH "sport = :$MCP_PORT" >"$L" 2>&1
		pid=$(grep -o 'pid=[0-9]*' "$L" | head -n1 | cut -d= -f2)
		user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
		echo "pid $pid user $user" >>"$L"
		if grep -q "$MCP_IP:$MCP_PORT" "$L" && [[ $user == wsm ]]; then
			LISTEN_OK=1
			LISTEN_DETAIL="listening on $MCP_IP:$MCP_PORT as $user (pid $pid)"
		else
			LISTEN_DETAIL="listener '$(head -n1 "$L")' user '$user'"
		fi
		REACH_BEFORE=$(counter mcp_allowed)
	fi
	log "probe ($mode)"
	run_probe "$mode"
	[[ $mode == stateful ]] && REACH_AFTER=$(counter mcp_allowed)
	log "Claude Code ($mode)"
	run_claude "$mode"
	log "Codex ($mode)"
	run_codex "$mode"
	stop_server "$mode"
	SEEN[$mode]=$(grep -o 'requests_seen=[0-9]*' "$STATE/server-$mode.log" | tail -n1 | cut -d= -f2)
	LINES[$mode]=$(audit_lines "$mode")
done
in_sbx "$C1" -- cat /tmp/mockllm.log >"$RESULTS/mockllm.log" 2>&1

# --- criteria ---------------------------------------------------------------

# probe_pass <check> is true when the probe passed <check> in every mode.
probe_pass() {
	local mode
	for mode in "${MODES[@]}"; do
		grep -q "^PROBE"$'\t'"$1"$'\t'"PASS"$'\t' "$RESULTS/probe-$mode.txt" 2>/dev/null || return 1
	done
}

# probe_audit <jq filter> is true when every mode's probe slice has a
# record that matches.
probe_audit() {
	local mode n
	for mode in "${MODES[@]}"; do
		n=$(jq -s "map(select($1)) | length" "$TMP/probe-audit-$mode.jsonl" 2>/dev/null)
		[[ ${n:-0} -ge 1 ]] || return 1
	done
}

for mode in "${MODES[@]}"; do
	cp "$TMP/probe-audit-$mode.jsonl" "$RESULTS/probe-audit-$mode.jsonl" 2>/dev/null
done

if ((LISTEN_OK)); then report MCP-LISTEN PASS "$LISTEN_DETAIL"; else report MCP-LISTEN FAIL "$LISTEN_DETAIL"; fi

if probe_pass reach && probe_pass identity && [[ ${REACH_AFTER:-0} -gt ${REACH_BEFORE:-0} ]]; then
	report MCP-REACH PASS "initialize and tools/list from s1 in both modes; mcp_allowed $REACH_BEFORE -> $REACH_AFTER"
else
	report MCP-REACH FAIL "probe reach/identity or counter (mcp_allowed $REACH_BEFORE -> $REACH_AFTER); see probe-*.txt"
fi

if probe_pass noauth && probe_audit ".status == 401 and .auth == \"none\" and .decision == \"deny\" and .path == \"/mcp/$S1\" and .sandbox == null"; then
	report MCP-NOAUTH PASS "401 without token, audited as deny"
else
	report MCP-NOAUTH FAIL "see probe-*.txt and probe-audit-*.jsonl"
fi

if probe_pass badtoken && probe_audit ".status == 401 and .auth == \"bearer\" and .decision == \"deny\" and .sandbox == null"; then
	report MCP-BADTOKEN PASS "401 with a random token, audited as deny"
else
	report MCP-BADTOKEN FAIL "see probe-*.txt and probe-audit-*.jsonl"
fi

if probe_pass cross && probe_audit ".status == 403 and .decision == \"deny\" and .sandbox == \"$S1\" and .path == \"/mcp/$S2\""; then
	report MCP-CROSS PASS "s1's token on s2's path: 403, audited with identity s1 and path of s2"
else
	report MCP-CROSS FAIL "see probe-*.txt and probe-audit-*.jsonl"
fi

if probe_pass no-id-args; then
	report MCP-NO-ID-ARGS PASS "no tool argument mentions project or sandbox"
else
	report MCP-NO-ID-ARGS FAIL "see probe-*.txt"
fi

if probe_pass deny && probe_audit ".rpc_method == \"tools/call\" and .tool == \"service_enable\" and .decision == \"deny\" and .sandbox == \"$S1\""; then
	report MCP-DENY PASS "service_enable: isError with a clear message, audited as deny"
else
	report MCP-DENY FAIL "see probe-*.txt and probe-audit-*.jsonl"
fi

for mode in "${MODES[@]}"; do
	MODE_UP=${mode^^}
	if [[ ${CLAUDE_OK[$mode]:-0} == 1 ]]; then
		report "MCP-CLAUDE-$MODE_UP" PASS "Claude Code called sandbox_info as s1 via the mock model; output names s1"
	else
		report "MCP-CLAUDE-$MODE_UP" FAIL "see claude-$mode.txt"
	fi
	if [[ ${CODEX_OK[$mode]:-0} == 1 ]]; then
		report "MCP-CODEX-$MODE_UP" PASS "Codex called sandbox_info as s1 via the mock model; output names s1"
	else
		report "MCP-CODEX-$MODE_UP" FAIL "see codex-$mode.txt"
	fi
done

complete=1
detail=""
for mode in "${MODES[@]}"; do
	detail+="$mode: seen ${SEEN[$mode]:-?} audited ${LINES[$mode]:-?}; "
	if [[ -z ${SEEN[$mode]:-} || ${SEEN[$mode]} != "${LINES[$mode]:-}" || ${SEEN[$mode]} -eq 0 ]]; then
		complete=0
	fi
done
if ((complete)); then report MCP-AUDIT-COMPLETE PASS "$detail"; else report MCP-AUDIT-COMPLETE FAIL "$detail"; fi

note "counters: mcp_allowed $(counter mcp_allowed), wsm0_blocked $(counter wsm0_blocked), loopback_blocked $(counter loopback_blocked)"

# Last, so it also covers everything written above.
L=$RESULTS/MCP-NO-SECRETS.log
hits=$(
	{
		cat "$STATE"/audit-*.jsonl "$STATE"/server-*.log
		find "$RESULTS" -type f ! -name MCP-NO-SECRETS.log -exec cat {} +
	} | grep -F -c -f "$TOKFILE"
)
echo "token hits in audit logs, server logs and results/: $hits" >"$L"
if [[ $hits == 0 ]]; then
	report MCP-NO-SECRETS PASS "no token in the audit logs, server logs or results/"
else
	report MCP-NO-SECRETS FAIL "$hits lines with a token"
fi

n=$(failures)
log "done: $n failed criteria; summary in $RESULTS/summary.txt"
((n == 0))
