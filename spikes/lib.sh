# Shared helpers for the wsm spikes. Source this file; sourcing has no side
# effects. Every run.sh must call guard before it changes anything.

WSM_SPIKE_MARKER=/etc/wsm-spike-disposable

# guard refuses to run anywhere but a disposable VM, and only as root.
guard() {
	if [[ ! -e $WSM_SPIKE_MARKER ]]; then
		echo "refusing to run: $WSM_SPIKE_MARKER is missing; run this only on a disposable VM" >&2
		exit 3
	fi
	if [[ $EUID -ne 0 ]]; then
		echo "refusing to run: must run as root" >&2
		exit 4
	fi
}

# results_init <dir> creates the results directory and an empty summary.
results_init() {
	RESULTS=$1
	mkdir -p "$RESULTS"
	: >"$RESULTS/summary.txt"
}

# report <ID> PASS|FAIL <detail> records one criterion in summary.txt.
report() {
	local id=$1 status=$2
	shift 2
	case $status in
	PASS | FAIL) ;;
	*)
		echo "report: invalid status '$status' for $id" >&2
		status=FAIL
		;;
	esac
	printf '%s\t%s\t%s\n' "$id" "$status" "$*" | tee -a "$RESULTS/summary.txt"
}

# note <text> records informational output that is not a criterion.
note() {
	printf 'INFO\t%s\n' "$*" | tee -a "$RESULTS/summary.txt"
}

# failures prints the number of FAIL lines in the summary.
failures() {
	grep -c $'\tFAIL\t' "$RESULTS/summary.txt"
}

# as_user <user> <cmd...> runs a command as a rootless-Podman user, with the
# runtime dir and user bus that Podman needs for its systemd cgroup manager.
# Variables exported by the caller are passed through.
as_user() {
	local user=$1 uid home
	shift
	uid=$(id -u "$user") || return 1
	home=$(getent passwd "$user" | cut -d: -f6)
	(cd / && runuser -u "$user" -- env HOME="$home" \
		XDG_RUNTIME_DIR="/run/user/$uid" \
		DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
		"$@")
}

# gen_password prints a random alphanumeric password. Never log its output.
gen_password() {
	local pw=''
	while ((${#pw} < 32)); do
		pw+=$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
	done
	printf '%s' "${pw:0:32}"
}

# ensure_cgroup_delegation makes systemd delegate the cpu, io, memory and pids
# controllers to user managers, so rootless Podman can apply --cpus, --memory
# and --pids-limit. Call it before the first user manager starts.
ensure_cgroup_delegation() {
	local dropin=/etc/systemd/system/user@.service.d/wsm-delegate.conf
	[[ -e $dropin ]] && return 0
	mkdir -p "$(dirname "$dropin")" || return 1
	printf '[Service]\nDelegate=cpu cpuset io memory pids\n' >"$dropin" || return 1
	systemctl daemon-reload
}

# ensure_rootless_user <name> [uid] creates a user with subordinate IDs and
# linger, then waits until its systemd user instance is up.
ensure_rootless_user() {
	local name=$1 uid=${2:-} start i
	if ! id -u "$name" >/dev/null 2>&1; then
		useradd --create-home --shell /bin/bash ${uid:+--uid "$uid"} "$name" || return 1
	fi
	uid=$(id -u "$name")
	# A range derived from the UID cannot overlap the ranges useradd hands out
	# (they start at 100000 and grow by 65536 per user).
	start=$((uid * 65536))
	if ! grep -q "^$name:" /etc/subuid; then
		usermod --add-subuids "$start-$((start + 65535))" "$name" || return 1
	fi
	if ! grep -q "^$name:" /etc/subgid; then
		usermod --add-subgids "$start-$((start + 65535))" "$name" || return 1
	fi
	loginctl enable-linger "$name" || return 1
	for i in $(seq 1 30); do
		[[ -S /run/user/$uid/bus ]] && return 0
		sleep 1
	done
	echo "user bus for $name did not come up" >&2
	return 1
}

# collect_versions <file> writes OS and tool versions for the findings.
collect_versions() {
	{
		echo "## os-release"
		cat /etc/os-release
		echo "## uname"
		uname -a
		echo "## cgroup controllers"
		cat /sys/fs/cgroup/cgroup.controllers
		for tool in podman pasta nft conmon crun runc catatonit; do
			echo "## $tool"
			command -v "$tool" >/dev/null && "$tool" --version 2>&1 | head -3
		done
		echo "## go version (if installed)"
		command -v go >/dev/null && go version
	} >"$1" 2>&1
}
