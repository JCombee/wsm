#!/usr/bin/env bash
# Spike 07 (S2): Paperclip as a contained control room, on a disposable VM, as
# root. Every phase is idempotent.
#
#   bash run.sh setup|contain|egress|run|symlink|ws|measure|collect|all
#
# setup    wsm, the users, project s07, the tenant server, the control room
#          image and container (locked from its first start; Paperclip
#          installs the plugin from the image's catalog itself)
# contain  local execution, workspace services, stdio MCP, plugin installs
# egress   nftables for the control room's user, then probes from inside
# run      a run with the file bridge and one with duplex (e2e.sh)
# symlink  a committed symlink into the control room's volume (e2e.sh)
# ws       workspace services again, on the runs' execution workspaces
# measure  memory, disk, image size
# collect  results and logs into /home/test/s07-out for download
#
# The tenant server user s07tenant is in the groups wsm and wsm-admin: a
# stand-in for the manager on this disposable VM only. Never set this up
# anywhere else.
set -uo pipefail
cd /
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"

check_vm() {
	local src t
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
	info vm-size "cpus=$(nproc) mem_total_mib=$(free -m | awk 'NR==2{print $2}') swap_mib=$(free -m | awk '/^Swap/{print $2}') disk=$(df -h / | awk 'NR==2{print $2" total, "$4" free"}') kernel=$(uname -r)"
}

start_server() {
	local i
	systemctl stop "$UNIT" 2>/dev/null
	systemctl reset-failed "$UNIT" 2>/dev/null
	systemd-run --unit="$UNIT" --uid=s07tenant --gid=s07tenant -p UMask=0077 "$TS" serve -config "$CONF" >/dev/null
	for i in $(seq 1 50); do
		[[ -S $SOCKDIR/tenant.sock ]] && return 0
		sleep 0.2
	done
	echo "tenant server did not open its socket" >&2
	journalctl -u "$UNIT" --no-pager | tail -5
	return 1
}

