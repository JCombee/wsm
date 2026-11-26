# Shared helpers for the spike 07 VM scripts. Source it; it refuses to run
# anywhere but a disposable VM, as root (guard from spikes/lib.sh).
# shellcheck shell=bash

VM_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SPIKE=$(cd "$VM_DIR/.." && pwd)
# shellcheck source=../../lib.sh
source "$SPIKE/../lib.sh"
guard

S=/home/test/s07
BIN=$S/bin
TS=/usr/local/bin/tenantspike
STATE=/root/s07
RESULTS=$STATE/results
RAWDIR=$STATE/raw
mkdir -p "$RESULTS" "$RAWDIR"
chmod 0700 "$STATE" "$RAWDIR"
touch "$RESULTS/summary.txt"

PV=2026.1117.0
SANDBOX_NODE=v24.22.3
PROJECT=s07
# The tenant server: a stand-in for the manager on this disposable VM only.
UNIT=s07-tenant
CONF=/etc/s07/tenants.json
AUDIT=/var/lib/s07/audit.jsonl
SOCKDIR=/run/s07/tenant/$PROJECT
# The control room: its own user, container, volume and image.
CRU=s07-cr
CRUID=61101
CTR=s07-cr
VOL=s07-cr-data
IMG=localhost/s07-control-room:latest
BASE_REPO=docker.io/library/node
BASE_TAG=24-trixie-slim
# The base is pinned: node:24-trixie-slim as pulled on 24 November 2026.
BASE_DIGEST=sha256:6709bf854652e2d21610e952df82ff9c0b98cc7315acdce75236bbb66bd8aa06
# The plugin is an image-owned bundle in Paperclip's distribution catalog
# (image/catalog.mjs); the build's test output sits next to it.
PLUGIN_ROOT=/opt/wsm-plugins
PLUGIN_DIR=$PLUGIN_ROOT/distribution/wsm
BUILD_INFO=/opt/wsm-build
PLUGIN_ID=wsm.s07-sandbox-provider
API=http://127.0.0.1:3100/api
# Names the tenant API strips from every exec's environment (env_deny). The
# GitHub tokens always go (B4); the agent keys and CLAUDE_CODE_MESSAGING_TOKEN
# go because a run does not need them (step 4 checks that). The last six are
# the control room's own secrets (cr.env): Paperclip passes its whole process
# environment into some sandbox execs (two `sh -c` per run, right before the
# claude CLI starts, and in a new agent's hello probe), measured on
# 26 November, so the ceiling strips them by name.
ENV_DENY=(GH_TOKEN GITHUB_TOKEN PAPERCLIP_GIT_TOKEN PAPERCLIP_GITHUB_BROKER_TOKEN
	PAPERCLIP_AGENT_PRIVATE_KEY PAPERCLIP_AGENT_PUBLIC_KEY CLAUDE_CODE_MESSAGING_TOKEN
	DATABASE_URL PAPERCLIP_SECRETS_MASTER_KEY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
	PAPERCLIP_CLOUD_TENANT_SERVER_TOKEN BETTER_AUTH_SECRET)
# Egress: the one allowed destination.
ALLOW_IP=1.1.1.1
ALLOW_PORT=443
# Containment through Paperclip's own harness contract, set by the owner like
# the rest of the ceiling, from the first start (source-checked against
# 2026.1117.0, see FINDINGS.md):
# - plugins.autoInstall: Paperclip installs the image's wsm bundle itself; a
#   managed instance activates only auto-installed bundles and loads nothing
#   from npm or the file system.
# - environments: the instance's one managed sandbox environment, provider
#   wsm (its provider must be in autoInstall, or Paperclip refuses to start).
# - enableManagedSandboxOnly: a run that would land on the local environment
#   goes to that environment, or fails; a board user cannot switch it off.
# - The hidden setting answers every plugin management route with 403.
MANAGED_CONFIG='{"v":1,"mode":"cloud","catalogVersion":"2026.1117.0","features":{"enableManagedSandboxOnly":true,"enableOwnerInstanceAdmin":true},"plugins":{"autoInstall":["wsm"]},"environments":[{"name":"wsm","provider":"wsm","config":{}}]}'
HIDDEN_SETTINGS=instance.plugins
# The managed config makes Paperclip a cloud instance, which refuses its
# embedded database and local file storage: it needs DATABASE_URL, an object
# store for files and run logs, and PAPERCLIP_SECRETS_MASTER_KEY in the
# environment (managed-cloud-prerequisites.ts). So the control room is a pod:
# Paperclip, PostgreSQL (the binaries Paperclip's image already carries) and
# an object store, all on the pod's own loopback, each with its own named
# volume. Only Paperclip's port is published, on the host's loopback.
POD=s07-cr-pod
PG_CTR=s07-cr-pg
PG_VOL=s07-cr-pg
PG_BIN=/opt/paperclip/lib/node_modules/paperclipai/node_modules/@embedded-postgres/linux-x64/native/bin
S3_CTR=s07-cr-s3
S3_VOL=s07-cr-s3v
S3_BUCKET=paperclip
S3_REGION=us-east-1
# The object store must enforce conditional writes: Paperclip's run-log
# journal publishes with If-None-Match: * and If-Match, and probes the store
# before the first run log (object-byte-journal.ts); a store that ignores them
# fails every run ("Object storage does not enforce conditional publication").
# Garage v2.5.0 honours them only on GET and copy sources, so it failed.
# Versity S3 Gateway's posix backend enforces both under a per-object flock
# (its default lock mode). v1.11.0 (6 November 2026), pinned by index digest.
S3_IMG=docker.io/versity/versitygw@sha256:8319a530d08d537426e2f855bae5280709795dc73a3f0c347973ef915d6343c5
# Paperclip's S3 settings, also used by the bucket probe (s3_probe).
S3_ENV=(-e PAPERCLIP_STORAGE_PROVIDER=s3 -e "PAPERCLIP_STORAGE_S3_BUCKET=$S3_BUCKET" -e "PAPERCLIP_STORAGE_S3_REGION=$S3_REGION"
	-e PAPERCLIP_STORAGE_S3_ENDPOINT=http://127.0.0.1:3900 -e PAPERCLIP_STORAGE_S3_FORCE_PATH_STYLE=true
	-e AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED -e AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED)
