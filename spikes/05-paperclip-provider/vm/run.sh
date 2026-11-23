#!/usr/bin/env bash
# Spike 05 (S1): a wsm provider plugin for Paperclip. Runs everything on a
# disposable VM, as root. Each phase is idempotent.
#
#   bash run.sh setup|plugin|exec|e2e <scenario>|collect|all
#
# setup   base packages, Node 24, wsm, the pclip user, Paperclip, project s05
# plugin  npm ci, typecheck, unit tests, build, install into Paperclip
# exec    vm/exec-check.mjs: the exec layer against a real sandbox
# e2e     vm/e2e.sh <scenario>: file, hostile-1, hostile-2, duplex
# collect results and the console log into /home/test/s05-out for download
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

phase_setup() {
	local src t0 code
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
	info platform "cpus=$(nproc) $(free -m | awk 'NR==2{print "mem_mib="$2}') kernel=$(uname -r)"

	log "packages"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q >"$STATE/apt.log" 2>&1
	apt-get install -y -q curl jq git xz-utils ca-certificates iproute2 >>"$STATE/apt.log" 2>&1 || {
		res SETUP-PAPERCLIP FAIL "apt-get install failed"
		exit 1
	}

	if ! node --version 2>/dev/null | grep -q '^v24\.'; then
		log "node 24"
		local ver
		ver=$(curl -fsS https://nodejs.org/dist/index.json | jq -r '[.[] | select(.version|startswith("v24."))][0].version')
		(
			set -e
			cd /tmp
			curl -fsSLO "https://nodejs.org/dist/$ver/node-$ver-linux-x64.tar.xz"
			curl -fsSLO "https://nodejs.org/dist/$ver/SHASUMS256.txt"
			grep " node-$ver-linux-x64.tar.xz\$" SHASUMS256.txt | sha256sum -c -
			tar -xJf "node-$ver-linux-x64.tar.xz" -C /usr/local --strip-components=1 --no-same-owner
			rm -f "node-$ver-linux-x64.tar.xz" SHASUMS256.txt
		) || {
			res SETUP-PAPERCLIP FAIL "node install failed"
			exit 1
		}
	fi
	node --version >"$STATE/node-version"
	info node "$(node --version) npm=$(npm --version)"
	id pclip >/dev/null 2>&1 || useradd -m -s /bin/bash pclip

	log "wsm"
	chmod +x "$BIN"/* "$SPIKE/../../deploy/install.sh"
	t0=$(date +%s)
	"$SPIKE/../../deploy/install.sh" --bin "$BIN" >"$STATE/wsm-install.log" 2>&1
	code=$?
	info wsm-install "exit=$code in $(($(date +%s) - t0))s"
	((code == 0)) || tail -20 "$STATE/wsm-install.log"
	# Spike-only stand-in for the tenant API (S3): never do this in production.
	usermod -aG wsm,wsm-admin pclip

	# The canary: a host file only pclip can read. Sandboxes must never see it.
	if [[ ! -e $CANARY ]]; then
		head -c 48 /dev/urandom | base64 | tr -d '\n' >"$CANARY"
		chown pclip:pclip "$CANARY"
		chmod 0600 "$CANARY"
	fi

	log "paperclip"
	if [[ $(as_pc paperclipai --version 2>/dev/null) != "$PV" ]]; then
		systemctl stop s05-paperclip 2>/dev/null
		rm -rf /home/pclip/.npm-global /home/pclip/pc
		t0=$(date +%s)
		# npm 11 skips install scripts unless allowed; embedded-postgres needs its postinstall.
		as_pc npm install -g --no-audit --no-fund \
			--allow-scripts=@embedded-postgres/linux-x64,@paperclipai/server,ssh2,protobufjs \
			--prefix /home/pclip/.npm-global "paperclipai@$PV" >"$STATE/npm-paperclip.log" 2>&1
		info paperclip-install "exit=$? in $(($(date +%s) - t0))s"
	fi
	start_paperclip

	log "project $PROJECT"
	bash "$SPIKE/image/gen-containerfile.sh" "$(cat "$STATE/node-version")" >"$STATE/Containerfile"
	if ! wsm project list 2>/dev/null | grep -qw "$PROJECT"; then
		wsm project create --slug "$PROJECT" --containerfile "$STATE/Containerfile" >"$STATE/project-create.log" 2>&1 ||
			tail -5 "$STATE/project-create.log"
	fi
	df -h / | tail -1
	t0=$(date +%s)
	wsm project image "$PROJECT" >"$STATE/project-image.log" 2>&1
	code=$?
	info image-build "exit=$code in $(($(date +%s) - t0))s"
	((code == 0)) || tail -20 "$STATE/project-image.log"
	df -h / | tail -1

	check_setup
}

start_paperclip() {
	local t0 i
	systemctl stop s05-paperclip 2>/dev/null
	systemctl reset-failed s05-paperclip 2>/dev/null
	t0=$(date +%s)
	systemd-run --unit=s05-paperclip --uid=pclip --gid=pclip -p WorkingDirectory=/home/pclip \
		$(printf -- '--setenv=%s ' "${PCENV[@]}") \
		/home/pclip/.npm-global/bin/paperclipai onboard --yes --no-install-service -d /home/pclip/pc >/dev/null
	for i in $(seq 1 150); do
		curl -fsS -m 2 "$API/health" >"$STATE/health.json" 2>/dev/null && break
		sleep 2
	done
	info paperclip-start "healthy after $(($(date +%s) - t0))s"
}

check_setup() {
	local ver health listen pg pid groups wsm_gid admin_gid puser image
	ver=$(as_pc paperclipai --version 2>&1)
	health=$(curl -fsS -m 5 "$API/health" 2>/dev/null)
	listen=$(ss -Hltn 'sport = :3100' | awk '{print $4}' | sort -u | tr '\n' ' ')
	pg=$(ss -Hltn 'sport = :54329' | awk '{print $4}' | sort -u | tr '\n' ' ')
	info postgres-listen "54329 on: ${pg:-none}"
	if [[ $ver == "$PV" ]] && jq -e '.status=="ok" and .deploymentMode=="local_trusted"' <<<"$health" >/dev/null 2>&1 &&
		[[ -n $listen ]] && ! grep -qvE '^127\.0\.0\.1:3100$' <<<"$(tr ' ' '\n' <<<"$listen" | sed '/^$/d')"; then
		res SETUP-PAPERCLIP PASS "version=$ver mode=local_trusted 3100 on: $listen"
	else
		res SETUP-PAPERCLIP FAIL "version=$ver health=$(head -c 120 <<<"$health") 3100 on: $listen"
	fi

	pid=$(pc_unit_pid)
	groups=$(awk '/^Groups:/{$1=""; print}' "/proc/$pid/status" 2>/dev/null)
	wsm_gid=$(getent group wsm | cut -d: -f3)
	admin_gid=$(getent group wsm-admin | cut -d: -f3)
	puser=$(project_user)
	image=$(wsmapi /projects | jq -r --arg p "$PROJECT" '.[] | select(.slug==$p) | .image')
	if systemctl is-active -q wsm.service && [[ -n $puser && -n $image ]] &&
		as_pc podman --remote --url "unix:///run/wsm-podman/$puser/podman.sock" image exists "$image" &&
		grep -qw "$wsm_gid" <<<"$groups" && grep -qw "$admin_gid" <<<"$groups"; then
		res SETUP-WSM PASS "wsm active; project $PROJECT image $image present; unit pid $pid groups:$groups (wsm=$wsm_gid wsm-admin=$admin_gid)"
	else
		res SETUP-WSM FAIL "wsm=$(systemctl is-active wsm.service) user=$puser image=$image unit pid $pid groups:$groups (wsm=$wsm_gid wsm-admin=$admin_gid)"
	fi
	info versions "paperclip=$ver node=$(node --version) podman=$(podman --version | awk '{print $3}') wsm=$(wsm version 2>/dev/null | head -1)"
}

# plugin_record prints the Paperclip plugin record for our plugin as JSON.
plugin_record() {
	curl -fsS "$API/plugins" 2>/dev/null |
		jq -c --arg id "$PLUGIN_ID" '[.. | objects | select((.pluginKey? // .key? // "") == $id)] | first // empty'
}

phase_plugin() {
	local code tap missing name rec status pid
	log "plugin build"
	mkdir -p "$PLUGIN_DIR"
	rm -rf "$PLUGIN_DIR/src" "$PLUGIN_DIR/test" "$PLUGIN_DIR/dist"
	cp -r "$SPIKE/plugin/package.json" "$SPIKE/plugin/tsconfig.json" "$SPIKE/plugin/src" "$SPIKE/plugin/test" "$PLUGIN_DIR/"
	[[ -f $SPIKE/plugin/package-lock.json ]] && cp "$SPIKE/plugin/package-lock.json" "$PLUGIN_DIR/"
	chown -R pclip:pclip "$PLUGIN_DIR"
	mkdir -p "$RESULTS/plugin"
	if [[ -f $PLUGIN_DIR/package-lock.json ]]; then
		as_pc bash -c "cd $PLUGIN_DIR && npm ci --no-audit --no-fund" >"$RESULTS/plugin/npm.log" 2>&1
	else
		as_pc bash -c "cd $PLUGIN_DIR && npm install --no-audit --no-fund" >"$RESULTS/plugin/npm.log" 2>&1
	fi
	code=$?
	cp "$PLUGIN_DIR/package-lock.json" "$RESULTS/plugin/" 2>/dev/null
	as_pc bash -c "cd $PLUGIN_DIR && npm run -s typecheck" >"$RESULTS/plugin/typecheck.log" 2>&1
	local tc=$?
	as_pc bash -c "cd $PLUGIN_DIR && npm run -s build" >"$RESULTS/plugin/build.log" 2>&1
	local bc=$?
	if ((code == 0 && tc == 0 && bc == 0)); then
		res PLUGIN-BUILD PASS "npm $( [[ -f $SPIKE/plugin/package-lock.json ]] && echo ci || echo install)=0 typecheck=0 build=0"
	else
		res PLUGIN-BUILD FAIL "npm=$code typecheck=$tc build=$bc"
		tail -30 "$RESULTS/plugin/typecheck.log" "$RESULTS/plugin/build.log" "$RESULTS/plugin/npm.log"
	fi

	as_pc bash -c "cd $PLUGIN_DIR && npm test" >"$RESULTS/plugin/test.tap" 2>&1
	code=$?
	tap=$RESULTS/plugin/test.tap
	missing=()
	while IFS= read -r name; do
		grep -qE "^ok [0-9]+ - ${name}\$" "$tap" || missing+=("$name")
	done < <(grep -hoE 'test\("S05 [^"]+"' "$SPIKE"/plugin/test/*.test.ts | sed -E 's/^test\("//; s/"$//')
	local total
	total=$(grep -hoE 'test\("S05 [^"]+"' "$SPIKE"/plugin/test/*.test.ts | wc -l)
	if ((code == 0)) && grep -q '^# fail 0$' "$tap" && ((${#missing[@]} == 0)); then
		res PLUGIN-UNIT PASS "npm test=0, # fail 0, $total S05 tests ok"
	else
		res PLUGIN-UNIT FAIL "npm test=$code; $(grep -E '^# (pass|fail)' "$tap" | tr '\n' ' ') missing: ${missing[*]}"
		grep -E '^not ok|error:|Error' "$tap" | head -20
	fi

	log "plugin install"
	rec=$(plugin_record)
	if [[ -n $rec ]]; then
		pid=$(jq -r .id <<<"$rec")
		curl -fsS -X POST "$API/plugins/$pid/disable" -o /dev/null
		curl -fsS -X POST "$API/plugins/$pid/enable" -o /dev/null
	else
		as_pc paperclipai plugin install "$PLUGIN_DIR" --api-base http://127.0.0.1:3100 >"$RESULTS/plugin/install.log" 2>&1
		tail -5 "$RESULTS/plugin/install.log"
	fi
	for _ in $(seq 1 60); do
		rec=$(plugin_record)
		status=$(jq -r '.status // empty' <<<"$rec" 2>/dev/null)
		[[ $status == ready ]] && break
		sleep 2
	done
	curl -fsS "$API/plugins" >"$STATE/plugins.json" 2>/dev/null
	if [[ $status == ready ]] && grep -q '"driverKey":"wsm"' <<<"$rec$(cat "$STATE/plugins.json")"; then
		res PLUGIN-READY PASS "status=ready driver=wsm"
	else
		res PLUGIN-READY FAIL "status=${status:-none} record=$(head -c 300 <<<"$rec")"
	fi
}

phase_exec() {
	log "exec check"
	install -o pclip -g pclip -m 0644 "$VM_DIR/exec-check.mjs" "$PLUGIN_DIR/exec-check.mjs"
	as_pc node "$PLUGIN_DIR/exec-check.mjs" "$PLUGIN_DIR/dist/testkit.js" "$PROJECT" \
		>"$RESULTS/exec-check.out" 2>"$RESULTS/exec-check.log"
	info exec-check "exit=$?"
	while IFS=$'\t' read -r _ id status detail; do
		case $status in
		PASS | FAIL) res "$id" "$status" "$detail" ;;
		*) info "$id" "$detail" ;;
		esac
	done < <(grep -P '^RESULT\t' "$RESULTS/exec-check.out")
}

phase_collect() {
	local out=/home/test/s05-out commit
	rm -rf "$out"
	mkdir -p "$out/05-paperclip-provider"
	cp -r "$RESULTS" "$out/05-paperclip-provider/results"
	cp /root/console.log "$out/05-paperclip-provider/console.log" 2>/dev/null
	commit=$(git get-tar-commit-id <"$S/src.tar" 2>/dev/null)
	{
		echo "run_id=${RUN_ID:-unknown}"
		echo "commit=$commit"
		echo "tree=${TREE:-unknown}"
		echo "date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		. /etc/os-release
		echo "os=$PRETTY_NAME"
		echo "kernel=$(uname -r)"
		echo "address=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')"
		echo "vm=${VM_NAME:-unknown} template=${TEMPLATE:-debian-13-public} fresh_vm=${FRESH_VM:-unknown}"
		echo "paperclip=$(as_pc paperclipai --version 2>&1)"
		echo "node=$(node --version)"
		echo "claude_code=$CLAUDE_VERSION (in the image)"
		echo "podman=$(podman --version | awk '{print $3}')"
		echo "wsm=$(wsm version 2>/dev/null | head -1)"
		echo "memory=$(free -m | awk 'NR==2{print "used_mib="$3" available_mib="$7}')"
		echo "disk=$(df -h / | awk 'NR==2{print "used="$3" free="$4}')"
	} >"$out/run.txt"
	cp "$RESULTS/summary.txt" "$out/SUMMARY.txt"
	local fails
	fails=$(grep -c $'\tFAIL\t' "$RESULTS/summary.txt")
	echo "05-paperclip-provider fails=$fails" >"$out/status.txt"
	chown -R test:test "$out"
	log "collected into $out (fails=$fails)"
}

phase=${1:-}
shift || true
case $phase in
setup) phase_setup ;;
plugin) phase_plugin ;;
exec) phase_exec ;;
e2e) bash "$HERE/e2e.sh" "$@" ;;
collect) phase_collect ;;
all)
	phase_setup
	phase_plugin
	phase_exec
	for sc in file hostile-1 hostile-2 duplex; do bash "$HERE/e2e.sh" "$sc"; done
	phase_collect
	;;
*)
	echo "usage: run.sh setup|plugin|exec|e2e <scenario>|collect|all" >&2
	exit 2
	;;
esac