rand_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# cr_secrets creates the pod's own secrets once, root-only in $CR_SECRETS (the
# leak checks read them there), and writes the env files that podman reads as
# the control room's user, and the operator's header file. The object store's
# root key is Paperclip's key: the store serves this one control room.
cr_secrets() {
	local PG_PASSWORD MASTER_KEY TENANT_TOKEN AUTH_SECRET S3_KEY_ID S3_SECRET
	ENVDIR=$(getent passwd "$CRU" | cut -d: -f6)/env
	if [[ ! -s $CR_SECRETS ]]; then
		(umask 077 && printf 'PG_PASSWORD=%s\nMASTER_KEY=%s\n' \
			"$(rand_hex 16)" "$(head -c 32 /dev/urandom | base64 -w0)" >"$CR_SECRETS")
	fi
	grep -q '^TENANT_TOKEN=' "$CR_SECRETS" ||
		printf 'TENANT_TOKEN=%s\nAUTH_SECRET=%s\n' "$(rand_hex 32)" "$(rand_hex 32)" >>"$CR_SECRETS"
	grep -q '^S3_SECRET=' "$CR_SECRETS" ||
		printf 'S3_KEY_ID=%s\nS3_SECRET=%s\n' "S07$(rand_hex 9 | tr a-f A-F)" "$(rand_hex 20)" >>"$CR_SECRETS"
	# shellcheck disable=SC1090
	source "$CR_SECRETS"
	install -d -m 0700 -o "$CRU" -g "$CRU" "$ENVDIR"
	(
		umask 077
		printf 'POSTGRES_PASSWORD=%s\n' "$PG_PASSWORD" >"$ENVDIR/pg.env"
		printf 'ROOT_ACCESS_KEY_ID=%s\nROOT_SECRET_ACCESS_KEY=%s\n' "$S3_KEY_ID" "$S3_SECRET" >"$ENVDIR/s3.env"
		printf 'DATABASE_URL=postgres://paperclip:%s@127.0.0.1:5432/postgres\nPAPERCLIP_SECRETS_MASTER_KEY=%s\nAWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\nPAPERCLIP_CLOUD_TENANT_SERVER_TOKEN=%s\nBETTER_AUTH_SECRET=%s\n' \
			"$PG_PASSWORD" "$MASTER_KEY" "$S3_KEY_ID" "$S3_SECRET" "$TENANT_TOKEN" "$AUTH_SECRET" >"$ENVDIR/cr.env"
		# The operator (wsm) as the stack owner; Paperclip makes the owner
		# instance admin of a cloud instance (enableOwnerInstanceAdmin).
		printf '%s\n' "x-paperclip-cloud-tenant-token: $TENANT_TOKEN" "x-paperclip-cloud-user-id: s07-owner" \
			"x-paperclip-cloud-user-email: owner@example.invalid" "x-paperclip-cloud-user-name: S07 owner" \
			"x-paperclip-cloud-stack-id: $STACK_ID" "x-paperclip-cloud-stack-role: owner" \
			"x-paperclip-cloud-paperclip-company-name: S07 spike" >"$HEADERS"
	)
	chown "$CRU:$CRU" "$ENVDIR"/*.env
}

# PG_SCRIPT runs in the PostgreSQL container ($1: Paperclip's own PostgreSQL
# binaries). It creates the cluster once, with scram passwords (the password
# goes from the env file through a tmpfs file, never argv), and serves only on
# the pod's loopback.
# shellcheck disable=SC2016
PG_SCRIPT='set -e
D=/pgdata/data
if [ ! -s "$D/PG_VERSION" ]; then
	umask 077
	printf "%s" "$POSTGRES_PASSWORD" >/tmp/pw
	"$1/initdb" -D "$D" -U paperclip -A scram-sha-256 --pwfile=/tmp/pw -E UTF8 --locale=C >/tmp/initdb.log 2>&1 || { cat /tmp/initdb.log; exit 1; }
	rm -f /tmp/pw
fi
unset POSTGRES_PASSWORD
exec "$1/postgres" -D "$D" -c listen_addresses=127.0.0.1 -c port=5432 -c unix_socket_directories=/tmp'

start_pg() {
	local i
	cr run -d --pod "$POD" --name "$PG_CTR" --user 1000:1000 \
		--cap-drop=ALL --security-opt=no-new-privileges --read-only \
		--memory=512m --pids-limit=256 \
		-v "$PG_VOL:/pgdata" --env-file "$ENVDIR/pg.env" \
		--entrypoint sh "$IMG" -c "$PG_SCRIPT" sh "$PG_BIN" >>"$STATE/cr-run.log" 2>&1 || return 1
	for i in $(seq 1 90); do
		cr logs "$PG_CTR" 2>&1 | grep -q 'ready to accept connections' && return 0
		[[ $(cr inspect -f '{{.State.Running}}' "$PG_CTR" 2>/dev/null) == true ]] || break
		sleep 1
	done
	echo "postgres did not become ready:" >&2
	cr logs --tail 20 "$PG_CTR" 2>&1 | cut -c1-300 >&2
	return 1
}

# S3_PROBE runs in a throwaway container of the control room image, in the
# pod, with Paperclip's own env file and S3 settings (the script holds no
# secret and comes on stdin). It creates Paperclip's bucket once, then does
# what Paperclip's journal probe does: a put with If-None-Match: * on a new
# key, the same put again and a put with an impossible If-Match. Both repeats
# must fail with 412, or Paperclip refuses the store.
# shellcheck disable=SC2016
S3_PROBE='const s3 = require("/opt/paperclip/lib/node_modules/paperclipai/node_modules/@aws-sdk/client-s3");
const e = process.env;
const c = new s3.S3Client({ region: e.PAPERCLIP_STORAGE_S3_REGION, endpoint: e.PAPERCLIP_STORAGE_S3_ENDPOINT, forcePathStyle: true });
const Bucket = e.PAPERCLIP_STORAGE_S3_BUCKET;
const code = (x) => (x && x.$metadata && x.$metadata.httpStatusCode) || (x && x.name) || String(x);
(async () => {
	let bucket = "exists";
	try { await c.send(new s3.HeadBucketCommand({ Bucket })); }
	catch { await c.send(new s3.CreateBucketCommand({ Bucket })); bucket = "created"; }
	const Key = "s07-conditional-probe-" + Date.now();
	const put = async (extra) => {
		try { return (await c.send(new s3.PutObjectCommand({ Bucket, Key, Body: "s07-probe", ...extra }))).$metadata.httpStatusCode; }
		catch (x) { return code(x); }
	};
	const first = await put({ IfNoneMatch: "*" });
	const again = await put({ IfNoneMatch: "*" });
	const wrong = await put({ IfMatch: "\"s07-impossible-etag\"" });
	await c.send(new s3.DeleteObjectCommand({ Bucket, Key }));
	console.log("bucket=" + bucket + " put=" + first + " repeat-if-none-match=" + again + " if-match-wrong=" + wrong);
})().catch((x) => { console.log("error=" + code(x) + " " + String((x && x.message) || x).slice(0, 200)); process.exit(1); });'

s3_probe() {
	local out=$STATE/s3-init.log line
	line=$(cr run --rm -i --pod "$POD" --user 1000:1000 \
		--cap-drop=ALL --security-opt=no-new-privileges --read-only \
		--memory=256m --pids-limit=64 \
		--env-file "$ENVDIR/cr.env" "${S3_ENV[@]}" \
		--entrypoint node "$IMG" - <<<"$S3_PROBE" 2>&1 | tail -1)
	printf '%s %s\n' "$(ts)" "$line" >>"$out"
	info s3-conditional "$line"
	[[ $line == *" put=200 repeat-if-none-match=412 if-match-wrong=412" ]]
}

start_s3() {
	local i
	cr image exists "$S3_IMG" || cr pull -q "$S3_IMG" >>"$STATE/cr-run.log" 2>&1
	cr run -d --pod "$POD" --name "$S3_CTR" --user 1000:1000 \
		--cap-drop=ALL --security-opt=no-new-privileges --read-only \
		--memory=256m --pids-limit=128 \
		-v "$S3_VOL:/data" --env-file "$ENVDIR/s3.env" \
		"$S3_IMG" --port 127.0.0.1:3900 --region "$S3_REGION" --health /health \
		posix /data >>"$STATE/cr-run.log" 2>&1 || return 1
	for i in $(seq 1 30); do
		cr exec "$S3_CTR" wget -q -O /dev/null http://127.0.0.1:3900/health 2>/dev/null && break
		[[ $(cr inspect -f '{{.State.Running}}' "$S3_CTR" 2>/dev/null) == true ]] || break
		sleep 1
	done
	s3_probe || {
		echo "the object store failed its probe:" >&2
		cr logs --tail 20 "$S3_CTR" 2>&1 | cut -c1-300 >&2
		tail -3 "$STATE/s3-init.log" >&2
		return 1
	}
}

# start_cr (re)creates the control room pod, always locked: the managed config
# and the hidden setting are there from the first start. A restarted tenant
# server needs a new container: a mounted socket keeps the old inode (spike
# 06). The volumes keep the data across restarts.
start_cr() {
	local t0 i v lock=(-e "PAPERCLIP_MANAGED_CONFIG=$MANAGED_CONFIG" -e "PAPERCLIP_HIDDEN_SETTINGS=$HIDDEN_SETTINGS")
	# Authenticated with public exposure, as a Cloud stack; it listens on the
	# pod's address (the pod publishes it on the host's loopback only), and
	# nobody signs up: only the operator's trusted headers get in. The server
	# reads these at every start, over the config that `onboard --yes` writes
	# (which forces local_trusted). The public URL goes in as the managed
	# runtime URL, which only the server reads and which implies the explicit
	# base URL mode: onboard ignores PAPERCLIP_PUBLIC_URL under --yes but not
	# PAPERCLIP_AUTH_BASE_URL_MODE, and writes an invalid config with both.
	local auth=(-e PAPERCLIP_DEPLOYMENT_MODE=authenticated -e PAPERCLIP_DEPLOYMENT_EXPOSURE=public
		-e PAPERCLIP_BIND=lan -e HOST=0.0.0.0 -e PAPERCLIP_MANAGED_RUNTIME_PUBLIC_URL=http://127.0.0.1:3100
		-e PAPERCLIP_AUTH_DISABLE_SIGN_UP=true)
	cr_secrets
	cr pod rm -f -t 30 "$POD" >/dev/null 2>&1
	cr rm -f -t 30 "$CTR" >/dev/null 2>&1
	for v in "$VOL" "$PG_VOL" "$S3_VOL"; do
		cr volume exists "$v" || cr volume create "$v" >/dev/null
	done
	t0=$(date +%s)
	: >"$STATE/cr-run.log"
	cr pod create --name "$POD" --userns=keep-id:uid=1000,gid=1000 \
		--network=pasta -p 127.0.0.1:3100:3100 >>"$STATE/cr-run.log" 2>&1 &&
		start_pg && start_s3 || {
		echo "the pod's database or object store did not start:" >&2
		tail -5 "$STATE/cr-run.log" >&2
		return 1
	}
	info cr-pod "postgres and the object store ready after $(($(date +%s) - t0))s"
	cr run -d --pod "$POD" --name "$CTR" --user 1000:1000 \
		--cap-drop=ALL --security-opt=no-new-privileges \
		--read-only \
		--memory=2g --pids-limit=512 \
		-v "$VOL:/paperclip" \
		-v "$SOCKDIR/tenant.sock:/run/wsm/tenant.sock" \
		--env-file "$ENVDIR/cr.env" \
		"${lock[@]}" "${S3_ENV[@]}" "${auth[@]}" \
		"$IMG" >>"$STATE/cr-run.log" 2>&1 || {
		tail -5 "$STATE/cr-run.log"
		return 1
	}
	rm -f "$STATE/health.json"
	for i in $(seq 1 150); do
		pc_health >"$STATE/health.json" && break
		[[ $(cr inspect -f '{{.State.Running}}' "$CTR" 2>/dev/null) == true ]] || break
		sleep 2
	done
	CR_START=$(($(date +%s) - t0))
	info cr-start "healthy after ${CR_START}s ($(jq -c '{status, deploymentMode, deploymentExposure}' "$STATE/health.json" 2>/dev/null))"
	[[ -s $STATE/health.json ]] || {
		# A malformed managed config or a catalog digest mismatch stops the
		# server before it listens; its log says which.
		echo "control room did not become healthy; last log lines:" >&2
		cr logs --tail 30 "$CTR" 2>&1 | cut -c1-300 >&2
		return 1
	}
}

build_cr() {
	local home ctx digest t0 code
	home=$(getent passwd "$CRU" | cut -d: -f6)
	ctx=$home/ctx
	rm -rf "$ctx"
	install -d -m 0750 -o "$CRU" -g "$CRU" "$ctx"
	cp -r "$SPIKE/image" "$SPIKE/plugin" "$ctx/"
	rm -rf "$ctx/plugin/node_modules" "$ctx/plugin/dist" "$ctx/plugin/.claude"
	chown -R "$CRU:$CRU" "$ctx"
	cr pull -q "$BASE_REPO@$BASE_DIGEST" >"$STATE/base-pull.log" 2>&1 || tail -5 "$STATE/base-pull.log"
	digest=$(cr image inspect -f '{{.Digest}}' "$BASE_REPO@$BASE_DIGEST")
	BASE_REF=$BASE_REPO@$BASE_DIGEST
	info cr-base "$BASE_REF (tag $BASE_TAG on 24 November; inspect digest $digest)"
	t0=$(date +%s)
	cr build --pull=never -t "$IMG" --build-arg "BASE=$BASE_REF" --build-arg "PAPERCLIP_VERSION=$PV" \
		-f "$ctx/image/Containerfile" "$ctx" >"$STATE/cr-build.log" 2>&1
	code=$?
	info cr-build "exit=$code in $(($(date +%s) - t0))s"
	((code == 0)) || tail -30 "$STATE/cr-build.log"
	cr image prune -f >/dev/null 2>&1
	usage build
	return "$code"
}

# wait_plugin waits until Paperclip has installed and started the plugin from
# the image's catalog (the boot does it after the server listens).
wait_plugin() {
	local status i
	for i in $(seq 1 60); do
		status=$(plugin_record | jq -r '.status // empty' 2>/dev/null)
		[[ $status == ready ]] && break
		sleep 2
	done
	info plugin-boot "status=${status:-none} after $((i * 2))s; $(cr logs "$CTR" 2>&1 | grep -oE '(bundled plugin [a-z -]+|Failed to auto-install bundled plugin|Distribution [a-z ]+)' | sort | uniq -c | awk '{c=$1; $1=""; printf "%s x%s;", $0, c}')"
}

phase_setup() {
	local t code lu img
	check_vm
	log "packages"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q >"$STATE/apt.log" 2>&1
	apt-get install -y -q curl jq ca-certificates iproute2 nftables e2fsprogs >>"$STATE/apt.log" 2>&1 || {
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
	usage wsm

	log "users"
	id s07tenant >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin s07tenant
	# Spike-only stand-in for the manager: never do this in production.
	usermod -aG wsm,wsm-admin s07tenant
	ensure_cgroup_delegation
	ensure_rootless_user "$CRU" "$CRUID" || res SETUP-TENANT FAIL "user $CRU"

	log "project $PROJECT"
	bash "$SPIKE/../05-paperclip-provider/image/gen-containerfile.sh" "$SANDBOX_NODE" >"$STATE/Containerfile"
	if ! wsm project list 2>/dev/null | grep -qw "$PROJECT"; then
		wsm project create --slug "$PROJECT" --containerfile "$STATE/Containerfile" >"$STATE/project-create.log" 2>&1 ||
			tail -5 "$STATE/project-create.log"
	fi
	lu=$(project_field linux_user)
	img=$(project_field image)
	if [[ -z $lu || -z $img ]] || ! podman --url "unix:///run/wsm-podman/$lu/podman.sock" image exists "$img" 2>/dev/null; then
		t=$(date +%s)
		wsm project image "$PROJECT" >"$STATE/project-image.log" 2>&1
		code=$?
		info image-build "exit=$code in $(($(date +%s) - t))s"
		((code == 0)) || tail -20 "$STATE/project-image.log"
	fi
	usage project

	log "tenant server"
	install -d -m 0755 -o root -g root /run/s07 /run/s07/tenant /etc/s07
	install -d -m 0750 -o s07tenant -g "$(id -gn "$CRU")" "$SOCKDIR"
	install -d -m 0700 -o s07tenant -g s07tenant /var/lib/s07
	[[ -e $AUDIT ]] || install -m 0600 -o s07tenant -g s07tenant /dev/null "$AUDIT"
	jq -n --arg p "$PROJECT" --argjson uid "$CRUID" --arg dir "$SOCKDIR" --args '{
		audit_path: "/var/lib/s07/audit.jsonl", admin_socket: "/run/wsm/api.sock", podman_socket_dir: "/run/wsm-podman",
		tenants: [{project: $p, control_room_uid: $uid, socket_dir: $dir, max_sandboxes: 3, env_deny: $ARGS.positional}]}' \
		"${ENV_DENY[@]}" >"$CONF.tmp"
	if ! cmp -s "$CONF.tmp" "$CONF" || ! systemctl is-active -q "$UNIT"; then
		mv "$CONF.tmp" "$CONF"
		chmod 0644 "$CONF"
		start_server
		CR_NEEDS_RESTART=1
	fi
	rm -f "$CONF.tmp"

	log "control room image"
	if ! cr image exists "$IMG" || [[ ${REBUILD:-0} == 1 ]]; then
		build_cr || {
			res CR-IMAGE FAIL "build failed: $(grep -m3 -iE 'error|failed' "$STATE/cr-build.log" | cut -c1-200 | tr '\n' ' ')"
			return 1
		}
		CR_NEEDS_RESTART=1
	fi

	log "control room container"
	if [[ ${CR_NEEDS_RESTART:-0} == 1 || $(cr inspect -f '{{.State.Running}}' "$CTR" 2>/dev/null) != true ]]; then
		start_cr || {
			res CR-HEALTH FAIL "no health after the locked start: $(cr logs --tail 5 "$CTR" 2>&1 | tr -c '[:print:]' ' ' | cut -c1-300)"
			return 1
		}
		# The old pod held the previous image until now. Rootless Podman keeps
		# a chowned copy of an image's layers for keep-id (made at its first
		# start), so an old image costs its size twice.
		cr image prune -f >/dev/null 2>&1
	fi
	wait_plugin
	usage started
	check_setup
	usage setup
}

check_setup() {
	local ok notes lu img st pid user socks tcp mode
	# SETUP-WSM
	ok=1 notes=()
	systemctl is-active --quiet wsm || {
		ok=0
		notes+=("wsm not active")
	}
	lu=$(project_field linux_user)
	img=$(project_field image)
	if [[ -z $lu || -z $img ]] || ! podman --url "unix:///run/wsm-podman/$lu/podman.sock" image exists "$img" 2>/dev/null; then
		ok=0
		notes+=("$PROJECT: no image ($lu $img)")
	fi
	if ((ok)); then res SETUP-WSM PASS "wsm active; project $PROJECT image $img present (user $lu)"; else res SETUP-WSM FAIL "${notes[*]}"; fi

	# SETUP-TENANT
	ok=1 notes=()
	pid=$(systemctl show -p MainPID --value "$UNIT")
	user=$(ps -o user= -p "$pid" 2>/dev/null)
	[[ $user == s07tenant ]] || {
		ok=0
		notes+=("server user=${user:-none}")
	}
	mode=$(stat -c '%U:%G:%a' "$SOCKDIR")
	[[ $mode == "s07tenant:$(id -gn "$CRU"):750" ]] || {
		ok=0
		notes+=("dir=$mode")
	}
	socks=$(stat -c '%a' "$SOCKDIR/tenant.sock" 2>/dev/null)
	[[ $socks == 666 ]] || {
		ok=0
		notes+=("socket mode=${socks:-missing}")
	}
	tcp=$(ss -Hltnp 2>/dev/null | grep -c "pid=$pid,")
	((tcp == 0)) || {
		ok=0
		notes+=("$tcp TCP listeners")
	}
	if ((ok)); then res SETUP-TENANT PASS "$UNIT as s07tenant; dir $mode; socket 666; no TCP listener"; else res SETUP-TENANT FAIL "${notes[*]}"; fi

	check_image
	check_harden
	check_no_host
	check_identity
	check_health
	check_plugin
}

check_image() {
	local base ver ro catalog root
	base=$(cr image inspect -f '{{index .Labels "s07.base"}}' "$IMG" 2>/dev/null)
	ver=$(crx paperclipai --version 2>/dev/null | tr -d '\r')
	ro=$(crx sh -c "test -w $PLUGIN_DIR/dist/worker.js && echo writable || echo read-only; stat -c '%U:%a' $PLUGIN_DIR/dist/worker.js" 2>&1 | tr '\n' ' ')
	catalog=$(crx cat "$PLUGIN_ROOT/distribution/catalog.json" 2>/dev/null | jq -c --arg id "$PLUGIN_ID" '.plugins[] | select(.key=="wsm" and .pluginKey==$id) | {key, pluginKey, version, digest}' 2>/dev/null)
	root=$(crx printenv PAPERCLIP_BUNDLED_PLUGIN_ROOT 2>/dev/null)
	if [[ $base == "$BASE_REPO@sha256:"* && $ver == "$PV" && $ro == read-only* && $catalog == *'"digest":"sha256:'* && $root == "$PLUGIN_ROOT" ]]; then
		res CR-IMAGE PASS "base $base; paperclip $ver; plugin $ro; catalog $catalog under $root"
	else
		res CR-IMAGE FAIL "base=${base:-none} paperclip=${ver:-none} plugin=$ro catalog=${catalog:-none} root=${root:-unset}"
	fi
	info plugin-catalog "$(crx cat "$BUILD_INFO/catalog.txt" 2>/dev/null)"
	info cr-image-size "$(cr image inspect -f '{{.Size}}' "$IMG" | awk '{printf "%.0f MiB", $1/1048576}')"
	info cr-tsx-shim "managed plugin runtime loader copied into place: $(crx cat /opt/paperclip/.s07-tsx-loader.txt 2>/dev/null)"
	info cr-pod-images "postgres: $(cr exec "$PG_CTR" "$PG_BIN/postgres" --version 2>/dev/null) from the control room image; object store: $S3_IMG ($(cr image inspect -f '{{.Size}}' "$S3_IMG" 2>/dev/null | awk '{printf "%.0f MiB", $1/1048576}'), $(cr exec "$S3_CTR" versitygw --version 2>/dev/null | tr '\n' ' ' | cut -c1-120))"
}

check_harden() {
	local j capeff nnp ro mem pids mounts ports ok=1 notes=()
	j=$(cr inspect "$CTR" 2>/dev/null)
	capeff=$(crx grep -m1 CapEff /proc/1/status | awk '{print $2}')
	nnp=$(crx grep -m1 NoNewPrivs /proc/1/status | awk '{print $2}')
	ro=$(jq -r '.[0].HostConfig.ReadonlyRootfs' <<<"$j")
	mem=$(jq -r '.[0].HostConfig.Memory' <<<"$j")
	pids=$(jq -r '.[0].HostConfig.PidsLimit' <<<"$j")
	mounts=$(jq -r '[.[0].Mounts[] | "\(.Type):\(.Name // .Source)->\(.Destination)"] | sort | join(" ")' <<<"$j")
	# The pod publishes the port.
	ports=$(cr pod inspect "$POD" 2>/dev/null | jq -r '(if type=="array" then .[0] else . end)
		| [.InfraConfig.PortBindings // {} | to_entries[] | .value[]? | "\(.HostIp):\(.HostPort)"] | join(" ")')
	[[ $capeff == 0000000000000000 ]] || { ok=0; notes+=("CapEff=$capeff"); }
	[[ $nnp == 1 ]] || { ok=0; notes+=("NoNewPrivs=$nnp"); }
	[[ $ro == true ]] || { ok=0; notes+=("ReadonlyRootfs=$ro"); }
	((mem > 0 && pids > 0)) || { ok=0; notes+=("memory=$mem pids=$pids"); }
	[[ $mounts == "bind:$SOCKDIR/tenant.sock->/run/wsm/tenant.sock volume:$VOL->/paperclip" ]] || { ok=0; notes+=("mounts=[$mounts]"); }
	[[ $ports == "127.0.0.1:3100" ]] || { ok=0; notes+=("ports=[$ports]"); }
	# The pod's other containers, read from the host: no capabilities, no new
	# privileges, a read-only root, limits, and only their own volume.
	local c name want sj spid st smounts side=()
	for c in "$PG_CTR=volume:$PG_VOL->/pgdata" "$S3_CTR=volume:$S3_VOL->/data"; do
		name=${c%%=*}
		want=${c#*=}
		sj=$(cr inspect "$name" 2>/dev/null)
		spid=$(jq -r '.[0].State.Pid // 0' <<<"$sj")
		st=$(awk '/^CapEff:/{c=$2} /^NoNewPrivs:/{n=$2} END{printf "%s/%s", c, n}' "/proc/$spid/status" 2>/dev/null)
		smounts=$(jq -r '[.[0].Mounts[] | select((.Destination | startswith("/run/secrets/")) | not) | "\(.Type):\(.Name // .Source)->\(.Destination)"] | sort | join(" ")' <<<"$sj")
		side+=("$name:$st:ro=$(jq -r '.[0].HostConfig.ReadonlyRootfs' <<<"$sj"):mem=$(($(jq -r '.[0].HostConfig.Memory // 0' <<<"$sj") >> 20))MiB:[$smounts]")
		[[ $st == 0000000000000000/1 && $(jq -r '.[0].HostConfig.ReadonlyRootfs' <<<"$sj") == true && $(jq -r '.[0].HostConfig.Memory // 0' <<<"$sj") -gt 0 && $smounts == "$want" ]] || {
			ok=0
			notes+=("$name: caps/nnp=$st mounts=[$smounts]")
		}
	done
	if ((ok)); then
		res CR-HARDEN PASS "CapEff=0 NoNewPrivs=1 ReadonlyRootfs memory=$((mem >> 20))MiB pids=$pids mounts=[$mounts] port=$ports (pod); ${side[*]}"
	else
		res CR-HARDEN FAIL "${notes[*]}"
	fi
	crx sh -c 'grep -E " / | /run| /tmp| /var/tmp| /paperclip| /dev/shm" /proc/self/mountinfo | cut -d" " -f5,9,10 | sort' >"$RESULTS/cr-mounts.txt" 2>&1
}

check_no_host() {
	local out
	out=$(crx sh -c '
		for p in /run/wsm-podman /run/wsm/api.sock /run/podman /var/run/docker.sock /run/user; do [ -e "$p" ] && echo "present:$p"; done
		command -v podman >/dev/null && echo "present:podman-binary"
		[ -n "${CONTAINER_HOST:-}" ] && echo "present:CONTAINER_HOST"
		find / \( -path /proc -o -path /sys \) -prune -o -type s -print 2>/dev/null | sort | sed "s/^/socket:/"' 2>&1)
	printf '%s\n' "$out" >"$RESULTS/cr-no-host.txt"
	# Postgres's own socket in /tmp belongs to the control room; anything else
	# besides the tenant socket would come from the host.
	if ! grep -q '^present:' <<<"$out" && ! grep '^socket:' <<<"$out" | grep -vqE '^socket:(/run/wsm/tenant\.sock|/tmp/\.s\.PGSQL\.[0-9]+)$'; then
		res CR-NO-HOST PASS "no podman socket, api.sock, podman binary or CONTAINER_HOST; sockets: $(grep '^socket:' <<<"$out" | cut -d: -f2 | tr '\n' ' ')"
	else
		res CR-NO-HOST FAIL "$(tr '\n' ' ' <<<"$out" | cut -c1-400)"
	fi
}

check_identity() {
	local out
	out=$(crx node -e '
		const http = require("node:http");
		http.get({ socketPath: "/run/wsm/tenant.sock", path: "/v1/tenant/whoami", host: "tenant" }, (r) => {
			let b = ""; r.on("data", (c) => (b += c)); r.on("end", () => console.log(r.statusCode, b.trim()));
		}).on("error", (e) => console.log("ERR", e.code));' 2>&1)
	if [[ $out == "200 "* ]] && jq -e --argjson uid "$CRUID" --arg p "$PROJECT" '.project==$p and .peer_uid==$uid' <<<"${out#200 }" >/dev/null 2>&1; then
		res CR-IDENTITY PASS "whoami 200 project=$PROJECT peer_uid=$CRUID (host uid of $CRU)"
	else
		res CR-IDENTITY FAIL "whoami: $(cut -c1-200 <<<"$out")"
	fi
}

check_health() {
	local health listen other
	health=$(pc_health)
	listen=$(ss -Hltn 'sport = :3100' | awk '{print $4}' | sort -u | tr '\n' ' ')
	other=$(curl -sS -m 3 -o /dev/null -w '%{http_code}' "http://$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+'):3100/api/health" 2>/dev/null)
	if jq -e '.status=="ok"' <<<"$health" >/dev/null 2>&1 && [[ $listen == "127.0.0.1:3100 " && $other == 000 ]]; then
		res CR-HEALTH PASS "health ok ($(jq -r .deploymentMode <<<"$health")); 3100 only on $listen; host address -> $other"
	else
		res CR-HEALTH FAIL "health=$(head -c 120 <<<"$health") 3100 on: $listen host address -> $other"
	fi
}

check_plugin() {
	local status tap total missing name rec others
	status=$(crx cat "$BUILD_INFO/status.txt" 2>/dev/null)
	if [[ $status == "npm-ci=0 typecheck=0 build=0 test="* ]]; then
		res PLUGIN-BUILD PASS "${status% test=*}"
	else
		res PLUGIN-BUILD FAIL "status=[$status] $(crx tail -5 "$BUILD_INFO/typecheck.log" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
	fi
	mkdir -p "$RESULTS/plugin"
	crx cat "$BUILD_INFO/test.tap" >"$RESULTS/plugin/test.tap" 2>/dev/null
	tap=$RESULTS/plugin/test.tap
	missing=()
	while IFS= read -r name; do
		grep -qE "^ok [0-9]+ - ${name}\$" "$tap" || missing+=("$name")
	done < <(grep -hoE 'test\("S07 [^"]+"' "$SPIKE"/plugin/test/*.test.ts | sed -E 's/^test\("//; s/"$//')
	total=$(grep -hoE 'test\("S07 [^"]+"' "$SPIKE"/plugin/test/*.test.ts | wc -l)
	if [[ $status == *" test=0" ]] && grep -q '^# fail 0$' "$tap" && grep -q '^# skipped 0$' "$tap" && ((${#missing[@]} == 0)); then
		res PLUGIN-UNIT PASS "# fail 0, # skipped 0, all $total S07 tests ok (with the tenant transport tests)"
	else
		res PLUGIN-UNIT FAIL "status=[$status] $(grep -E '^# (pass|fail|skipped)' "$tap" | tr '\n' ' ') missing: ${missing[*]}"
	fi
	# Ready, serving the wsm driver, installed by the boot from the image's
	# catalog (never through the API), and the only plugin.
	rec=$(plugin_record)
	pc -fsS "$API/plugins" >"$STATE/plugins.json" 2>/dev/null
	others=$(jq -r --arg id "$PLUGIN_ID" '[.. | objects | select(has("pluginKey") and has("status")) | .pluginKey | select(. != $id)] | unique | join(",")' "$STATE/plugins.json" 2>/dev/null)
	if [[ $(jq -r '.status // empty' <<<"$rec") == ready ]] && grep -q '"driverKey":"wsm"' <<<"$rec$(cat "$STATE/plugins.json")" &&
		[[ $(jq -r '.packagePath // empty' <<<"$rec") == "$PLUGIN_DIR"* && -z $others ]]; then
		res PLUGIN-READY PASS "status=ready driver=wsm packagePath=$(jq -r .packagePath <<<"$rec") (auto-installed from the catalog); no other plugin"
	else
		res PLUGIN-READY FAIL "status=$(jq -r '.status // "none"' <<<"$rec" 2>/dev/null) packagePath=$(jq -r '.packagePath // "none"' <<<"$rec" 2>/dev/null) others=[${others}] record=$(head -c 300 <<<"$rec")"
	fi
}

# probe_script prints the node program that probes one destination from
# inside the control room: "<name> ok <detail>" or "<name> fail <code>".
probe_script() {
	cat <<'EOF'
const net = require("node:net");
const https = require("node:https");
const dns = require("node:dns");
const [kind, name, host, port] = process.argv.slice(1);
const done = (ok, d) => { console.log(`${name} ${ok ? "ok" : "fail"} ${d}`); process.exit(0); };
setTimeout(() => done(false, "timeout"), 9000).unref();
if (kind === "https") {
  https.get({ host, port: Number(port), path: "/", timeout: 6000, servername: host }, (r) => done(true, `status=${r.statusCode}`))
    .on("timeout", function () { this.destroy(); done(false, "timeout"); })
    .on("error", (e) => done(false, e.code || e.message));
} else if (kind === "tcp") {
  const s = net.connect({ host, port: Number(port), timeout: 6000 }, () => { s.destroy(); done(true, "connected"); });
  s.on("timeout", () => { s.destroy(); done(false, "timeout"); });
  s.on("error", (e) => done(false, e.code || e.message));
} else if (kind === "dns") {
  const r = new dns.promises.Resolver({ timeout: 3000, tries: 1 });
  r.resolve4(host).then((a) => done(true, `answers=${a.length}`), (e) => done(false, e.code || e.message));
}
EOF
}

drop_count() { nft -j list table inet s07 2>/dev/null | jq '[.nftables[] | .rule? // empty | .expr[]? | .counter? // empty | .packets] | add // 0'; }

# run_probes <out-file> probes every destination in $STATE/probes.txt.
run_probes() {
	local kind name dest port out
	: >"$1"
	while read -r kind name dest port; do
		# exec -i would read the rest of the probe list from this loop's stdin.
		out=$(crx node -e "$(probe_script)" "$kind" "$name" "$dest" "$port" </dev/null 2>&1 | tail -1)
		printf '%s %s:%s %s\n' "$kind" "$dest" "$port" "$out" >>"$1"
	done <"$STATE/probes.txt"
}

phase_egress() {
	local npm_ip host_ip before after allow deny_ok=1 line
	npm_ip=$(getent ahostsv4 registry.npmjs.org | awk 'NR==1{print $1}')
	host_ip=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')
	{
		printf 'https allow %s %s\n' "$ALLOW_IP" "$ALLOW_PORT"
		printf 'tcp npm-registry %s 443\n' "$npm_ip"
		printf 'tcp other-public 9.9.9.9 443\n'
		printf 'tcp lan-gateway 192.168.0.1 22\n'
		printf 'tcp wsm-mcp 10.255.0.1 7443\n'
		printf 'tcp host-ssh %s 22\n' "$host_ip"
		printf 'tcp host-dashboard 169.254.1.2 8080\n'
		printf 'tcp host-preview 169.254.1.2 8081\n'
		printf 'dns resolver registry.npmjs.org 53\n'
	} >"$STATE/probes.txt"
	log "probes without rules"
	nft list table inet s07 >/dev/null 2>&1 && nft delete table inet s07
	run_probes "$RESULTS/egress-before.txt"
	info egress-before "without the s07 rules: $(awk '{print $3"="$4}' "$RESULTS/egress-before.txt" | tr '\n' ' ')"
	log "egress rules"
	nft -f - <<EOF
table inet s07 {
	chain out {
		type filter hook output priority filter; policy accept;
		meta skuid $CRUID ct state established,related accept
		meta skuid $CRUID ip daddr $ALLOW_IP tcp dport $ALLOW_PORT accept
		meta skuid $CRUID counter drop
	}
}
EOF
	nft list table inet s07 >"$RESULTS/nft-s07.txt"
	before=$(drop_count)
	log "egress probes"
	run_probes "$RESULTS/egress-probes.txt"
	after=$(drop_count)
	allow=$(grep -E '^https .* allow ok ' "$RESULTS/egress-probes.txt")
	while read -r line; do
		grep -q ' fail ' <<<"$line" || deny_ok=0
	done < <(grep -v ' allow ' "$RESULTS/egress-probes.txt")
	if [[ -n $allow ]]; then
		res EGRESS-ALLOW PASS "$(cut -d' ' -f2- <<<"$allow")"
	else
		res EGRESS-ALLOW FAIL "$(grep ' allow ' "$RESULTS/egress-probes.txt")"
	fi
	if ((deny_ok)) && ((after > before)); then
		res EGRESS-DENY PASS "$(grep -vc ' allow ' "$RESULTS/egress-probes.txt") destinations refused; drop counter $before -> $after; $(grep -v ' allow ' "$RESULTS/egress-probes.txt" | awk '{print $3"="$5}' | tr '\n' ' ')"
	else
		res EGRESS-DENY FAIL "drop counter $before -> $after; $(grep -v ' allow ' "$RESULTS/egress-probes.txt" | tr '\n' ';')"
	fi
	# Does Paperclip come back without DNS or internet?
	log "restart under the egress rules"
	start_cr && wait_plugin
	info paperclip-net-at-start "restart under egress rules: $(pc_health | jq -c '{status}' 2>/dev/null || echo unhealthy) plugin=$(plugin_record | jq -r '.status // "none"' 2>/dev/null)"
	usage egress
}

# check_audit: TENANT-AUDIT. Every call is one audit line from the control
# room's UID; every allowed exec has an end line; creates and deletes match the
# plugin's lease lines; the hash chain holds.
check_audit() {
	local verify uids creates deletes acq rel allows ends
	verify=$("$TS" audit-verify "$AUDIT" 2>&1 | tail -1)
	uids=$(jq -r '.peer_uid' "$AUDIT" | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}')
	creates=$(jq -c 'select(.route=="create" and .decision=="allow")' "$AUDIT" | wc -l)
	deletes=$(jq -c 'select(.route=="delete" and .decision=="allow")' "$AUDIT" | wc -l)
	allows=$(jq -c 'select(.route=="exec" and .decision=="allow")' "$AUDIT" | wc -l)
	ends=$(jq -c 'select(.route=="exec" and .decision=="end")' "$AUDIT" | wc -l)
	cr logs "$CTR" 2>&1 | grep -F '[plugin]' >"$STATE/plugin-all.log"
	acq=$(grep -c 's07 lease acquire sandbox=' "$STATE/plugin-all.log")
	rel=$(grep -cE 's07 lease (release|destroy) sandbox=t[a-z0-9]{10} result=deleted' "$STATE/plugin-all.log")
	if [[ $verify == *ok* && $uids == "$CRUID:"*" " && $uids != *" "*:* && $allows == "$ends" ]] && ((creates >= acq && deletes >= rel)); then
		res TENANT-AUDIT PASS "audit-verify: $verify; peers: $uids; creates=$creates (plugin acquires $acq) deletes=$deletes (releases $rel); exec allow=$allows end=$ends"
	else
		res TENANT-AUDIT FAIL "audit-verify: $verify; peers: $uids; creates=$creates/acq=$acq deletes=$deletes/rel=$rel exec allow=$allows end=$ends"
	fi
	info exec-count "$(jq -r 'select(.route=="exec" and .decision=="allow") | .sandbox' "$AUDIT" | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}')"
	info env-dropped "$(jq -r 'select(.exec.env_dropped != null) | .exec.env_dropped[]' "$AUDIT" | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}')"
	# Over the whole run, the containment checks' process agent included: execs
	# that passed a name of the control room's own secrets on to a sandbox.
	info audit-server-names "execs that passed a cr.env name on: $(jq -c --args 'select(.route=="exec" and .decision=="allow" and .exec.env_names != null)
		| select(any(.exec.env_names[]; . as $n | $ARGS.positional | index($n)))' $(sed -n 's/=.*//p' "$ENVDIR/cr.env") <"$AUDIT" | wc -l) of $allows"
}

