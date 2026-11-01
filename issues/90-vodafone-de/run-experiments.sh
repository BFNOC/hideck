#!/bin/sh
# hideck#90: run each port-s experiment for a fixed time, one after another,
# and summarise how long port-s lived under each. Needs root, systemd and
# journald (HiDeck logs to stdout as well as to its log file).
#
#   DEVICE=wwan0 sh run-experiments.sh
#
# Settings (environment):
#   DEVICE       log tag of the Vodafone DE card, as in "[wwan0] IMS ..." (required)
#   MINUTES      minutes per experiment (default 25)
#   SERVICE      systemd unit (default hideck)
#   OUT          output directory (default ./issue90-out-<time>)
#   EXPERIMENTS  space-separated list (default: baseline, every switch, baseline again)
#   OUTER_IFACE  interface for the outer IKE/ESP header capture (default any; "off" disables)
#
# Each run keeps the lines tagged with DEVICE plus the capture status lines,
# with every run of 11 or more digits masked (IMSI, ICCID, IMEI, MSISDN; the
# 10-digit SPIs stay).
set -eu

: "${DEVICE:?set DEVICE to the Vodafone DE log tag, e.g. DEVICE=wwan0}"
MINUTES=${MINUTES:-25}
SERVICE=${SERVICE:-hideck}
OUT=${OUT:-issue90-out-$(date +%Y%m%d-%H%M%S)}
EXPERIMENTS=${EXPERIMENTS:-"baseline no-options no-alias via-port-c via-port-s no-outbound no-port-s-keepalive baseline"}
OUTER_IFACE=${OUTER_IFACE:-any}
PREFLIGHT_SECONDS=120
DROPIN_DIR=/etc/systemd/system/$SERVICE.service.d
DROPIN=$DROPIN_DIR/issue90.conf

# Check everything before touching the service.
case $MINUTES in '' | *[!0-9]*) echo "MINUTES must be a whole number" >&2; exit 2 ;; esac
[ "$MINUTES" -ge 3 ] || { echo "MINUTES must be at least 3" >&2; exit 2; }
for tool in systemctl journalctl awk; do
	command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }
done
systemctl cat "$SERVICE" >/dev/null 2>&1 || { echo "systemd unit $SERVICE not found" >&2; exit 2; }
if [ -e "$DROPIN" ]; then
	echo "$DROPIN already exists (another run, or left over); remove it first" >&2
	exit 2
