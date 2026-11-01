#!/usr/bin/env bash
# Spike 02: the manager (user wsm) drives a project user's rootless Podman
# over its API socket (docs/plan/bouwplan-fase-1.md, sections 3.1, 3.4, 3.8
# and 5). Also checks whether the official bindings build without cgo.
#
# Run only on a disposable VM, as root; see spikes/README.md. A failing test
# does not stop the script. Exit code: 0 when every criterion passed, 1
# otherwise, 3/4 when guard refuses.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../lib.sh
source "$HERE/../lib.sh"

guard

UID_A=50000
UID_B=50001
IMAGE=docker.io/library/debian:trixie-slim
SOCK_DIR=/run/wsm-podman/wsm-p-a
SOCK=$SOCK_DIR/podman.sock
BIN=/usr/local/lib/wsm-spike
STATICCHECK_VERSION=2025.1.1
PROGRAM_IDS=(POD-USERNS POD-HARDENING POD-EXEC-WS POD-RESIZE)

export GOTOOLCHAIN=local CGO_ENABLED=0

results_init "$HERE/results"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

log() { printf '== %s\n' "$*"; }

collect_diagnostics() {
	collect_versions "$RESULTS/versions.txt"
	as_user wsm-p-a podman info >"$RESULTS/podman-info-wsm-p-a.txt" 2>&1
	{
		echo "## socket"
		ls -la "$SOCK_DIR"
		stat "$SOCK"
		echo "## podman.socket (wsm-p-a)"
		as_user wsm-p-a systemctl --user cat podman.socket
		as_user wsm-p-a systemctl --user status --no-pager podman.socket podman.service
		echo "## id wsm"
		id wsm
		echo "## cgroup controllers for user@$UID_A"
		cat "/sys/fs/cgroup/user.slice/user-$UID_A.slice/user@$UID_A.service/cgroup.controllers"
		echo "## podman ps (wsm-p-a)"
		as_user wsm-p-a podman ps -a --pod
	} >"$RESULTS/socket.txt" 2>&1
}

setup_fail() {
	report "SETUP-$1" FAIL "see $1.log"
	collect_diagnostics
	exit 1
}

# --- setup ------------------------------------------------------------------

log "installing packages"
export DEBIAN_FRONTEND=noninteractive
{
	apt-get update -q &&
		apt-get install -y -q podman passt uidmap dbus-user-session catatonit \
			golang-go git ca-certificates curl file
} >"$RESULTS/packages.log" 2>&1 || setup_fail packages
go version >>"$RESULTS/packages.log" 2>&1

log "creating users"
(
	ensure_cgroup_delegation &&
		ensure_rootless_user wsm-p-a "$UID_A" &&
		ensure_rootless_user wsm-p-b "$UID_B" &&
		[[ $(id -u wsm-p-a) == "$UID_A" ]] || exit 1
	# A user manager that started before the delegation drop-in existed has
	# no cpu controller; restart it so --cpus works.
	ctrl=/sys/fs/cgroup/user.slice/user-$UID_A.slice/user@$UID_A.service/cgroup.controllers
	if ! grep -qw cpu "$ctrl"; then
		systemctl restart "user@$UID_A.service" || exit 1
		sleep 2
	fi
	grep -w cpu "$ctrl" || exit 1
	getent group wsm >/dev/null || groupadd --system wsm || exit 1
	if ! id -u wsm >/dev/null 2>&1; then
		useradd --system --gid wsm --home-dir /nonexistent --no-create-home \
			--shell /usr/sbin/nologin wsm || exit 1
	fi
	id wsm
) >"$RESULTS/users.log" 2>&1 || setup_fail users

log "giving group wsm access to wsm-p-a's Podman socket"
# The socket lives outside the user's 0700 runtime dir, in a setgid directory
# owned by group wsm, so the socket file gets group wsm. wsm-p-a needs no
# membership in wsm, and other project users cannot traverse the directory.
(
	cat >/etc/tmpfiles.d/wsm-podman.conf <<EOF
d /run/wsm-podman 0755 root root -
d $SOCK_DIR 2750 wsm-p-a wsm -
EOF
	systemd-tmpfiles --create /etc/tmpfiles.d/wsm-podman.conf || exit 1
	dropin=/home/wsm-p-a/.config/systemd/user/podman.socket.d
	as_user wsm-p-a mkdir -p "$dropin" || exit 1
	cat >"$dropin/wsm.conf" <<EOF
[Socket]
ListenStream=
ListenStream=$SOCK
SocketMode=0660
EOF
	chmod 0644 "$dropin/wsm.conf"
	as_user wsm-p-a systemctl --user daemon-reload &&
		as_user wsm-p-a systemctl --user enable podman.socket &&
		as_user wsm-p-a systemctl --user restart podman.socket || exit 1
	for _ in $(seq 1 20); do
		[[ -S $SOCK ]] && break
		sleep 0.5
	done
	[[ -S $SOCK ]] || exit 1
	ls -la "$SOCK_DIR"
) >"$RESULTS/socket.log" 2>&1 || setup_fail socket

