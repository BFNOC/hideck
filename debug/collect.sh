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
	# Timestamped lines decide; untimestamped lines (stack traces, the startup
	# banner) follow the line before them.
	awk -v since="$(date -d "$SINCE" '+%Y-%m-%d %H:%M:%S')" '
		/^\[[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] / { keep = substr($0, 2, 19) >= since }
		keep { print }' "$file" |
		mask >"$OUT/logs/$(basename "$file")"
done

# Inner IMS captures: headers only, but inner IPv6 addresses are kept. The
# service keeps at most 24 files of 32 MB, so take all of them: a long-lived
# tunnel's file can start long before --since. A file still being written may
# end in a partial block; Wireshark reads everything before it. A file rotated
# away during the copy is skipped and listed.
if [ -d "$LOGS/dev-pcap" ]; then
	for file in "$LOGS"/dev-pcap/ims-inner-*.pcapng; do
		[ -f "$file" ] || continue
		cp "$file" "$OUT/pcap/" 2>/dev/null || echo "$(basename "$file")" >>"$OUT/pcap-skipped.txt"
	done
fi

{
	echo "collected: $(date '+%F %T %z')"
	echo "since: $SINCE"
	echo "logs: $LOGS"
	echo "host timezone: $(date +%Z%z)"
	uname -srvm
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

# Host network events (Wi-Fi, DHCP, link up/down) that could explain a stall.
# MAC addresses and SSIDs are masked.
if command -v journalctl >/dev/null; then
	{
		journalctl --since "$SINCE" --no-pager -u NetworkManager -u wpa_supplicant -u systemd-networkd -u dhcpcd -u iwd 2>/dev/null
		journalctl -k --since "$SINCE" --no-pager 2>/dev/null | grep -iE 'wlan|wifi|link (is )?(up|down)|carrier|deauth|disassoc|eth[0-9]|enp|wlp'
	} | mask | sed -E 's/([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}/<mac>/g; s/([0-9A-Fa-f]{2}-){5}[0-9A-Fa-f]{2}/<mac>/g; s/'"'"'[^'"'"']*'"'"'/<name>/g; s/"[^"]*"/<name>/g; s/(SSID|ssid)([=: ]+)[^ ,]+/\1\2<ssid>/g' >"$OUT/network.txt" || true
	echo "(host journal, host clock $(date +%Z%z); app logs use HiDeck's clock)" >>"$OUT/network.txt"
fi

for overrides in "$(dirname "$LOGS")/config/carrier_overrides.yaml" ./config/carrier_overrides.yaml; do
	if [ -f "$overrides" ]; then mask <"$overrides" >"$OUT/carrier_overrides.yaml"; break; fi
done

# Summary: coverage, counts and the last 400 timeline lines that usually matter.
count() { cat "$OUT"/logs/*.log | grep -cF -- "$1" || true; }
lines=$(cat "$OUT"/logs/*.log 2>/dev/null | wc -l)
if [ "$lines" -eq 0 ]; then
	echo "no app log lines since $SINCE in $LOGS; check --since and --logs" >&2
	exit 1
fi
{
	echo "== coverage"
	echo "log lines: $lines"
	echo "first: $(cat "$OUT"/logs/*.log | grep -m1 -oE '^\[[0-9-]+ [0-9:.]+\]')"
	echo "last:  $(cat "$OUT"/logs/*.log | grep -oE '^\[[0-9-]+ [0-9:.]+\]' | tail -1)"
	echo "builds started:"
	grep -h 'debug-field build' "$OUT"/logs/*.log | grep -oE '^\[[^]]+\]|"version": "[^"]*"' | paste - - || true
	grep -h 'debug-field build' "$OUT"/logs/*.log | grep -oE '"server_debug": [a-z]+' | sort | uniq -c || true
	grep -h 'carrier_overrides' "$OUT"/logs/*.log | grep -E '已加载|失败' | cut -c1-200 | tail -3 || true
	echo "pcap files: $(ls "$OUT"/pcap | wc -l) ($(du -sh "$OUT"/pcap | cut -f1))"
	[ -f "$OUT/pcap-skipped.txt" ] && echo "pcap skipped (rotated during copy): $(wc -l <"$OUT/pcap-skipped.txt")"
	echo "pcap health:"
	grep -h '\[dev\] IMS inner pcap' "$OUT"/logs/*.log | grep -vE 'pcap (opened|closed)' | cut -c1-200 | tail -20 || true
	grep -h '\[dev\] IMS inner pcap closed' "$OUT"/logs/*.log | grep -oE '"dropped": [1-9][0-9]*' | sort | uniq -c || true
	echo "transport_timeout since --since:"
	grep -h 'port-s lifecycle' "$OUT"/logs/*.log | grep transport_timeout | grep -oE '^\[[^]]+\]' || true
	echo "DPD results since --since:"
	grep -h '\[dev\] DPD probe \(answered\|failed\|interrupted\)' "$OUT"/logs/*.log | grep -oE '^\[[^]]+\]|DPD probe [a-z]+' | paste - - | tail -20 || true
	echo "CHILD_SA rekey failures since --since:"
	grep -h 'CHILD_SA rekey attempt failed\|"notify": "3[5-9]\|"notify": "1[47]' "$OUT"/logs/*.log | cut -c1-200 | tail -10 || true
	echo
	echo "== counts since $SINCE"
	for pattern in \
		'IMS REGISTER succeeded' 'IMS REGISTER failed' 'IMS SIP tx' 'IMS SIP rx' \
		'IMS conn opened' 'IMS conn read failed' 'IMS conn closed' \
		'port-s lifecycle' 'peer_reset' 'SUBSCRIBE(reg)' 'SUBSCRIBE(mwi)' \
		'peer deleted' 'IKE_SA' 'CHILD_SA' '[dev] IKE rx' '[dev] IKE tx' \
		'[dev] AT command' '[dev] QMI SetOperatingMode' \
		'[dev] CHILD_SA installed' '[dev] CHILD_SA peer delete' '[dev] CHILD_SA delete sending' \
		'[dev] IKE open failed' 'transport_timeout' 'context deadline exceeded' \
		'[dev] DPD probe due' '[dev] DPD probe sent' '[dev] DPD probe answered' '[dev] DPD probe failed' \
		'[dev] DPD probe interrupted' 'CHILD_SA rekey attempt failed' \
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
	echo "== timeline (connection failures, registrations, recoveries, DPD; last 400)"
	cat "$OUT"/logs/*.log | grep -E 'IMS conn (read failed|closed)|REGISTER (succeeded|failed)|port-s lifecycle|peer deleted|\[dev\] CHILD_SA|\[dev\] DPD probe|CHILD_SA rekey attempt failed|IKE open failed|"delete": |transport_timeout|Timeout storm|USB 控制面|清理 Worker|panic| ERROR ' | cut -c1-400 | tail -400
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
