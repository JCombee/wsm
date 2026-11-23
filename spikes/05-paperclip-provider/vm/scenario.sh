#!/bin/sh
# Spike 05 scenario script. e2e.sh commits it into each Paperclip project's
# host repository as .s05/run.sh; the mock model runs it as its only Bash call,
# inside the sandbox, in the workspace root. It prints only KEY=value lines and
# never a token or another environment value.

body=/tmp/s05-body
# api <method> <path> [curl args...] prints the HTTP status of a bridge call.
api() {
	m=$1 p=$2
	shift 2
	curl -sS -m 30 -o "$body" -w '%{http_code}' -X "$m" -H "Authorization: Bearer $PAPERCLIP_API_KEY" "$@" "$PAPERCLIP_API_URL$p" 2>/dev/null
}
# net <name> <url>: a direct connection that must fail. curl's exit code tells
# a refused connection (7) or a timeout (28) from a connection that succeeded.
net() {
	code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$2" 2>/dev/null)
	rc=$?
	echo "net-$1=$code"
	echo "net-$1-rc=$rc"
}
json_field() { node -e 'try { const j = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.stdout.write(String(j[process.argv[2]] ?? "")); } catch {}' "$body" "$1"; }

ws_head=$(git rev-parse HEAD 2>/dev/null)
code=$(api GET "/api/issues/$PAPERCLIP_TASK_ID")
title=$(json_field title)
case $title in
*"[s05-file]"*) sc=file ;;
*"[s05-hostile-1]"*) sc=hostile-1 ;;
*"[s05-hostile-2]"*) sc=hostile-2 ;;
*"[s05-duplex]"*) sc=duplex ;;
*) sc=unknown ;;
esac
echo "scenario=$sc"
echo "ws-head=$ws_head"
echo "issue-get=$code"
echo "api-url-host=$(node -e 'try { console.log(new URL(process.argv[1]).host) } catch { console.log("none") }' "$PAPERCLIP_API_URL")"

bridge_checks() {
	echo "env-names=$(node -e 'console.log(Object.keys(process.env).sort().join(","))')"
	echo "env-multiline=$(node -e 'console.log(Object.keys(process.env).filter((k) => process.env[k].includes("\n")).sort().join(",") || "none")')"
	echo "bridge-me=$(api GET /api/agents/me)"
	echo "bridge-deny=$(api POST "/api/companies/$PAPERCLIP_COMPANY_ID/agents" -H 'Content-Type: application/json' -d '{"name":"s05-must-not-exist","adapterType":"claude_local"}')"
	echo "bridge-badtoken=$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' "$PAPERCLIP_API_URL/api/agents/me" 2>/dev/null)"
	echo "bridge-github=$(api POST /runtime-tools/github/credentials -H 'Content-Type: application/json' -d '{}')"
	host=$(cat .s05/host 2>/dev/null)
	net 169.254.1.2-3100 http://169.254.1.2:3100/api/health
	net host.containers.internal-3100 http://host.containers.internal:3100/api/health
	net 169.254.1.2-54329 http://169.254.1.2:54329/
	net "$host-3100" "http://$host:3100/api/health"
	net "$host-3199" "http://$host:3199/"
	net 127.0.0.1-3100 http://127.0.0.1:3100/api/health
}

commit_spike() {
	printf 'hello from the sandbox (%s)\n' "$sc" >SPIKE.txt
	git add SPIKE.txt &&
		git -c user.name=mock-agent -c user.email=mock@example.invalid commit -q -m "S1 spike commit from the sandbox ($sc)"
	echo "commit=$(git rev-parse HEAD)"
}

hostile_1() {
	# A committed symlink to a host file only pclip can read.
	ln -sfn /home/pclip/s05-canary.txt leak
	git add leak && git -c user.name=mock-agent -c user.email=mock@example.invalid commit -q -m "S1 hostile: leak symlink"
	echo "commit=$(git rev-parse HEAD)"
	# Uncommitted symlinks that point out of the workspace.
	ln -sfn /etc/hostname escape-abs
	ln -sfn ../../../../../../../../etc/hostname escape-rel
	mkdir -p sub && ln -sfn / sub/root
	# Hooks and git config that would run code on the host.
	mkdir -p .git/hooks .s05/hooks
	for h in post-checkout post-merge pre-commit post-rewrite reference-transaction; do
		printf '#!/bin/sh\ntouch /tmp/s05-hook-%s\n' "$h" >".git/hooks/$h"
		cp ".git/hooks/$h" ".s05/hooks/$h"
		chmod +x ".git/hooks/$h" ".s05/hooks/$h"
	done
	git config core.fsmonitor 'touch /tmp/s05-fsmonitor'
	git config core.hooksPath .s05/hooks
	echo "planted=leak,escape-abs,escape-rel,sub/root,hooks,fsmonitor,hooksPath"
}

hostile_2() {
	if [ -L leak ]; then echo "leak-present=symlink"; elif [ -e leak ]; then echo "leak-present=file"; else echo "leak-present=no"; fi
	if c=$(cat leak 2>/dev/null); then
		echo "leak-read=$(printf %s "$c" | sha256sum | cut -c1-64)"
	else
		echo "leak-read=ERR"
	fi
}

case $sc in
file | duplex)
	bridge_checks
	commit_spike
	;;
hostile-1) hostile_1 ;;
hostile-2) hostile_2 ;;
esac

echo "issue-patch=$(api PATCH "/api/issues/$PAPERCLIP_TASK_ID" -H "X-Paperclip-Run-Id: $PAPERCLIP_RUN_ID" -H 'Content-Type: application/json' -d "{\"status\":\"done\",\"comment\":\"S1 spike: scenario $sc done.\"}")"
echo "s05-end=1"