fi
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
case $OUT in *[[:space:]\"\'\\]*) echo "OUT must not contain spaces, quotes or backslashes: $OUT" >&2; exit 2 ;; esac
mkdir -p "$DROPIN_DIR"

outer_pid=
stop_outer() {
	if [ -n "$outer_pid" ]; then
		kill "$outer_pid" 2>/dev/null || true
		wait "$outer_pid" 2>/dev/null || true
		outer_pid=
	fi
}

cleanup() {
	stop_outer
	rm -f "$DROPIN" || echo "WARNING: could not remove $DROPIN" >&2
	systemctl daemon-reload || echo "WARNING: systemctl daemon-reload failed" >&2
	if systemctl restart "$SERVICE"; then
		echo "experiment settings removed; $SERVICE restarted with its normal environment"
	else
		echo "WARNING: $SERVICE did not restart; start it by hand" >&2
	fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# summarise LOG EXPERIMENT: one summary row from one HiDeck run's log.
summarise() {
	awk -v device="[$DEVICE]" -v experiment="$2" '
	function field(name,   pattern) {
		pattern = "\"" name "\": \"?[^\",}]*"
		if (!match($0, pattern)) return ""
		value = substr($0, RSTART, RLENGTH)
		sub("^\"" name "\": \"?", "", value)
		return value
	}
	index($0, device " IMS port-s lifecycle") {
		gen = field("generation"); event = field("event")
		if (event == "opened") { opened[gen] = 1; nopened++ }
		else if (event == "closed" && (gen in opened) && !(gen in closed)) {
			closed[gen] = 1; kind = field("close_kind"); life = field("lifetime")
			if (kind == "peer_reset") { resets++; reset_lives = reset_lives " " life }
			else if (kind == "local") { survived = survived " " life }
			else { other = other " " kind ":" life }
		}
	}
	index($0, device) && index($0, "\"experiment\": \"" experiment "\"") { active++ }
	index($0, "[dev] IMS inner pcap opened") { pcaps++ }
	/\[dev\] IMS inner pcap (unavailable|reached its size cap|write failed)/ { pcap_problem++ }
	/\[dev\] IMS inner pcap closed/ && /dropped(=|": )[1-9]/ { pcap_problem++ }
	index($0, device " [dev] IMS experiment disabled the port-s keepalive") && /err=<nil>|"err": null/ { ka_off++ }
	index($0, device " [dev] IMS experiment could not disable the port-s keepalive") { ka_fail++ }
	index($0, device " [dev] IMS experiment disabled the port-s keepalive") && !/err=<nil>|"err": null/ { ka_fail++ }
	END {
		unclosed = 0
		for (gen in opened) if (!(gen in closed)) unclosed++
		valid = (active > 0 && pcaps > 0 && pcap_problem == 0) ? "yes" : "no"
		if (experiment == "no-port-s-keepalive" && (ka_off == 0 || ka_fail > 0)) valid = "no"
		printf "%s valid=%s opened=%d peer_reset=%d survived_to_end=[%s ] other_close=[%s ] unclosed=%d reset_lifetimes=[%s ] register_lines=%d pcap_files=%d pcap_problems=%d keepalive_off=%d keepalive_fail=%d\n",
			experiment, valid, nopened, resets, survived, other, unclosed, reset_lives, active, pcaps, pcap_problem, ka_off, ka_fail
	}' "$1"
}

: > "$OUT/summary.txt"
index=0
for experiment in $EXPERIMENTS; do
	index=$((index + 1))
	name=$(printf '%02d-%s' "$index" "$experiment")
	printf '[Service]\nEnvironment="HIDECK_DEV90_EXPERIMENT=%s"\nEnvironment="HIDECK_DEV90_PCAP_DIR=%s"\n' \
		"$experiment" "$OUT/pcap-$name" > "$DROPIN"
	systemctl daemon-reload
	if [ "$OUTER_IFACE" != off ] && command -v tcpdump >/dev/null; then
		# Headers only: outer IP, UDP and the ESP SPI/sequence or IKE header.
		tcpdump -i "$OUTER_IFACE" -s 96 -w "$OUT/outer-$name.pcap" 'udp port 500 or udp port 4500 or esp' \
			>/dev/null 2>&1 &
		outer_pid=$!
	fi
	systemctl restart "$SERVICE"
	invocation=$(systemctl show -p InvocationID --value "$SERVICE")
	started=$(date '+%F %T')
	echo "$started $name: running for $MINUTES minutes (invocation $invocation)"
	# Stop early rather than waste a run when the switch or the capture is not live.
	sleep "$PREFLIGHT_SECONDS"
	early=$(journalctl "_SYSTEMD_INVOCATION_ID=$invocation" --no-pager -o cat 2>/dev/null || true)
	if ! printf '%s\n' "$early" | grep -F "[$DEVICE]" | grep -qF "\"experiment\": \"$experiment\""; then
		echo "ERROR: after ${PREFLIGHT_SECONDS}s no [$DEVICE] REGISTER line reports experiment=$experiment;" \
			"check DEVICE, the carrier preset and that $SERVICE logs to the journal" >&2
		exit 1
	fi
	if ! printf '%s\n' "$early" | grep -qF "[dev] IMS inner pcap opened"; then
		echo "ERROR: after ${PREFLIGHT_SECONDS}s no inner pcap was opened; see [dev] IMS inner pcap lines" >&2
		exit 1
	fi
	remaining=$((MINUTES * 60 - PREFLIGHT_SECONDS))
	if [ "$remaining" -gt 0 ]; then sleep "$remaining"; fi
	# Stop first so the closing port-s of this run is logged as part of it.
	systemctl stop "$SERVICE"
	stop_outer
	ended=$(date '+%F %T')

	log=$OUT/$name.log
	if ! journalctl "_SYSTEMD_INVOCATION_ID=$invocation" --no-pager -o cat > "$log.tmp"; then
		echo "$name valid=no reason=journalctl-failed" >> "$OUT/summary.txt"
		echo "  WARNING: could not read the journal for this run"
		rm -f "$log.tmp"
		continue
	fi
	grep -F -e "[$DEVICE]" -e "[dev] IMS inner pcap" "$log.tmp" | sed -E 's/[0-9]{11,}/<masked>/g' > "$log" || true
	rm -f "$log.tmp"
	summarise "$log" "$experiment" | sed "s/^$experiment /$name start=\"$started\" end=\"$ended\" /" >> "$OUT/summary.txt"
	tail -n 1 "$OUT/summary.txt" | sed 's/^/  /'
done

echo
cat "$OUT/summary.txt"
echo "logs and pcaps: $OUT"
