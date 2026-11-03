#!/usr/bin/env bash
# Spike 01: per-UID nftables egress filtering for rootless pasta pods, with
# MariaDB on wsm0 (docs/plan/bouwplan-fase-1.md, sections 3.3 and 5).
#
# Run only on a disposable VM, as root; see spikes/README.md. A failing test
# does not stop the script. Exit code: 0 when every criterion passed, 1
# otherwise, 3/4 when guard refuses.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../lib.sh
source "$HERE/../lib.sh"

guard

WSM0_ADDR=10.255.0.1
MARIADB_PORT=3306
MCP_PORT=7443
OTHER_PORT=9999     # root listener on wsm0 that no project may reach
LOOPBACK_PORT=18081 # root listener on host loopback
PREVIEW_PORT=18080  # project B's preview, published on 127.0.0.1
HOST_PORT=18082     # root listener on all host addresses
UID_A=50000
UID_B=50001
MARIADB_IMAGE=docker.io/library/mariadb:11.4
BUSYBOX_IMAGE=docker.io/library/busybox:1.37
PROBE_TIMEOUT=5

# Hardening for every workspace container (CLAUDE.md, invariant 9).
WORK_FLAGS=(--user 1000:1000 --cap-drop=ALL --security-opt=no-new-privileges
	--cpus 1 --memory 512m --pids-limit 256)