phase_measure() {
	check_audit
	info cr-memory-now "current/peak MiB since the last pod start: paperclip=$(ctr_mem "$CTR") postgres=$(ctr_mem "$PG_CTR") s3=$(ctr_mem "$S3_CTR")"
	info cr-volumes "$(for v in "$VOL" "$PG_VOL" "$S3_VOL"; do printf '%s=%s ' "$v" "$(du -sh "$(cr volume inspect -f '{{.Mountpoint}}' "$v" 2>/dev/null)" 2>/dev/null | cut -f1)"; done)"
	info disk-used "$(df -h / | awk 'NR==2{print "used="$3" free="$4" of "$2}'); containers: cr=$(du -sh "$(getent passwd "$CRU" | cut -d: -f6)/.local/share/containers" 2>/dev/null | cut -f1) project=$(du -sh "$(getent passwd "$(project_field linux_user)" | cut -d: -f6)/.local/share/containers" 2>/dev/null | cut -f1)"
	info host-memory "$(free -m | awk 'NR==2{print "used="$3"MiB available="$7"MiB of "$2"MiB"}')"
	usage measure
}

phase_collect() {
	local out=/home/test/s07-out fails
	journalctl -u "$UNIT" --no-pager >"$RESULTS/server.log" 2>&1
	cp "$AUDIT" "$RESULTS/audit.jsonl"
	"$TS" audit-verify "$AUDIT" >"$RESULTS/audit-verify.txt" 2>&1
	# Only the plugin's own lines from the control room's log.
	cr logs "$CTR" 2>&1 | grep -F '[plugin]' | grep -F 's07 ' | cut -c1-400 >"$RESULTS/plugin-log.txt"
	cr logs --tail 40 "$PG_CTR" >"$RESULTS/postgres-log.txt" 2>&1
	cr logs --tail 40 "$S3_CTR" >"$RESULTS/s3-log.txt" 2>&1
	cp "$STATE/s3-init.log" "$RESULTS/" 2>/dev/null
	collect_versions "$RESULTS/versions.txt"
	{
		echo "## podman"
		podman --version
		echo "## wsm"
		wsm version 2>&1 | head -3
		echo "## control room"
		crx paperclipai --version
		crx node --version
	} >>"$RESULTS/versions.txt" 2>&1
	scrub_results
	fails=$(failures)
	echo "fails=$fails" >"$RESULTS/status.txt"
	rm -rf "$out"
	mkdir -p "$out"
	cp -r "$RESULTS" "$out/"
	cp /root/s07-console.log "$out/console.log" 2>/dev/null
	chown -R test:test "$out"
	log "collected into $out (fails=$fails)"
}

