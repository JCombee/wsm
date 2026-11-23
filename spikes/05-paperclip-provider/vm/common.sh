# Shared helpers for the spike 05 VM scripts. Source it; it refuses to run
# anywhere but a disposable VM, as root (guard from spikes/lib.sh).
# shellcheck shell=bash

VM_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SPIKE=$(cd "$VM_DIR/.." && pwd)
# shellcheck source=../../lib.sh
source "$SPIKE/../lib.sh"
guard

S=/home/test/s05
BIN=$S/bin
STATE=/root/s05
RESULTS=$STATE/results
mkdir -p "$RESULTS"
touch "$RESULTS/summary.txt"

PV=2026.1117.0
CLAUDE_VERSION=2.1.330
PLUGIN_ID=wsm.s05-sandbox-provider
PLUGIN_DIR=/home/pclip/s05-plugin
PROJECT=s05
API=http://127.0.0.1:3100/api
CANARY=/home/pclip/s05-canary.txt
PCENV=(HOME=/home/pclip PATH=/home/pclip/.npm-global/bin:/usr/local/bin:/usr/bin:/bin PAPERCLIP_TELEMETRY_DISABLED=1 DO_NOT_TRACK=1 LANG=C.UTF-8)

# as_pc runs a command as the Paperclip user, from /.
as_pc() { (cd / && runuser -u pclip -- env "${PCENV[@]}" "$@"); }

# wsmapi <path> [curl args...] calls the wsm management API as pclip.
wsmapi() {
	local p=$1
	shift
	as_pc curl -sS --unix-socket /run/wsm/api.sock "$@" "http://wsm/v1$p"
}

# res <ID> PASS|FAIL <detail> records a criterion, replacing an earlier line.
res() {
	local id=$1
	sed -i "/^${id}\t/d" "$RESULTS/summary.txt"
	report "$@"
}

# info <key> <text> records an INFO line, replacing an earlier one with the key.
info() {
	local key=$1
	shift
	sed -i "/^INFO\t${key}[ =]/d" "$RESULTS/summary.txt"
	note "$key $*"
}

ts() { date -u +%H:%M:%S; }
log() { printf '== [%s] %s\n' "$(ts)" "$*"; }

# pc_unit_pid prints the main PID of the Paperclip unit.
pc_unit_pid() { systemctl show -p MainPID --value s05-paperclip; }

# project_user prints the project's linux user.
project_user() { wsmapi /projects | jq -r --arg p "$PROJECT" '.[] | select(.slug==$p) | .linux_user'; }
