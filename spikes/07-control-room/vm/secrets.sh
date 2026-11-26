#!/usr/bin/env bash
# secrets.sh <out-file>: collects the control room's own secrets from its data
# directory, for the watcher and the leak checks. Each value of at least 16
# characters goes to <out-file> (0600, one per line); stdout gets only where
# it was found (file and key name), never a value.
#
# Sources: .env files (NAME=VALUE with SECRET, PASSWORD, TOKEN, KEY or JWT in
# the name), JSON config (keys that look secret), and key or PEM files (one
# base64 line, which also appears verbatim in any env value holding the PEM).
set -uo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

out=$1
tmp=$STATE/secrets.tmp
(umask 077 && : >"$tmp")
crx sh -c 'find /paperclip/pc -maxdepth 7 -type f -size -64k \( -name ".env" -o -name "*.env" -o -name "*.key" -o -name "*.pem" -o -iname "*secret*" -o -name "*.json" \) 2>/dev/null' |
	grep -vE '/(node_modules|db|pgdata|data/postgres|logs?|plugins)/' >"$STATE/secret-files"
while IFS= read -r f; do
	case $f in
	*.json)
		crx cat "$f" 2>/dev/null | jq -r 'paths(scalars) as $p | select(($p | map(tostring) | join(".")) | test("secret|password|passwd|token|apikey|api_key|jwt|privatekey|private_key|masterkey|master_key"; "i"))
			| "\($p | map(tostring) | join("."))\t\(getpath($p) | tostring)"' 2>/dev/null
		;;
	*.env | */.env)
		crx cat "$f" 2>/dev/null | grep -E '^[A-Za-z0-9_]*(SECRET|PASSWORD|TOKEN|KEY|JWT)[A-Za-z0-9_]*=' | sed 's/=/\t/'
		;;
	*)
		printf '%s\t%s\n' "${f##*/}" "$(crx cat "$f" 2>/dev/null | grep -v -- '-----' | grep -m1 -E '^[A-Za-z0-9+/=_-]{16,}$')"
		;;
	esac | while IFS=$'\t' read -r name value; do
		value=${value%\"}
		value=${value#\"}
		if ((${#value} >= 16)); then
			printf '%s\n' "$value" >>"$tmp"
			echo "$f: $name"
		fi
	done
done <"$STATE/secret-files"
# A cloud instance takes its database password, master key and object store
# key from the environment (run.sh cr_secrets), not from files.
if [[ -s $CR_SECRETS ]]; then
	while IFS='=' read -r name value; do
		if ((${#value} >= 16)); then
			printf '%s\n' "$value" >>"$tmp"
			echo "$CR_SECRETS: $name"
		fi
	done <"$CR_SECRETS"
fi
(umask 077 && sort -u "$tmp" | sed '/^$/d' >"$out")
rm -f "$tmp"
echo "values=$(grep -c . "$out")"
