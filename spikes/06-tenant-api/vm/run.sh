#!/usr/bin/env bash
# Spike 06 (S3): a prototype of the tenant API on a disposable VM, as root.
# Every phase is idempotent.
#
#   bash run.sh setup|identity|api|exec|restart|audit|collect|all
#
# setup     wsm, the spike users, projects s06a and s06b, the server, client images
# identity  SO_PEERCRED through rootless Podman: keep-id, subuid, other user, root
# api       own sandboxes, another tenant's ID, the ceiling, strict specs, fail closed
# exec      exec streams, duplex, transfer, while scanners watch host argv
# restart   what a server restart does to a running control room's socket mount
# audit     every request in the audit, no sentinels, the hash chain
# collect   results and logs into /home/test/s06-out for download
#
# The server user s06tenant is in the groups wsm and wsm-admin: a stand-in for
# the manager on this disposable VM only. Never set this up anywhere else.
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SPIKE=$(cd "$HERE/.." && pwd)
# shellcheck source=../../lib.sh
source "$SPIKE/../lib.sh"
guard

S=/home/test/s06
BIN=$S/bin
TS=/usr/local/bin/tenantspike
STATE=/root/s06
RESULTS=$STATE/results
AUDIT=/var/lib/s06/audit.jsonl
CONF=/etc/s06/tenants.json
UNIT=s06-tenant
PA=s06a
PB=s06b
CA=s06-ca
CB=s06-cb
UIDA=61001
UIDB=61002
SENTINELS=$STATE/sentinels
mkdir -p "$RESULTS"
chmod 0700 "$STATE"
touch "$RESULTS/summary.txt"

# res <ID> PASS|FAIL <detail> records a criterion, replacing an earlier line.
res() {
	local id=$1
	sed -i "/^${id}\t/d" "$RESULTS/summary.txt"
	report "$@"
}

# info <key> <text> records an INFO line, replacing an earlier one.
info() {
	local key=$1
	shift
	sed -i "/^INFO\t${key}[ =]/d" "$RESULTS/summary.txt"
	note "$key $*"
}

log() { printf '== [%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

wsmapi() { curl -sS --unix-socket /run/wsm/api.sock "http://wsm/v1$1"; }

CLIENT_FLAGS=(--rm --cap-drop=ALL --security-opt=no-new-privileges --read-only --network=none
	--memory=256m --pids-limit=64 --passwd=false)

# client <user> <socket of project> keepid|subuid|root <client args...> runs
# the control room's client in a container of <user>, with only that
# project's tenant socket mounted.
client() {
	local user=$1 project=$2 mode=$3
	shift 3
	local idflags=(--userns=keep-id)
	case $mode in
	subuid) idflags=(--user=1000:1000) ;;
	root) idflags=() ;;
	esac
	as_user "$user" podman run "${CLIENT_FLAGS[@]}" "${idflags[@]}" \
		-e S06_SENTINEL_ENV -e S06_SENTINEL_ARGV \
		-v "/run/s06/tenant/$project/tenant.sock:/run/wsm/tenant.sock" \
		localhost/s06-client:latest client "$@"
}

# record <file> copies the client's result and info lines into the summary.
record() {
	local f=$1 id st detail k d
	while IFS=$'\t' read -r id st detail; do
		[[ -n $id ]] && res "$id" "$st" "$detail"
	done < <(jq -r 'select(.kind=="result") | [.id, .status, .detail] | @tsv' "$f" 2>/dev/null)
	while IFS=$'\t' read -r k d; do
		[[ -n $k ]] && info "$k" "$d"
	done < <(jq -r 'select(.kind=="info") | [.key, .detail] | @tsv' "$f" 2>/dev/null)
}

# override <ID> <detail> turns a PASS into a FAIL when a host-side check fails.
override() {
	local id=$1 old
	old=$(awk -F'\t' -v id="$id" '$1==id {print $3}' "$RESULTS/summary.txt")
	res "$id" FAIL "$2; client said: $old"
}

project_field() { wsmapi /projects | jq -r --arg p "$1" --arg f "$2" '.[] | select(.slug==$p) | .[$f]'; }