log "POD-SOCKET-ACCESS"
L=$RESULTS/POD-SOCKET-ACCESS.log
ping_as() {
	(cd / && runuser -u "$1" -- curl -sS --max-time 5 --unix-socket "$SOCK" http://d/_ping)
}
ok=1
if ping_as wsm >>"$L" 2>&1; then
	echo "-- wsm: _ping ok" >>"$L"
else
	echo "-- wsm: _ping failed" >>"$L"
	ok=0
fi
if ping_as wsm-p-b >>"$L" 2>&1; then
	echo "-- wsm-p-b: _ping succeeded, expected denial" >>"$L"
	ok=0
else
	echo "-- wsm-p-b: denied (expected)" >>"$L"
fi
if ((ok)); then
	report POD-SOCKET-ACCESS PASS "wsm reaches wsm-p-a's socket via group wsm; wsm-p-b does not"
else
	report POD-SOCKET-ACCESS FAIL "see POD-SOCKET-ACCESS.log"
fi

log "pulling $IMAGE as wsm-p-a"
as_user wsm-p-a podman pull -q "$IMAGE" >"$RESULTS/pull.log" 2>&1 || setup_fail pull

# --- Go program -------------------------------------------------------------

log "GO-TEST"
mkdir -p "$BIN" && chmod 0755 "$BIN"
if (cd "$HERE" && go vet ./... && go test -count=1 ./...) >"$RESULTS/GO-TEST.log" 2>&1; then
	report GO-TEST PASS "go vet and go test in 02-podman-api"
else
	report GO-TEST FAIL "see GO-TEST.log"
fi

log "building podmanapi"
(cd "$HERE" && go build -trimpath -o "$BIN/podmanapi" .) >"$RESULTS/build.log" 2>&1 ||
	setup_fail build
chmod 0755 "$BIN/podmanapi"

log "running podmanapi as wsm"
(cd / && runuser -u wsm -- "$BIN/podmanapi" -socket "$SOCK" -host-uid "$UID_A" -image "$IMAGE") \
	>"$TMP/program.out" 2>"$RESULTS/podmanapi.log"
echo "exit code $?" >>"$RESULTS/podmanapi.log"
for id in "${PROGRAM_IDS[@]}"; do
	line=$(ID=$id awk -F '\t' '$1 == ENVIRON["ID"] { print; exit }' "$TMP/program.out")
	if [[ -z $line ]]; then
		report "$id" FAIL "no result from podmanapi; see podmanapi.log"
		continue
	fi
	IFS=$'\t' read -r _ status detail <<<"$line"
	report "$id" "$status" "$detail"
done

# --- bindings ---------------------------------------------------------------

log "POD-BINDINGS-CGO0"
L=$RESULTS/POD-BINDINGS-CGO0.log
PROBE=$TMP/bindings-probe
cp -r "$HERE/bindings-probe" "$PROBE"
ok=0
detail="build failed; see POD-BINDINGS-CGO0.log"
{
	echo "-- go mod tidy"
	(cd "$PROBE" && go mod tidy) &&
		echo "-- go build" &&
		(cd "$PROBE" && go build -trimpath -tags 'remote containers_image_openpgp' -o "$BIN/bindingsprobe" .)
} >>"$L" 2>&1
build_rc=$?
cp "$PROBE/go.mod" "$RESULTS/bindings-go.mod" 2>/dev/null
cp "$PROBE/go.sum" "$RESULTS/bindings-go.sum" 2>/dev/null
if ((build_rc == 0)); then
	chmod 0755 "$BIN/bindingsprobe"
	desc=$(file -b "$BIN/bindingsprobe")
	lddout=$(ldd "$BIN/bindingsprobe" 2>&1)
	size=$(stat -c %s "$BIN/bindingsprobe")
	printf -- '-- file: %s\n-- ldd: %s\n-- size: %s bytes\n' "$desc" "$lddout" "$size" >>"$L"
	go version -m "$BIN/bindingsprobe" >"$RESULTS/bindings-deps.txt" 2>&1
	if [[ $desc == *"statically linked"* && $lddout == *"not a dynamic executable"* ]]; then
		ok=1
		detail="bindings build static with CGO_ENABLED=0 ($size bytes)"
	else
		detail="binary is not static: $desc"
	fi
	if (cd / && runuser -u wsm -- "$BIN/bindingsprobe" -socket "$SOCK") >>"$L" 2>&1; then
		note "bindingsprobe talked to wsm-p-a's socket as wsm"
	else
		note "bindingsprobe built but could not talk to the socket; see POD-BINDINGS-CGO0.log"
	fi
fi
# FAIL is a valid outcome here: it means we keep our own REST client.
if ((ok)); then
	report POD-BINDINGS-CGO0 PASS "$detail"
else
	report POD-BINDINGS-CGO0 FAIL "$detail; a FAIL means: own REST client"
fi

# --- staticcheck ------------------------------------------------------------

log "STATICCHECK"
L=$RESULTS/STATICCHECK.log
if GOBIN=$TMP/bin go install "honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" >>"$L" 2>&1; then
	ok=1
	echo "-- 02-podman-api" >>"$L"
	(cd "$HERE" && "$TMP/bin/staticcheck" ./...) >>"$L" 2>&1 || ok=0
	if ((build_rc == 0)); then
		echo "-- bindings-probe" >>"$L"
		(cd "$PROBE" && "$TMP/bin/staticcheck" -tags 'remote containers_image_openpgp' ./...) >>"$L" 2>&1 || ok=0
	fi
	if ((ok)); then
		report STATICCHECK PASS "staticcheck $STATICCHECK_VERSION clean"
	else
		report STATICCHECK FAIL "findings in STATICCHECK.log"
	fi
else
	report STATICCHECK FAIL "could not install staticcheck $STATICCHECK_VERSION; see STATICCHECK.log"
fi

# --- wrap up ----------------------------------------------------------------

collect_diagnostics
n=$(failures)
log "done: $n failed criteria; summary in $RESULTS/summary.txt"
((n == 0))
