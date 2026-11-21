#!/usr/bin/env bash
# Install wsm on a Debian-like host with systemd, cgroups v2, nftables and
# Podman 5 (docs/plan/bouwplan-fase-1.md, section 7). Run as root:
#
#   deploy/install.sh [--bin DIR] [--admin USER] [--no-packages]
#
#   --bin DIR      directory with the wsm, wsm-helper and wsm-fs binaries (default:
#                  ../bin next to this script)
#   --admin USER   add USER to group wsm-admin, so it can use the wsm CLI
#   --no-packages  do not install the required Debian packages
#
# The script is idempotent: running it again updates binaries and units.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BIN_SRC=$HERE/../bin
ADMIN=
PACKAGES=1

while (($#)); do
	case $1 in
	--bin)
		BIN_SRC=${2:?--bin needs a directory}
		shift 2
		;;
	--admin)
		ADMIN=${2:?--admin needs a user}
		shift 2
		;;
	--no-packages)
		PACKAGES=0
		shift
		;;
	*)
		echo "install.sh: unknown argument $1" >&2
		exit 2
		;;
	esac
done

log() { printf '== %s\n' "$*"; }

if [[ $EUID -ne 0 ]]; then
	echo "install.sh: must run as root" >&2
	exit 1
fi
for f in wsm wsm-helper wsm-fs; do
	if [[ ! -x $BIN_SRC/$f ]]; then
		echo "install.sh: $BIN_SRC/$f is missing; run make build first" >&2
		exit 1
	fi
done
if [[ ! -f /sys/fs/cgroup/cgroup.controllers ]]; then
	echo "install.sh: cgroups v2 is required" >&2
	exit 1
fi

if ((PACKAGES)); then
	log "installing packages"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q
	apt-get install -y -q podman passt uidmap dbus-user-session catatonit git openssh-client \
		nftables iproute2 systemd
fi

log "creating users and groups"
getent group wsm >/dev/null || groupadd --system wsm
getent group wsm-admin >/dev/null || groupadd --system wsm-admin
if ! id -u wsm >/dev/null 2>&1; then
	useradd --system --gid wsm --home-dir /var/lib/wsm --no-create-home \
		--shell /usr/sbin/nologin --comment "wsm manager" wsm
fi
if [[ -n $ADMIN ]]; then
	usermod --append --groups wsm-admin "$ADMIN"
fi

# wsm-svc runs the shared service instances as rootless Quadlet units
# (ADR 0013). wsm-helper writes one unit per instance; this script only
# prepares the user and pulls no images. The subordinate IDs are a fixed
# range below the project users' ranges (uid * 65536 from 50000 on).
log "preparing service user wsm-svc"
SVC_SUBIDS=1879048192-1879113727
if ! id -u wsm-svc >/dev/null 2>&1; then
	useradd --system --user-group --home-dir /var/lib/wsm-svc --create-home \
		--shell /usr/sbin/nologin --comment "wsm shared services" wsm-svc
fi
install -d -m 0700 -o wsm-svc -g wsm-svc /var/lib/wsm-svc
grep -q '^wsm-svc:' /etc/subuid 2>/dev/null || usermod --add-subuids "$SVC_SUBIDS" wsm-svc
grep -q '^wsm-svc:' /etc/subgid 2>/dev/null || usermod --add-subgids "$SVC_SUBIDS" wsm-svc
loginctl enable-linger wsm-svc
SVC_UID=$(id -u wsm-svc)
install -d -m 0755 -o root -g root /etc/containers/systemd/users/"$SVC_UID"

log "installing binaries"
install -m 0755 -o root -g root "$BIN_SRC/wsm" /usr/local/bin/wsm
install -m 0755 -o root -g root "$BIN_SRC/wsm-helper" /usr/local/bin/wsm-helper
# wsm-fs is mounted read-only into every sandbox at /opt/wsm/bin; the
# directory holds nothing else (CLAUDE.md invariant 1).
install -d -m 0755 -o root -g root /usr/local/lib/wsm/bin
install -m 0755 -o root -g root "$BIN_SRC/wsm-fs" /usr/local/lib/wsm/bin/wsm-fs

# Dashboard and preview settings. The file is created once and then left
# to the administrator.
install -d -m 0755 /etc/wsm
if [[ ! -e /etc/wsm/wsm.env ]]; then
	install -m 0644 "$HERE/wsm.env.example" /etc/wsm/wsm.env
fi

log "installing systemd units"
install -d -m 0755 /etc/systemd/system/user@.service.d
# Must be in place before the first project user's manager starts.
install -m 0644 "$HERE/user-delegate.conf" /etc/systemd/system/user@.service.d/wsm-delegate.conf
install -m 0644 "$HERE/tmpfiles-wsm.conf" /etc/tmpfiles.d/wsm.conf
for u in wsm-helper.socket wsm-helper.service wsm.socket wsm.service; do
	install -m 0644 "$HERE/systemd/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
systemd-tmpfiles --create /etc/tmpfiles.d/wsm.conf

log "starting wsm"
systemctl enable --now wsm-helper.socket wsm.socket
systemctl enable wsm-helper.service wsm.service
systemctl restart wsm-helper.service
systemctl restart wsm.service

for _ in $(seq 1 30); do
	if systemctl is-active --quiet wsm.service && nft list table inet wsm >/dev/null 2>&1; then
		log "wsm is running; base rules loaded"
		exit 0
	fi
	sleep 1
done
echo "install.sh: wsm did not come up; see journalctl -u wsm -u wsm-helper" >&2
exit 1