start_server() {
	local i
	systemctl stop "$UNIT" 2>/dev/null
	systemctl reset-failed "$UNIT" 2>/dev/null
	systemd-run --unit="$UNIT" --uid=s06tenant --gid=s06tenant -p UMask=0077 "$TS" serve -config "$CONF" >/dev/null
	for i in $(seq 1 50); do
		[[ -S /run/s06/tenant/$PA/tenant.sock && -S /run/s06/tenant/$PB/tenant.sock ]] && return 0
		sleep 0.2
	done
	echo "server did not open its sockets" >&2
	journalctl -u "$UNIT" --no-pager | tail -5
}

build_client() {
	local user=$1 uid home ctx gid
	uid=$(id -u "$user")
	gid=$(id -g "$user")
	home=$(getent passwd "$user" | cut -d: -f6)
	ctx=$home/s06ctx
	rm -rf "$ctx"
	install -d -m 0755 -o "$user" -g "$gid" "$ctx"
	install -m 0755 -o "$user" -g "$gid" "$TS" "$ctx/tenantspike"
	printf 'root:x:0:0:root:/:/sbin/nologin\n%s:x:%d:%d::/:/sbin/nologin\n' "$user" "$uid" "$gid" >"$ctx/passwd"
	printf 'root:x:0:\n%s:x:%d:\n' "$user" "$gid" >"$ctx/group"
	printf 'FROM scratch\nCOPY tenantspike /tenantspike\nCOPY passwd /etc/passwd\nCOPY group /etc/group\nENTRYPOINT ["/tenantspike"]\n' >"$ctx/Containerfile"
	chown "$user:$gid" "$ctx"/*
	as_user "$user" podman build -q -t localhost/s06-client:latest -f "$ctx/Containerfile" "$ctx" >/dev/null
}

phase_setup() {
	local src t code p
	src=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')
	case $src in
	198.51.100.*) ;;
	*)
		echo "refusing: source address $src is not in 198.51.100.0/24" >&2
		exit 3
		;;
	esac
	for t in 192.168.0.1 192.168.0.137; do
		if timeout 3 bash -c "</dev/tcp/$t/22" 2>/dev/null; then
			echo "refusing: LAN host $t:22 is reachable" >&2
			exit 3
		fi
	done
	info address "$src"
	info platform "cpus=$(nproc) $(free -m | awk 'NR==2{print "mem_mib="$2}') disk_free=$(df -h / | awk 'NR==2{print $4}') kernel=$(uname -r)"

	log "packages"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q >"$STATE/apt.log" 2>&1
	apt-get install -y -q curl jq ca-certificates iproute2 e2fsprogs >>"$STATE/apt.log" 2>&1 || {
		res SETUP-WSM FAIL "apt-get install failed"
		exit 1
	}

	log "wsm"
	chmod +x "$BIN"/* "$SPIKE/../../deploy/install.sh"
	t=$(date +%s)
	"$SPIKE/../../deploy/install.sh" --bin "$BIN" >"$STATE/wsm-install.log" 2>&1
	code=$?
	info wsm-install "exit=$code in $(($(date +%s) - t))s"
	((code == 0)) || tail -20 "$STATE/wsm-install.log"
	install -m 0755 "$S/tenantspike" "$TS"

	log "users"
	id s06tenant >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin s06tenant
	# Spike-only stand-in for the manager: never do this in production.
	usermod -aG wsm,wsm-admin s06tenant
	ensure_cgroup_delegation
	ensure_rootless_user "$CA" "$UIDA" || res SETUP-TENANT FAIL "user $CA"
	ensure_rootless_user "$CB" "$UIDB" || res SETUP-TENANT FAIL "user $CB"

	log "projects"
	cat >"$STATE/Containerfile" <<'EOF'
# Spike 06 sandbox image: Debian with tar and coreutils, user 1000.
FROM docker.io/library/debian:trixie-slim
RUN groupadd --gid 1000 agent \
 && useradd --uid 1000 --gid 1000 --home-dir /home/agent --create-home --shell /bin/bash agent \
 && install -d -o 1000 -g 1000 /workspace
WORKDIR /workspace
CMD ["sleep", "infinity"]
EOF
	for p in "$PA" "$PB"; do
		if ! wsm project list 2>/dev/null | grep -qw "$p"; then
			wsm project create --slug "$p" --containerfile "$STATE/Containerfile" >"$STATE/project-$p.log" 2>&1 ||
				tail -5 "$STATE/project-$p.log"
		fi
		t=$(date +%s)
		wsm project image "$p" >>"$STATE/project-$p.log" 2>&1
		info "image-$p" "exit=$? in $(($(date +%s) - t))s"
	done

	log "tenant sockets, config and audit"
	install -d -m 0755 -o root -g root /run/s06 /run/s06/tenant /etc/s06
	install -d -m 0750 -o s06tenant -g "$(id -gn "$CA")" "/run/s06/tenant/$PA"
	install -d -m 0750 -o s06tenant -g "$(id -gn "$CB")" "/run/s06/tenant/$PB"
	install -d -m 0700 -o s06tenant -g s06tenant /var/lib/s06
	[[ -e $AUDIT ]] || install -m 0600 -o s06tenant -g s06tenant /dev/null "$AUDIT"
	jq -n --arg a "$PA" --arg b "$PB" --argjson ua "$UIDA" --argjson ub "$UIDB" '{
		audit_path: "/var/lib/s06/audit.jsonl", admin_socket: "/run/wsm/api.sock", podman_socket_dir: "/run/wsm-podman",
		tenants: [
			{project: $a, control_room_uid: $ua, socket_dir: ("/run/s06/tenant/" + $a), max_sandboxes: 2},
			{project: $b, control_room_uid: $ub, socket_dir: ("/run/s06/tenant/" + $b), max_sandboxes: 2}]}' >"$CONF"
	chmod 0644 "$CONF"
	start_server

	log "client images"
	build_client "$CA" || res SETUP-TENANT FAIL "client image for $CA"
	build_client "$CB" || res SETUP-TENANT FAIL "client image for $CB"
	check_setup
}

check_setup() {
	local ok=1 notes=() p lu img st pid user dirs socks tcp
	systemctl is-active --quiet wsm || {
		ok=0
		notes+=("wsm not active")
	}
	for p in "$PA" "$PB"; do
		lu=$(project_field "$p" linux_user)
		img=$(project_field "$p" image)
		if [[ -z $lu || -z $img ]] || ! podman --url "unix:///run/wsm-podman/$lu/podman.sock" image exists "$img" 2>/dev/null; then
			ok=0
			notes+=("$p: no image ($lu $img)")
		fi
	done
	st=$(cd / && runuser -u s06tenant -- curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/wsm/api.sock http://wsm/v1/projects)
	[[ $st == 200 ]] || {
		ok=0
		notes+=("GET /v1/projects as s06tenant: $st")
	}
	if ((ok)); then res SETUP-WSM PASS "wsm active, images of $PA and $PB present, management API as s06tenant: $st"; else res SETUP-WSM FAIL "${notes[*]}"; fi

	ok=1
	notes=()
	pid=$(systemctl show -p MainPID --value "$UNIT")
	user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
	[[ $user == s06tenant ]] || {
		ok=0
		notes+=("server user '$user'")
	}
	dirs="$(stat -c '%U:%G:%a' "/run/s06/tenant/$PA") $(stat -c '%U:%G:%a' "/run/s06/tenant/$PB")"
	[[ $dirs == "s06tenant:$(id -gn "$CA"):750 s06tenant:$(id -gn "$CB"):750" ]] || {
		ok=0
		notes+=("dirs $dirs")
	}
	socks="$(stat -c '%F:%a' "/run/s06/tenant/$PA/tenant.sock") $(stat -c '%F:%a' "/run/s06/tenant/$PB/tenant.sock")"
	[[ $socks == "socket:666 socket:666" ]] || {
		ok=0
		notes+=("sockets $socks")
	}
	tcp=$(ss -Hltnp | grep -c tenantspike)
	((tcp == 0)) || {
		ok=0
		notes+=("tcp listeners $tcp")
	}
	if ((ok)); then res SETUP-TENANT PASS "server as $user (groups $(id -Gn s06tenant | tr ' ' ',')), dirs $dirs, sockets $socks, tcp listeners $tcp"; else res SETUP-TENANT FAIL "${notes[*]}"; fi
}

phase_identity() {
	local st proj uid cg codes start ok out err sub
	log "identity: keep-id"
	client "$CA" "$PA" keepid whoami >"$RESULTS/id-keepid.jsonl" 2>"$RESULTS/id-keepid.err"
	read -r st proj uid < <(jq -r 'select(.kind=="whoami") | "\(.status) \(.body.project // "-") \(.body.peer_uid // "-")"' "$RESULTS/id-keepid.jsonl")
	cg=$(jq -r 'select(.kind=="whoami") | .body.peer_cgroup // ""' "$RESULTS/id-keepid.jsonl")
	if [[ $st == 200 && $proj == "$PA" && $uid == "$UIDA" ]]; then
		res ID-KEEPID PASS "whoami 200, project $proj, peer uid $uid"
	else
		res ID-KEEPID FAIL "whoami $st project $proj uid $uid; $(head -c 300 "$RESULTS/id-keepid.err")"
	fi
	info peer-cgroup "$cg"

	log "identity: subuid"
	client "$CA" "$PA" subuid probe-denied >"$RESULTS/id-subuid.jsonl" 2>"$RESULTS/id-subuid.err"
	codes=$(jq -c 'select(.kind=="probe") | .statuses' "$RESULTS/id-subuid.jsonl")
	start=$(awk -F: -v u="$CA" '$1==u {print $2}' /etc/subuid)
	sub=$(jq -r --argjson s "$start" --argjson ua "$UIDA" 'select(.decision=="deny" and .peer_uid >= $s and .peer_uid < ($s + 65536) and .peer_uid != $ua) | .peer_uid' "$AUDIT" | sort -u | head -3 | tr '\n' ' ')
	if [[ $codes == "[403,403,403,403,403]" && -n $sub ]]; then
		res ID-SUBUID PASS "statuses $codes; audit deny lines with peer uid $sub(subuid range of $CA starts at $start)"
	else
		res ID-SUBUID FAIL "statuses ${codes:-none}; subuid denies in audit: '${sub}'; $(head -c 300 "$RESULTS/id-subuid.err")"
	fi

	log "identity: other user"
	ok=1
	out=$(client "$CB" "$PA" keepid whoami 2>&1)
	if [[ $? -eq 0 ]] || ! grep -qi 'permission denied' <<<"$out"; then ok=0; fi
	err=$(cd / && runuser -u "$CB" -- "$TS" client whoami -socket "/run/s06/tenant/$PA/tenant.sock" 2>&1 | jq -r 'select(.kind=="whoami") | .error // .status')
	grep -qi 'permission denied' <<<"$err" || ok=0
	client "$CB" "$PB" keepid whoami >"$RESULTS/id-b.jsonl" 2>/dev/null
	read -r st proj uid < <(jq -r 'select(.kind=="whoami") | "\(.status) \(.body.project // "-") \(.body.peer_uid // "-")"' "$RESULTS/id-b.jsonl")
	[[ $st == 200 && $proj == "$PB" && $uid == "$UIDB" ]] || ok=0
	if ((ok)); then
		res ID-OTHER PASS "podman run of $CB with A's socket: '$(grep -oi 'statfs[^:]*: permission denied\|permission denied' <<<"$out" | head -1)'; host connect as $CB: '$err'; via its own socket: $st $proj uid $uid"
	else
		res ID-OTHER FAIL "podman: '$(head -c 200 <<<"$out")'; host: '$err'; own socket: $st $proj $uid"
	fi

	log "identity: container root"
	client "$CA" "$PA" root whoami >"$RESULTS/id-root.jsonl" 2>"$RESULTS/id-root.err"
	info id-root "$(jq -r 'select(.kind=="whoami") | "status \(.status), peer uid \(.body.peer_uid // "-")"' "$RESULTS/id-root.jsonl") (control room uid $UIDA)"
}

phase_api() {
	local img cpu mem pids keep own st n0 n1 st1 st2 left
	img=$(project_field "$PA" image)
	cpu=$(project_field "$PA" cpu_millis)
	mem=$(project_field "$PA" mem_bytes)
	pids=$(project_field "$PA" pids_limit)
	log "api: own sandboxes and the ceiling"
	client "$CA" "$PA" keepid cleanup >/dev/null 2>&1
	client "$CA" "$PA" keepid api -image "$img" -cpu-millis "$cpu" -mem-bytes "$mem" -pids-limit "$pids" \
		>"$RESULTS/api-a.jsonl" 2>"$RESULTS/api-a.err"
	record "$RESULTS/api-a.jsonl"
	own=$(jq -r 'select(.kind=="own") | .id' "$RESULTS/api-a.jsonl")
	if [[ -n $own ]] && wsmapi /sandboxes | jq -e --arg s "$own" 'any(.[]; .slug==$s)' >/dev/null; then
		override TENANT-OWN "the management API still lists $own"
	fi

	log "api: another tenant's sandbox"
	keep=$(jq -r 'select(.kind=="keep") | .id' "$RESULTS/api-a.jsonl")
	client "$CB" "$PB" keepid cross -target "$keep" >"$RESULTS/cross-b.jsonl" 2>"$RESULTS/cross-b.err"
	record "$RESULTS/cross-b.jsonl"
	st=$(wsmapi /sandboxes | jq -r --arg s "$keep" '.[] | select(.slug==$s and .project=="'"$PA"'") | .status')
	[[ $st == running ]] || override TENANT-CROSS "A's sandbox $keep is '$st' afterwards"

	log "api: fail closed"
	n0=$(wsmapi /sandboxes | jq --arg p "$PA" '[.[] | select(.project==$p)] | length')
	chattr +i "$AUDIT"
	st1=$(client "$CA" "$PA" keepid create-once 2>/dev/null | jq -r 'select(.kind=="create") | .status')
	n1=$(wsmapi /sandboxes | jq --arg p "$PA" '[.[] | select(.project==$p)] | length')
	chattr -i "$AUDIT"
	st2=$(client "$CA" "$PA" keepid create-once 2>/dev/null | tee "$RESULTS/failclosed-after.jsonl" | jq -r 'select(.kind=="create") | .status')
	if [[ $st1 == 503 && $n0 == "$n1" && $st2 == 201 ]]; then
		res AUDIT-FAILCLOSED PASS "audit immutable: create $st1, sandboxes $n0 -> $n1; writable again: create $st2"
	else
		res AUDIT-FAILCLOSED FAIL "immutable: create $st1, sandboxes $n0 -> $n1; writable: $st2"
	fi

	client "$CA" "$PA" keepid cleanup >"$RESULTS/cleanup-a.jsonl" 2>/dev/null
	left=$(wsmapi /sandboxes | jq --arg p "$PA" '[.[] | select(.project==$p and (.slug | test("^t[a-z0-9]{10}$")))] | length')
	info api-cleanup "tenant sandboxes of $PA left: $left"
}

phase_exec() {
	local env_hits argv_hits test_hits lu luid disk
	if [[ ! -s $SENTINELS ]]; then
		(umask 077 && printf '%s\n%s\n' "$(head -c 24 /dev/urandom | base64 | tr '+/' '-_')" \
			"$(head -c 24 /dev/urandom | base64 | tr '+/' '-_')" >"$SENTINELS")
	fi
	S06_SENTINEL_ENV=$(sed -n 1p "$SENTINELS")
	S06_SENTINEL_ARGV=$(sed -n 2p "$SENTINELS")
	export S06_SENTINEL_ENV S06_SENTINEL_ARGV
	S06_NEEDLES="ENV:$S06_SENTINEL_ENV"$'\n'"ARGV:$S06_SENTINEL_ARGV"
	export S06_NEEDLES
	rm -f "$STATE/scan.stop"
	"$TS" scan -stop "$STATE/scan.stop" -for 30m >"$RESULTS/scan-root.json" 2>&1 &
	(cd / && runuser -u test -- "$TS" scan -stop "$STATE/scan.stop" -for 30m) >"$RESULTS/scan-test.json" 2>&1 &
	sleep 1

	log "exec: scenario (about 6 minutes)"
	client "$CA" "$PA" keepid exec >"$RESULTS/exec-a.jsonl" 2>"$RESULTS/exec-a.err"
	touch "$STATE/scan.stop"
	wait
	unset S06_NEEDLES
	record "$RESULTS/exec-a.jsonl"

	env_hits=$(jq -r '.ENV.processes // "?"' "$RESULTS/scan-root.json")
	argv_hits=$(jq -r '.ARGV.processes // "?"' "$RESULTS/scan-root.json")
	test_hits=$(jq -r '.ARGV.processes // "?"' "$RESULTS/scan-test.json")
	if [[ $env_hits != 0 || $argv_hits == "?" || $argv_hits == 0 ]]; then
		override EXEC-ENV "host argv scan as root: env value in $env_hits processes, argv sentinel in $argv_hits (must be > 0 to show the scan works)"
	fi
	info argv-host "user test read the argv sentinel in $test_hits processes ($(jq -c '.ARGV.comms // []' "$RESULTS/scan-test.json")); root saw it in $argv_hits; env value in $env_hits; scans root=$(jq -r .scans "$RESULTS/scan-root.json") test=$(jq -r .scans "$RESULTS/scan-test.json")"

	lu=$(project_field "$PA" linux_user)
	luid=$(id -u "$lu" 2>/dev/null)
	sed -n 1p "$SENTINELS" >"$STATE/needle-env"
	disk=$(grep -rlsF -f "$STATE/needle-env" "$(getent passwd "$lu" | cut -d: -f6)/.local/share/containers" "/run/user/$luid" 2>/dev/null | sed 's/[0-9a-f]\{64\}/<id>/g' | sort -u | head -5 | tr '\n' ' ')
	info env-on-disk "env sentinel after the execs, in the storage and runtime dirs of $lu: ${disk:-none}"
	rm -f "$STATE/needle-env"
}

phase_restart() {
	local before after
	log "restart: a running control room keeps its mounted socket?"
	client "$CA" "$PA" keepid restart-probe -count 20 >"$RESULTS/restart-probe.jsonl" 2>"$RESULTS/restart-probe.err" &
	sleep 10
	start_server
	wait
	before=$(jq -r 'select(.kind=="probe-step" and .i < 4) | .status' "$RESULTS/restart-probe.jsonl" | tr '\n' ' ')
	after=$(jq -r 'select(.kind=="probe-step" and .i >= 8) | "\(.status)/\(.error | split(": ") | last)"' "$RESULTS/restart-probe.jsonl" | sort | uniq -c | tr -s ' ' | tr '\n' ';')
	info restart-mount "before the restart: $before; from step 8 on: $after"
	client "$CA" "$PA" keepid whoami >"$RESULTS/restart-fresh.jsonl" 2>/dev/null
	info restart-fresh "a new container after the restart: whoami $(jq -r 'select(.kind=="whoami") | .status' "$RESULTS/restart-fresh.jsonl")"
}

phase_audit() {
	local verify tampered n mid missing extra bad hits_audit hits_journal hits_results expect
	log "audit"
	verify=$("$TS" audit-verify "$AUDIT" 2>&1)
	cp "$AUDIT" "$STATE/audit-tampered.jsonl"
	n=$(wc -l <"$STATE/audit-tampered.jsonl")
	mid=$((n / 2))
	sed -i "${mid}s/\"route\":\"/\"route\":\"x/" "$STATE/audit-tampered.jsonl"
	tampered=$("$TS" audit-verify "$STATE/audit-tampered.jsonl" 2>&1)
	if [[ $verify == ok:* ]] && grep -q "line $((mid + 1))" <<<"$tampered"; then
		res AUDIT-CHAIN PASS "real file: $verify; line $mid changed: '$tampered'"
	else
		res AUDIT-CHAIN FAIL "real file: '$verify'; tampered line $mid: '$tampered'"
	fi

	jq -r 'select(.kind=="req") | "\(.req) \(.lines)"' "$RESULTS"/*.jsonl | sort -u >"$STATE/reqs.txt"
	jq -r '.req_id // empty' "$AUDIT" | sort | uniq -c | awk '{print $2, $1}' >"$STATE/audit-counts.txt"
	read -r missing extra bad < <(awk 'NR==FNR {got[$1]=$2; next}
		{ want=$2; have=($1 in got) ? got[$1] : 0
		  if (want == 0) { if (have > 1) bad++ }
		  else if (have == 0) missing++
		  else if (have != want) bad++ }
		END {print missing+0, 0, bad+0}' "$STATE/audit-counts.txt" "$STATE/reqs.txt")
	expect=$(jq -r 'select(.kind=="audit-expect") | "\(.req) \(.reason)"' "$RESULTS"/*.jsonl | while read -r rid reason; do
		jq -r --arg r "$rid" 'select(.req_id==$r and .decision=="end") | .reason' "$AUDIT"
	done | tr '\n' ' ')
	if [[ $missing == 0 && $bad == 0 && $(wc -l <"$STATE/reqs.txt") -gt 50 && $expect == "cancel " ]]; then
		res AUDIT-ALL PASS "$(wc -l <"$STATE/reqs.txt") client requests: each has its line(s) (two for a started exec); cancelled exec ends with: $expect; audit has $(wc -l <"$AUDIT") lines"
	else
		res AUDIT-ALL FAIL "requests $(wc -l <"$STATE/reqs.txt"): missing $missing, wrong count $bad; cancel end reason: '$expect'"
	fi

	if [[ -s $SENTINELS ]]; then
		hits_audit=$(grep -cF -f "$SENTINELS" "$AUDIT")
		hits_journal=$(journalctl -u "$UNIT" --no-pager 2>/dev/null | grep -cF -f "$SENTINELS")
		hits_results=$(grep -rlF -f "$SENTINELS" "$RESULTS" 2>/dev/null | wc -l)
		if [[ $hits_audit == 0 && $hits_journal == 0 && $hits_results == 0 ]]; then
			res AUDIT-NOSECRETS PASS "sentinels (env value and argv[1:]) in the audit: 0 lines, the journal: 0, results/: 0 files"
		else
			res AUDIT-NOSECRETS FAIL "sentinels in the audit: $hits_audit, journal: $hits_journal, results files: $hits_results"
		fi
	else
		res AUDIT-NOSECRETS FAIL "no sentinels: run the exec phase first"
	fi
}

phase_collect() {
	local out=/home/test/s06-out fails
	journalctl -u "$UNIT" --no-pager >"$RESULTS/server.log" 2>&1
	cp "$AUDIT" "$RESULTS/audit.jsonl"
	collect_versions "$RESULTS/versions.txt"
	{
		echo "## podman"
		podman --version
		echo "## wsm"
		wsm version 2>&1 | head -3
	} >>"$RESULTS/versions.txt" 2>&1
	fails=$(failures)
	echo "fails=$fails" >"$RESULTS/status.txt"
	rm -rf "$out"
	mkdir -p "$out"
	cp -r "$RESULTS" "$out/"
	cp /root/s06-console.log "$out/console.log" 2>/dev/null
	chown -R test:test "$out"
	log "collected into $out (fails=$fails)"
}

case ${1:-} in
setup) phase_setup ;;
identity) phase_identity ;;
api) phase_api ;;
exec) phase_exec ;;
restart) phase_restart ;;
audit) phase_audit ;;
collect) phase_collect ;;
all)
	phase_setup
	phase_identity
	phase_api
	phase_exec
	phase_restart
	phase_audit
	phase_collect
	;;
*)
	echo "usage: run.sh setup|identity|api|exec|restart|audit|collect|all" >&2
	exit 2
	;;
esac