# scrub_results drops every line that holds a sentinel or secret value from
# results/, and records how many lines it dropped. Values never leave the VM.
scrub_results() {
	local f n=0 pat=$STATE/secret-values
	[[ -s $pat ]] || return 0
	while IFS= read -r f; do
		grep -vF -f "$pat" "$f" >"$f.tmp"
		mv "$f.tmp" "$f"
		n=$((n + 1))
	done < <(grep -rlF -f "$pat" "$RESULTS")
	info results-scrubbed "files with a secret value removed from results: $n"
}

case ${1:-} in
setup) phase_setup ;;
pod)
	# Recreate the control room pod from the current image and volumes.
	start_cr && wait_plugin
	cr image prune -f >/dev/null 2>&1
	usage pod
	;;
contain) bash "$HERE/contain.sh" ;;
egress) phase_egress ;;
run)
	bash "$HERE/e2e.sh" file
	bash "$HERE/e2e.sh" duplex
	;;
symlink)
	bash "$HERE/e2e.sh" symlink-1
	bash "$HERE/e2e.sh" symlink-2
	;;
ws) bash "$HERE/contain.sh" ws ;;
measure) phase_measure ;;
collect) phase_collect ;;
all)
	phase_setup
	bash "$HERE/contain.sh"
	phase_egress
	bash "$HERE/e2e.sh" file
	bash "$HERE/e2e.sh" duplex
	bash "$HERE/e2e.sh" symlink-1
	bash "$HERE/e2e.sh" symlink-2
	bash "$HERE/contain.sh" ws
	phase_measure
	phase_collect
	;;
*)
	echo "usage: run.sh setup|pod|contain|egress|run|symlink|ws|measure|collect|all" >&2
	exit 2
	;;
esac