# A cloud instance creates no company through the API (403 cloud_managed);
# Paperclip Cloud provisions it through trusted headers, which Paperclip reads
# only in authenticated mode. So the control room runs authenticated with
# public exposure, like a Cloud stack, and wsm acts as its operator: every API
# call carries the tenant server token and the owner's identity, from a
# root-only header file (curl -H @file, never argv). The first such call
# seeds the stack's company. Public exposure also makes Paperclip refuse
# local stdio MCP servers. (In local_trusted, Paperclip must listen on
# loopback, and pasta 2025_05 forwards a published port only to the pod's own
# address, never its loopback, so the port was unreachable from the host.)
HEADERS=$STATE/cloud-headers
STACK_ID=s07
# The pod's own secrets: root-only in $STATE, and env files that podman reads
# as the control room's user (never argv, which every host user can read).
CR_SECRETS=$STATE/cr-secrets.env
ENVDIR=/home/$CRU/env

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

# cr <podman args...> runs podman as the control room's user.
cr() { as_user "$CRU" podman "$@"; }
# crx <cmd...> runs a command in the control room container, as its user.
crx() { cr exec -i "$CTR" "$@"; }

wsmapi() { curl -sS --unix-socket /run/wsm/api.sock "http://wsm/v1$1"; }
project_field() { wsmapi /projects | jq -r --arg p "$PROJECT" --arg f "$1" '.[] | select(.slug==$p) | .[$f]'; }

# ctr_mem <container> prints "<current>/<peak>" MiB from its cgroup, or none.
ctr_mem() {
	local cg
	cg=$(cr inspect -f '{{.State.CgroupPath}}' "$1" 2>/dev/null)
	if [[ -n $cg && -r /sys/fs/cgroup$cg/memory.current ]]; then
		printf '%s/%s' "$(($(cat "/sys/fs/cgroup$cg/memory.current") >> 20))" \
			"$(($(cat "/sys/fs/cgroup$cg/memory.peak" 2>/dev/null || echo 0) >> 20))"
	else
		printf none
	fi
}

# usage_line prints memory, disk and each pod container's cgroup memory
# (current/peak MiB).
usage_line() {
	printf 'mem_used_mib=%s mem_avail_mib=%s disk_used=%s disk_free=%s cr_mib=%s pg_mib=%s s3_mib=%s' \
		"$(free -m | awk 'NR==2{print $3}')" "$(free -m | awk 'NR==2{print $7}')" \
		"$(df -h / | awk 'NR==2{print $3}')" "$(df -h / | awk 'NR==2{print $4}')" \
		"$(ctr_mem "$CTR")" "$(ctr_mem "$PG_CTR")" "$(ctr_mem "$S3_CTR")"
}

# pc <curl args...> calls Paperclip as the operator (trusted Cloud headers).
pc() { curl -H "@$HEADERS" "$@"; }

# company_id prints the stack's company, which Paperclip seeds on the first
# trusted-header request.
company_id() { pc -sS "$API/companies" | jq -r 'if type == "array" then (.[0].id // empty) else empty end'; }

# usage <phase> appends a usage line for the phase to results/usage.txt.
usage() { printf '%s %s %s\n' "$(ts)" "$1" "$(usage_line)" >>"$RESULTS/usage.txt"; }

# pc_health prints the health JSON, or nothing.
pc_health() { curl -fsS -m 5 "$API/health" 2>/dev/null; }

# plugin_record prints Paperclip's record of our plugin as JSON.
plugin_record() {
	pc -fsS "$API/plugins" 2>/dev/null |
		jq -c --arg id "$PLUGIN_ID" '[.. | objects | select((.pluginKey? // .key? // "") == $id)] | first // empty'
}

# call <METHOD> <path> [json-file]: the body goes to $RAW/resp.json; prints
# the HTTP status. The caller sets RAW.
call() {
	local m=$1 p=$2 f=${3:-}
	if [[ -n $f ]]; then
		pc -sS -m 120 -o "$RAW/resp.json" -w '%{http_code}' -X "$m" "$API$p" -H 'content-type: application/json' --data-binary "@$f"
	else
		pc -sS -m 120 -o "$RAW/resp.json" -w '%{http_code}' -X "$m" "$API$p"
	fi
}
# jcall <METHOD> <path> <json>: like call, with an inline body (never a secret).
jcall() {
	printf '%s' "$3" >"$RAW/req.json"
	call "$1" "$2" "$RAW/req.json"
}
# resp_brief prints a short, printable summary of the last response.
resp_brief() { tr -c '[:print:]' ' ' <"$RAW/resp.json" | cut -c1-${1:-200}; }

# cr_log_since <epoch> prints the control room's log lines since then.
cr_log_since() { cr logs --since "$(date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ)" "$CTR" 2>&1; }
