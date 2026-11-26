#!/bin/sh
# Spike 07 scenario script. e2e.sh commits it into each Paperclip project's
# repository in the control room as .s05/run.sh: the S1 image's mock model runs
# that path as its only Bash call, inside the sandbox, in the workspace root.
# It prints only KEY=value lines: names, codes and hashes, never a value.

body=/tmp/s07-body
# api <method> <path> [curl args...] prints the HTTP status of a bridge call.
api() {
	m=$1 p=$2
	shift 2
	curl -sS -m 30 -o "$body" -w '%{http_code}' -X "$m" -H "Authorization: Bearer $PAPERCLIP_API_KEY" "$@" "$PAPERCLIP_API_URL$p" 2>/dev/null
}
json_field() { node -e 'try { const j = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.stdout.write(String(j[process.argv[2]] ?? "")); } catch {}' "$body" "$1"; }

ws_head=$(git rev-parse HEAD 2>/dev/null)
code=$(api GET "/api/issues/$PAPERCLIP_TASK_ID")
title=$(json_field title)
case $title in
*"[s07-file]"*) sc=file ;;
*"[s07-duplex]"*) sc=duplex ;;
*"[s07-symlink-1]"*) sc=symlink-1 ;;
*"[s07-symlink-2]"*) sc=symlink-2 ;;
*) sc=unknown ;;
esac
echo "scenario=$sc"
echo "ws-head=$ws_head"
echo "issue-get=$code"
echo "env-names=$(node -e 'console.log(Object.keys(process.env).sort().join(","))')"
echo "env-count=$(node -e 'console.log(Object.keys(process.env).length)')"

# env-chain: this script's ancestors up to the process the exec started, each
# with the env_deny names in its own initial environment (names only), or
# "-". It tells a name that came with the exec from one that a process inside
# the sandbox set for its children.
deny="GH_TOKEN GITHUB_TOKEN PAPERCLIP_GIT_TOKEN PAPERCLIP_GITHUB_BROKER_TOKEN PAPERCLIP_AGENT_PRIVATE_KEY PAPERCLIP_AGENT_PUBLIC_KEY CLAUDE_CODE_MESSAGING_TOKEN DATABASE_URL PAPERCLIP_SECRETS_MASTER_KEY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY PAPERCLIP_CLOUD_TENANT_SERVER_TOKEN BETTER_AUTH_SECRET"
chain= p=$$
while [ "${p:-0}" -gt 0 ]; do
	names=$(tr '\0' '\n' 2>/dev/null <"/proc/$p/environ" | sed -n 's/=.*//p')
	hit=
	for n in $deny; do
		printf '%s\n' "$names" | grep -qx "$n" && hit="$hit${hit:++}$n"
	done
	chain="$chain${chain:+,}$(cat "/proc/$p/comm" 2>/dev/null):${hit:--}"
	p=$(sed -n 's/^PPid:[[:space:]]*//p' "/proc/$p/status" 2>/dev/null)
done
echo "env-chain=$chain"

commit_spike() {
	printf 'hello from the sandbox (%s)\n' "$sc" >SPIKE.txt
	git add SPIKE.txt &&
		git -c user.name=mock-agent -c user.email=mock@example.invalid commit -q -m "S2 spike commit from the sandbox ($sc)"
	echo "commit=$(git rev-parse HEAD)"
}

# symlink_1 commits only a symlink to a file in the control room's data
# volume. The sandbox has no such file; the question is where it ends up.
symlink_1() {
	ln -sfn /paperclip/s07-canary.txt leak
	git add leak && git -c user.name=mock-agent -c user.email=mock@example.invalid commit -q -m "S2 symlink into the control room"
	echo "commit=$(git rev-parse HEAD)"
}

# symlink_2 reports what the next sync-in brought: no content, only a hash.
symlink_2() {
	if [ -L leak ]; then
		echo "leak-present=symlink"
		echo "leak-target=$(readlink leak)"
	elif [ -e leak ]; then
		echo "leak-present=file"
	else
		echo "leak-present=no"
	fi
	if c=$(cat leak 2>/dev/null); then
		echo "leak-read=$(printf %s "$c" | sha256sum | cut -c1-64)"
	else
		echo "leak-read=ERR"
	fi
	[ -e /paperclip ] && echo "leak-cr-path=present" || echo "leak-cr-path=absent"
}

case $sc in
file | duplex)
	echo "bridge-me=$(api GET /api/agents/me)"
	commit_spike
	;;
symlink-1) symlink_1 ;;
symlink-2) symlink_2 ;;
esac

echo "issue-patch=$(api PATCH "/api/issues/$PAPERCLIP_TASK_ID" -H "X-Paperclip-Run-Id: $PAPERCLIP_RUN_ID" -H 'Content-Type: application/json' -d "{\"status\":\"done\",\"comment\":\"S2 spike: scenario $sc done.\"}")"
echo "s07-end=1"