results_init "$HERE/results"
TMP=$(mktemp -d)
LISTENERS=()
cleanup() {
	((${#LISTENERS[@]})) && kill "${LISTENERS[@]}" 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

log() { printf '== %s\n' "$*"; }

collect_diagnostics() {
	collect_versions "$RESULTS/versions.txt"
	nft list ruleset >"$RESULTS/nft-ruleset.txt" 2>&1
	as_user wsm-p-a podman info >"$RESULTS/podman-info-wsm-p-a.txt" 2>&1
	{
		echo "## ip addr"
		ip addr
		echo "## ip route"
		ip route
		ip -6 route
		echo "## /etc/resolv.conf"
		cat /etc/resolv.conf
		echo "## pasta processes"
		ps -o user,pid,args -C pasta,pasta.avx2
		for u in wsm-svc wsm-p-a wsm-p-b; do
			echo "## podman ps ($u)"
			as_user "$u" podman ps -a --pod
		done
		echo "## /etc/resolv.conf in sbx-a-work"
		as_user wsm-p-a podman exec sbx-a-work cat /etc/resolv.conf
		echo "## /etc/hosts in sbx-a-work"
		as_user wsm-p-a podman exec sbx-a-work cat /etc/hosts
	} >"$RESULTS/network.txt" 2>&1
}

# setup_fail <step> records a setup failure, collects what it can and exits:
# without the setup no criterion can be tested.
setup_fail() {
	report "SETUP-$1" FAIL "see $1.log"
	collect_diagnostics
	exit 1
}

# --- probes -----------------------------------------------------------------

# A TCP connect with bash's /dev/tcp; works in any image that has bash.
TCP_CONNECT='exec 3<>"/dev/tcp/$0/$1"'

probe_root() { timeout "$PROBE_TIMEOUT" bash -c "$TCP_CONNECT" "$1" "$2"; }

# probe_uid <user> <host> <port>: connect as the project user on the host,
# the same position pasta is in.
probe_uid() { as_user "$1" timeout "$PROBE_TIMEOUT" bash -c "$TCP_CONNECT" "$2" "$3"; }

# probe_ctr <user> <container> <host> <port>: connect from inside a sandbox.
probe_ctr() {
	as_user "$1" podman exec "$2" timeout "$PROBE_TIMEOUT" bash -c "$TCP_CONNECT" "$3" "$4"
}

counter_packets() {
	nft list counter inet wsm "$1" |
		awk '{ for (i = 1; i < NF; i++) if ($i == "packets") { print $(i + 1); exit } }'
}

# blocked_by <log> <counter> <cmd...>: true when cmd fails and the named
# counter moved, so the wsm rule did the blocking.
blocked_by() {
	local log=$1 counter=$2 before after rc
	shift 2
	before=$(counter_packets "$counter")
	"$@" >>"$log" 2>&1
	rc=$?
	after=$(counter_packets "$counter")
	printf -- '-- %s: rc=%s, counter %s %s -> %s\n' "$*" "$rc" "$counter" "$before" "$after" >>"$log"
	[[ $rc -ne 0 && ${after:-0} -gt ${before:-0} ]]
}

# fails <log> <cmd...>: true when cmd fails. For paths where pasta itself may
# refuse before the host sees a packet.
fails() {
	local log=$1 rc
	shift
	"$@" >>"$log" 2>&1
	rc=$?
	printf -- '-- %s: rc=%s (expect failure)\n' "$*" "$rc" >>"$log"
	[[ $rc -ne 0 ]]
}

# succeeds <log> <cmd...>: true when cmd succeeds. Used for controls.
succeeds() {
	local log=$1 rc
	shift
	"$@" >>"$log" 2>&1
	rc=$?
	printf -- '-- %s: rc=%s (expect success)\n' "$*" "$rc" >>"$log"
	[[ $rc -eq 0 ]]
}

# verdict <ID> <ok> <detail>
verdict() {
	if (($2)); then
		report "$1" PASS "$3"
	else
		report "$1" FAIL "$3; see $1.log"
	fi
}

is_private4() {
	case $1 in
	10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].*) return 0 ;;
	100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) return 0 ;;
	esac
	return 1
}

# --- setup ------------------------------------------------------------------

log "installing packages"
export DEBIAN_FRONTEND=noninteractive
{
	apt-get update -q &&
		apt-get install -y -q podman passt nftables mariadb-client netcat-openbsd \
			uidmap dbus-user-session catatonit iproute2
} >"$RESULTS/packages.log" 2>&1 || setup_fail packages

log "creating users"
{
	ensure_cgroup_delegation &&
		ensure_rootless_user wsm-svc &&
		ensure_rootless_user wsm-p-a "$UID_A" &&
		ensure_rootless_user wsm-p-b "$UID_B" &&
		[[ $(id -u wsm-p-a) == "$UID_A" && $(id -u wsm-p-b) == "$UID_B" ]]
} >"$RESULTS/users.log" 2>&1 || setup_fail users

log "removing state from a previous run"
{
	as_user wsm-p-a podman pod rm -f --ignore sbx-a
	as_user wsm-p-b podman pod rm -f --ignore sbx-b
	as_user wsm-svc podman rm -f --ignore wsm-mariadb
	nft delete table inet wsm
	ip link del wsm0
} >"$RESULTS/cleanup.log" 2>&1

log "pulling images"
{
	as_user wsm-svc podman pull -q "$MARIADB_IMAGE" &&
		as_user wsm-p-a podman pull -q "$MARIADB_IMAGE" &&
		as_user wsm-p-b podman pull -q "$MARIADB_IMAGE" &&
		as_user wsm-p-b podman pull -q "$BUSYBOX_IMAGE"
} >"$RESULTS/pull.log" 2>&1 || setup_fail pull

log "creating wsm0"
{
	ip link add wsm0 type dummy &&
		ip addr add "$WSM0_ADDR/32" dev wsm0 &&
		ip link set wsm0 up
} >"$RESULTS/wsm0.log" 2>&1 || setup_fail wsm0

log "starting MariaDB as wsm-svc on $WSM0_ADDR:$MARIADB_PORT"
ROOT_PW=$(gen_password)
(
	umask 077
	printf '[client]\nuser=root\npassword=%s\nhost=%s\nport=%s\nskip-ssl-verify-server-cert\n' \
		"$ROOT_PW" "$WSM0_ADDR" "$MARIADB_PORT" >"$TMP/root.cnf"
)
# -e NAME without a value copies it from the environment, so the password
# never appears on a command line.
export MARIADB_ROOT_PASSWORD=$ROOT_PW
as_user wsm-svc podman run -d --name wsm-mariadb \
	-p "$WSM0_ADDR:$MARIADB_PORT:3306" -e MARIADB_ROOT_PASSWORD \
	--memory 1g --pids-limit 512 "$MARIADB_IMAGE" >"$RESULTS/mariadb.log" 2>&1 ||
	setup_fail mariadb
unset ROOT_PW MARIADB_ROOT_PASSWORD
ready=0
for _ in $(seq 1 90); do
	if mariadb --defaults-extra-file="$TMP/root.cnf" -N -e 'SELECT 1' >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
((ready)) || setup_fail mariadb

A_PW=$(gen_password)
mariadb --defaults-extra-file="$TMP/root.cnf" >>"$RESULTS/mariadb.log" 2>&1 <<SQL || setup_fail mariadb
CREATE USER 'sbx_a'@'%' IDENTIFIED BY '$A_PW' WITH MAX_USER_CONNECTIONS 50;
GRANT ALL PRIVILEGES ON \`sbx\\_a\\_%\`.* TO 'sbx_a'@'%';
CREATE DATABASE \`sbx_a_app\`;
SQL

log "loading nftables.nft"
(
	nft -f "$HERE/nftables.nft" || exit 1
	awk '$1 == "nameserver" { print $2 }' /etc/resolv.conf | while read -r ns; do
		if [[ $ns == *:* ]]; then
			nft add element inet wsm dns6 "{ ${ns%%%*} }" || exit 1
		else
			nft add element inet wsm dns4 "{ $ns }" || exit 1
		fi
	done
	nft list set inet wsm dns4
	nft list set inet wsm dns6
) >"$RESULTS/nft-load.log" 2>&1 || setup_fail nft-load

log "starting sandbox pods"
{
	as_user wsm-p-a podman pod create --name sbx-a \
		--userns=keep-id:uid=1000,gid=1000 --network pasta &&
		as_user wsm-p-a podman run -d --pod sbx-a --name sbx-a-work --init \
			"${WORK_FLAGS[@]}" --entrypoint sleep "$MARIADB_IMAGE" infinity &&
		as_user wsm-p-b podman pod create --name sbx-b \
			--userns=keep-id:uid=1000,gid=1000 --network pasta \
			-p "127.0.0.1:$PREVIEW_PORT:8080" &&
		as_user wsm-p-b podman run -d --pod sbx-b --name sbx-b-work --init \
			"${WORK_FLAGS[@]}" --entrypoint sleep "$MARIADB_IMAGE" infinity &&
		as_user wsm-p-b podman run -d --pod sbx-b --name sbx-b-preview \
			"${WORK_FLAGS[@]}" "$BUSYBOX_IMAGE" httpd -f -p 8080 -h /tmp
} >"$RESULTS/pods.log" 2>&1 || setup_fail pods

log "starting root listeners"
start_listener() {
	nc -dlk "$@" >/dev/null 2>&1 &
	LISTENERS+=($!)
}
start_listener 127.0.0.1 "$LOOPBACK_PORT"
start_listener -6 ::1 "$LOOPBACK_PORT"
start_listener "$WSM0_ADDR" "$OTHER_PORT"
start_listener "$WSM0_ADDR" "$MCP_PORT"
start_listener 0.0.0.0 "$HOST_PORT"
sleep 2

GATEWAY=$(ip -4 route show default | awk '{ print $3; exit }')
HOST_ADDR=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }')
note "gateway=${GATEWAY:-none} host_addr=${HOST_ADDR:-none}"

# --- tests ------------------------------------------------------------------

log "NET-DNS"
L=$RESULTS/NET-DNS.log
ok=0
succeeds "$L" as_user wsm-p-a podman exec sbx-a-work getent hosts deb.debian.org && ok=1
verdict NET-DNS $ok "resolve deb.debian.org from sbx-a-work"

log "NET-INTERNET"
L=$RESULTS/NET-INTERNET.log
# shellcheck disable=SC2016
HTTP_HEAD='exec 3<>/dev/tcp/deb.debian.org/80 &&
printf "HEAD / HTTP/1.0\r\nHost: deb.debian.org\r\n\r\n" >&3 && head -c 12 <&3'
ok=0
as_user wsm-p-a podman exec sbx-a-work timeout 10 bash -c "$HTTP_HEAD" >"$TMP/http" 2>>"$L"
cat "$TMP/http" >>"$L"
grep -q '^HTTP/' "$TMP/http" && ok=1
verdict NET-INTERNET $ok "HTTP HEAD to deb.debian.org:80 from sbx-a-work"

log "NET-MARIA-BEFORE"
L=$RESULTS/NET-MARIA-BEFORE.log
ok=1
succeeds "$L" probe_root "$WSM0_ADDR" "$MARIADB_PORT" || ok=0
blocked_by "$L" wsm0_blocked probe_ctr wsm-p-a sbx-a-work "$WSM0_ADDR" "$MARIADB_PORT" || ok=0
verdict NET-MARIA-BEFORE $ok "root reaches MariaDB, sbx-a-work is blocked before service_enable"

log "service_enable: allow project A to $WSM0_ADDR:$MARIADB_PORT"
nft add element inet wsm svc_allow4 "{ $UID_A . $WSM0_ADDR . $MARIADB_PORT }" >>"$RESULTS/nft-load.log" 2>&1
note "svc_allow4 += $UID_A . $WSM0_ADDR . $MARIADB_PORT"

log "NET-MARIA-AFTER"
L=$RESULTS/NET-MARIA-AFTER.log
ok=0
printf '[client]\nuser=sbx_a\npassword=%s\nskip-ssl-verify-server-cert\n' "$A_PW" |
	as_user wsm-p-a podman exec -i sbx-a-work sh -c 'umask 077 && cat >/tmp/sbx.cnf' >>"$L" 2>&1
unset A_PW
before=$(counter_packets svc_allowed)
as_user wsm-p-a podman exec sbx-a-work mariadb --defaults-extra-file=/tmp/sbx.cnf \
	-h "$WSM0_ADDR" -P "$MARIADB_PORT" -N -e 'SELECT CURRENT_USER()' >"$TMP/whoami" 2>>"$L"
after=$(counter_packets svc_allowed)
cat "$TMP/whoami" >>"$L"
echo "-- svc_allowed $before -> $after" >>"$L"
grep -qx 'sbx_a@%' "$TMP/whoami" && ((after > before)) && ok=1
verdict NET-MARIA-AFTER $ok "sbx-a-work logs in as sbx_a after service_enable"

log "NET-MARIA-OTHER"
L=$RESULTS/NET-MARIA-OTHER.log
ok=0
blocked_by "$L" wsm0_blocked probe_ctr wsm-p-b sbx-b-work "$WSM0_ADDR" "$MARIADB_PORT" && ok=1
verdict NET-MARIA-OTHER $ok "sbx-b-work stays blocked after A's service_enable"

log "NET-LAN"
L=$RESULTS/NET-LAN.log
targets=(10.0.0.1 172.16.0.1 192.168.1.1 100.64.0.1)
if [[ -n $GATEWAY ]] && is_private4 "$GATEWAY"; then
	targets+=("$GATEWAY")
else
	echo "-- gateway ${GATEWAY:-none} is not private, not probed" >>"$L"
fi
ok=1
for t in "${targets[@]}"; do
	blocked_by "$L" lan_blocked probe_ctr wsm-p-a sbx-a-work "$t" 80 || ok=0
done
# pasta copies the host address into the pod, so from a container this address
# is the pod itself and the packet never reaches the host's rules.
if [[ -n $HOST_ADDR ]] && is_private4 "$HOST_ADDR"; then
	fails "$L" probe_ctr wsm-p-a sbx-a-work "$HOST_ADDR" 80 || ok=0
fi
blocked_by "$L" lan_blocked probe_uid wsm-p-a 192.168.1.1 80 || ok=0
verdict NET-LAN $ok "private ranges blocked from sbx-a-work and for UID $UID_A: ${targets[*]}"

log "NET-METADATA"
L=$RESULTS/NET-METADATA.log
ok=1
blocked_by "$L" linklocal_blocked probe_ctr wsm-p-a sbx-a-work 169.254.169.254 80 || ok=0
blocked_by "$L" linklocal_blocked probe_uid wsm-p-a 169.254.169.254 80 || ok=0
verdict NET-METADATA $ok "169.254.169.254:80 blocked from sbx-a-work and for UID $UID_A"

log "NET-LOOPBACK"
L=$RESULTS/NET-LOOPBACK.log
ok=1
succeeds "$L" probe_root 127.0.0.1 "$LOOPBACK_PORT" || ok=0
blocked_by "$L" loopback_blocked probe_uid wsm-p-a 127.0.0.1 "$LOOPBACK_PORT" || ok=0
if probe_root ::1 "$LOOPBACK_PORT" >>"$L" 2>&1; then
	blocked_by "$L" loopback_blocked probe_uid wsm-p-a ::1 "$LOOPBACK_PORT" || ok=0
else
	echo "-- no IPv6 loopback listener, ::1 not probed" >>"$L"
fi
# Ways a container could reach the host through pasta.
for t in host.containers.internal 169.254.1.2 ${GATEWAY:+"$GATEWAY"}; do
	fails "$L" probe_ctr wsm-p-a sbx-a-work "$t" "$LOOPBACK_PORT" || ok=0
done
verdict NET-LOOPBACK $ok "host loopback listener unreachable for UID $UID_A and from sbx-a-work"

log "NET-CROSS-PREVIEW"
L=$RESULTS/NET-CROSS-PREVIEW.log
ok=1
succeeds "$L" probe_root 127.0.0.1 "$PREVIEW_PORT" || ok=0
blocked_by "$L" loopback_blocked probe_uid wsm-p-a 127.0.0.1 "$PREVIEW_PORT" || ok=0
for t in host.containers.internal 169.254.1.2 ${GATEWAY:+"$GATEWAY"} ${HOST_ADDR:+"$HOST_ADDR"}; do
	fails "$L" probe_ctr wsm-p-a sbx-a-work "$t" "$PREVIEW_PORT" || ok=0
done
verdict NET-CROSS-PREVIEW $ok "root reaches B's preview on 127.0.0.1:$PREVIEW_PORT, project A does not"

log "NET-WSM0-OTHER-PORT"
L=$RESULTS/NET-WSM0-OTHER-PORT.log
ok=1
succeeds "$L" probe_root "$WSM0_ADDR" "$OTHER_PORT" || ok=0
blocked_by "$L" wsm0_blocked probe_ctr wsm-p-a sbx-a-work "$WSM0_ADDR" "$OTHER_PORT" || ok=0
verdict NET-WSM0-OTHER-PORT $ok "$WSM0_ADDR:$OTHER_PORT blocked for A, which may reach only $MARIADB_PORT and $MCP_PORT"

log "NET-HOST-SERVICE"
L=$RESULTS/NET-HOST-SERVICE.log
# A host service bound to all addresses. On a public host address no block rule
# applies, so this is the test that shows whether a sandbox can reach it.
if is_private4 "${HOST_ADDR:-}"; then
	echo "-- host address ${HOST_ADDR:-none} is private" >>"$L"
else
	echo "-- host address ${HOST_ADDR:-none} is not private" >>"$L"
fi
note "host address class: $(is_private4 "${HOST_ADDR:-}" && echo private || echo public)"
ok=1
succeeds "$L" probe_root "${HOST_ADDR:-127.0.0.1}" "$HOST_PORT" || ok=0
if [[ -n $HOST_ADDR ]]; then
	fails "$L" probe_uid wsm-p-a "$HOST_ADDR" "$HOST_PORT" || ok=0
fi
for t in host.containers.internal 169.254.1.2; do
	fails "$L" probe_ctr wsm-p-a sbx-a-work "$t" "$HOST_PORT" || ok=0
done
verdict NET-HOST-SERVICE $ok "host service on 0.0.0.0:$HOST_PORT unreachable for UID $UID_A and from sbx-a-work"

# Informational: not criteria, but useful for the findings.
L=$RESULTS/INFO.log
if probe_ctr wsm-p-a sbx-a-work "$WSM0_ADDR" "$MCP_PORT" >>"$L" 2>&1; then
	note "MCP port $WSM0_ADDR:$MCP_PORT reachable from sbx-a-work (expected)"
else
	note "MCP port $WSM0_ADDR:$MCP_PORT NOT reachable from sbx-a-work (unexpected)"
fi

# --- wrap up ----------------------------------------------------------------

collect_diagnostics
n=$(failures)
log "done: $n failed criteria; summary in $RESULTS/summary.txt"
((n == 0))
