#!/bin/sh
# Collect a HiDeck debug-field run into one archive to send back.
#
#   sh debug/collect.sh --since "2026-11-04 08:00" [--logs /path/to/logs] [--service hideck]
#
# Packs the app logs and inner IMS captures written since --since, the
# kernel's USB/modem lines, carrier overrides, system info and a summary.
# Every run of 11 or more digits is masked in every text file (IMSI, ICCID,
# IMEI, MSISDN; 10-digit SPIs and timestamps stay), and the archive is not
# written if anything that looks like one is left. config.yaml is never
# included: it holds passwords and tokens.
set -eu

SINCE=
LOGS=
SERVICE=hideck
while [ $# -gt 0 ]; do
	case $1 in
	--since) SINCE=${2:-}; shift 2 ;;
	--logs) LOGS=${2:-}; shift 2 ;;
	--service) SERVICE=${2:-}; shift 2 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done
[ -n "$SINCE" ] || { echo 'usage: sh debug/collect.sh --since "YYYY-MM-DD HH:MM"' >&2; exit 2; }
date -d "$SINCE" >/dev/null 2>&1 || { echo "--since is not a date: $SINCE" >&2; exit 2; }
SINCE_DAY=$(date -d "$SINCE" +%Y-%m-%d)

if [ -z "$LOGS" ]; then
	dir=$(systemctl show -p WorkingDirectory --value "$SERVICE" 2>/dev/null || true)
	for candidate in "${dir:+$dir/logs}" ./logs /app/logs; do
		if [ -n "$candidate" ] && [ -d "$candidate" ]; then LOGS=$candidate; break; fi
	done
fi
[ -n "$LOGS" ] && [ -d "$LOGS" ] || { echo "HiDeck logs directory not found; pass --logs" >&2; exit 2; }
ls "$LOGS"/app-*.log >/dev/null 2>&1 || { echo "no app-*.log in $LOGS; pass --logs" >&2; exit 2; }

NAME=hideck-debug-$(date +%Y%m%d-%H%M%S)
OUT=$(mktemp -d)/$NAME
mkdir -p "$OUT/logs" "$OUT/pcap"
trap 'rm -rf "$(dirname "$OUT")"' EXIT

mask() { sed -E 's/[0-9]{11,}/<masked>/g; s/(imei:)[0-9A-Fa-f-]+/\1<masked>/g'; }

# App logs: every daily file from the --since day on, lines from --since on.
for file in "$LOGS"/app-*.log; do
	day=$(basename "$file" .log | sed 's/^app-//')
	if expr "x$day" \< "x$SINCE_DAY" >/dev/null; then continue; fi
	awk -v since="$(date -d "$SINCE" '+[%Y-%m-%d %H:%M:%S]')" 'substr($0,1,21) >= since || started { started=1; print }' "$file" |
		mask >"$OUT/logs/$(basename "$file")"
done

# Inner IMS captures: headers only, but inner IPv6 addresses are kept.
if [ -d "$LOGS/dev-pcap" ]; then
	find "$LOGS/dev-pcap" -name '*.pcapng' -newermt "$SINCE" -exec cp {} "$OUT/pcap/" \;
fi

{
	echo "collected: $(date '+%F %T %z')"
	echo "since: $SINCE"
	echo "logs: $LOGS"
	uname -a
	grep -h 'debug-field build' "$OUT"/logs/*.log | tail -1 || true
	echo
	command -v lsusb >/dev/null && lsusb
	echo
	ls -l /dev/cdc-wdm* /dev/ttyUSB* 2>/dev/null || true
} 2>&1 | mask >"$OUT/system.txt"

if command -v journalctl >/dev/null; then
	journalctl -k --since "$SINCE" --no-pager 2>/dev/null
else
	dmesg 2>/dev/null
fi | grep -iE 'usb|cdc|qmi|wwan|option|ttyUSB|qcserial' | mask >"$OUT/kernel.txt" || true

for overrides in "$(dirname "$LOGS")/config/carrier_overrides.yaml" ./config/carrier_overrides.yaml; do
	if [ -f "$overrides" ]; then mask <"$overrides" >"$OUT/carrier_overrides.yaml"; break; fi
done

# Summary: counts and a timeline of the lines that usually matter.
count() { cat "$OUT"/logs/*.log | grep -cF -- "$1" || true; }
{
	echo "== counts since $SINCE"
	for pattern in \
		'IMS REGISTER succeeded' 'IMS REGISTER failed' 'IMS SIP tx' 'IMS SIP rx' \
		'IMS conn opened' 'IMS conn read failed' 'IMS conn closed' \
		'port-s lifecycle' 'peer_reset' 'SUBSCRIBE(reg)' 'SUBSCRIBE(mwi)' \
		'peer deleted' 'IKE_SA' 'CHILD_SA' '[dev] IKE rx' '[dev] IKE tx' \
		'[dev] AT command' '[dev] QMI SetOperatingMode' \
		'Timeout storm detected' 'USB 控制面' '清理 Worker' 'panic' ' ERROR '; do
		printf '%8s  %s\n' "$(count "$pattern")" "$pattern"
	done
	echo
	echo "== SIP responses by status"
	cat "$OUT"/logs/*.log | grep -oE '"first_line": "SIP/2.0 [0-9]{3}[^"]*' | sed 's/"first_line": "//' | sort | uniq -c | sort -rn | head -30
	echo
	echo "== SIP requests by method and direction"
	cat "$OUT"/logs/*.log | grep -oE 'IMS SIP [rt]x .*"first_line": "[A-Z]{3,10} ' | sed -E 's/.*IMS SIP ([rt]x) .*"first_line": "([A-Z]+) /\1 \2/' | sort | uniq -c | sort -rn
	echo
	echo "== timeline (connection failures, registrations, recoveries; last 200)"
	cat "$OUT"/logs/*.log | grep -E 'IMS conn (read failed|closed)|REGISTER (succeeded|failed)|port-s lifecycle|peer deleted|Timeout storm|USB 控制面|清理 Worker|panic| ERROR ' | cut -c1-400 | tail -200
} >"$OUT/summary.txt"

# Refuse to pack anything that still looks like an identity.
leaks=$(grep -rIlE '[0-9]{11,}|imei:[0-9]{6}' "$OUT" --exclude-dir=pcap || true)
if [ -n "$leaks" ]; then
	echo "unmasked digit runs remain in:" >&2
	echo "$leaks" >&2
	echo "nothing was written; please report this" >&2
	exit 1
fi

ARCHIVE=$PWD/$NAME.tar.gz
tar -C "$(dirname "$OUT")" -czf "$ARCHIVE" "$NAME"
echo "written: $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1))"
echo "summary:"
head -25 "$OUT/summary.txt"
