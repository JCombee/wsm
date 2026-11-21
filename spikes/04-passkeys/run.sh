#!/usr/bin/env bash
# Spike 04-passkeys: run pkspike with headless Chromium on a disposable VM.
# Usage (as root): bash run.sh  (pkspike must sit next to this script)
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../lib.sh
source "$HERE/../lib.sh"
guard

results_init "$HERE/results"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q >"$RESULTS/apt.log" 2>&1
apt-get install -y -q chromium libnss3-tools >>"$RESULTS/apt.log" 2>&1 || { report SETUP FAIL "apt-get install"; exit 1; }
note "$(chromium --version 2>&1)"

id pkspike >/dev/null 2>&1 || useradd -m -s /bin/bash pkspike
install -d -m 0755 /opt/pkspike
install -m 0755 "$HERE/pkspike" /opt/pkspike/pkspike
asPK() { (cd / && runuser -u pkspike -- env HOME=/home/pkspike "$@"); }

rm -rf /home/pkspike/certs /home/pkspike/.pki
asPK /opt/pkspike/pkspike gencert -dir /home/pkspike/certs
asPK bash -c 'mkdir -p ~/.pki/nssdb && certutil -d sql:$HOME/.pki/nssdb -N --empty-password &&
	certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n wsm-spike04 -i ~/certs/ca.pem'

asPK /opt/pkspike/pkspike run -certs /home/pkspike/certs -mode nss >"$RESULTS/run.out" 2>"$RESULTS/server.log"
note "pkspike exit $?"

# Lines are RESULT<TAB>ID<TAB>PASS|FAIL|INFO<TAB>detail.
while IFS=$'\t' read -r _ id status detail; do
	case $status in
	PASS | FAIL) report "$id" "$status" "$detail" ;;
	*) note "$id $detail" ;;
	esac
done < <(grep -P '^RESULT\t' "$RESULTS/run.out")

(($(failures) == 0))
