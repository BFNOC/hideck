#!/bin/sh
# hideck#90: run each port-s experiment for a fixed time, one after another,
# and summarise how long port-s lived under each. Needs root, systemd and
# journald (HiDeck logs to stdout as well as to its log file).
#
#   DEVICE=wwan0 sh run-experiments.sh
#
# Settings (environment):
#   DEVICE       log tag of the Vodafone DE card, as in "[wwan0] IMS ..." (required)
#   MINUTES      minutes per experiment (default 10)
#   SERVICE      systemd unit (default hideck)
#   OUT          output directory (default ./issue90-out-<time>)
#   EXPERIMENTS  space-separated list of runs; a run is one switch or a comma-separated
#                set (default: baseline, notify-dialog and no-mwi alternating with
#                baseline, then both together)
#   OUTER_IFACE  interface for the outer IKE/ESP header capture (default any; "off" disables)
#
# Each run keeps the lines tagged with DEVICE plus the capture status lines,
# with every run of 11 or more digits masked (IMSI, ICCID, IMEI, MSISDN; the
# 10-digit SPIs stay).
set -eu

: "${DEVICE:?set DEVICE to the Vodafone DE log tag, e.g. DEVICE=wwan0}"
MINUTES=${MINUTES:-10}
SERVICE=${SERVICE:-hideck}
OUT=${OUT:-issue90-out-$(date +%Y%m%d-%H%M%S)}
EXPERIMENTS=${EXPERIMENTS:-"baseline notify-dialog no-mwi baseline notify-dialog no-mwi,notify-dialog"}
KNOWN="baseline no-options no-alias via-port-c via-port-s no-outbound no-port-s-keepalive no-register-route no-feature-caps recovery-jitter notify-dialog no-mwi"
OUTER_IFACE=${OUTER_IFACE:-any}
PREFLIGHT_SECONDS=120
DROPIN_DIR=/etc/systemd/system/$SERVICE.service.d
DROPIN=$DROPIN_DIR/issue90.conf

# Check everything before touching the service.
case $MINUTES in '' | *[!0-9]*) echo "MINUTES must be a whole number" >&2; exit 2 ;; esac
[ "$MINUTES" -ge 3 ] || { echo "MINUTES must be at least 3" >&2; exit 2; }
for tool in systemctl journalctl awk sort paste; do
	command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }
done
systemctl cat "$SERVICE" >/dev/null 2>&1 || { echo "systemd unit $SERVICE not found" >&2; exit 2; }
if [ -e "$DROPIN" ]; then
	echo "$DROPIN already exists (another run, or left over); remove it first" >&2
	exit 2
fi
# HiDeck logs a set sorted and without duplicates; use the same spelling.
normalise() {
	sorted=$(printf '%s\n' "$1" | tr ',' '\n' | sed '/^$/d; /^baseline$/d' | LC_ALL=C sort -u | paste -sd, -)
	echo "${sorted:-baseline}"
}
for run in $EXPERIMENTS; do
	for switch in $(printf '%s' "$run" | tr ',' ' '); do
		case " $KNOWN " in *" $switch "*) ;; *) echo "unknown experiment: $switch (known: $KNOWN)" >&2; exit 2 ;; esac
	done
done
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
# port-s generations restart at 1 on each new tunnel, so a tunnel counter is
# part of the key. cycle_s lists the gaps between successive peer resets: on
# 11-02 they were whole seconds plus about 0.07 s per P-CSCF (31/32 s on the
# vodafone SBCs, 55-60 s on the nokia ones) whatever our REGISTER timing was.
summarise() {
	awk -v device="[$DEVICE]" -v experiment="$2" '
	function field(name,   pattern, value) {
		pattern = "\"" name "\": \"?[^\",}]*"
		if (!match($0, pattern)) return ""
		value = substr($0, RSTART, RLENGTH)
		sub("^\"" name "\": \"?", "", value)
		return value
	}
	function secs(hms,   parts) {
		split(hms, parts, ":")
		return parts[1] * 3600 + parts[2] * 60 + parts[3]
	}
	function logsecs(   t) {
		t = secs(substr($0, 13, 8))
		if (last_t != "" && t < last_t) day += 86400
		last_t = t
		return t + day
	}
	function inc(arr, key) { if (key == "") key = "?"; arr[key]++ }
	function list(arr,   key, out) { out = ""; for (key in arr) out = out " " key ":" arr[key]; return out }
	/^\[[0-9-]+ [0-9:]+\]/ { now = logsecs(); if (gap_start == "") gap_start = now }
	# A new tunnel drops the old flows; a reset cycle never spans tunnels.
	index($0, device " IMS P-CSCF configuration from new tunnel") {
		tunnel++; last_reset = ""
		if (open_count > 0) { open_count = 0; gap_start = now }
	}
	index($0, device " IMS port-s lifecycle") {
		gen = tunnel ":" field("generation"); event = field("event")
		# max_gap: the longest time with no port-s open at all.
		if (event == "opened") {
			opened[gen] = 1; nopened++
			if (open_count == 0 && now - gap_start > max_gap) max_gap = now - gap_start
			open_count++
		} else if (event == "closed" && (gen in opened) && !(gen in closed)) {
			closed[gen] = 1; kind = field("close_kind"); life = field("lifetime")
			if (--open_count <= 0) { open_count = 0; gap_start = now }
			if (kind == "peer_reset") { resets++; reset_lives = reset_lives " " life }
			else if (kind == "local") { survived = survived " " life }
			else { other = other " " kind ":" life }
		}
	}
	index($0, device " [dev] IMS port-s timing") && index($0, "\"event\": \"closed:peer_reset\"") {
		at = secs(field("at"))
		if (last_reset != "") {
			gap = at - last_reset
			if (gap < 0) gap += 86400
			if (gap >= 1) cycles = cycles " " sprintf("%.2f", gap)
		}
		last_reset = at
	}
	index($0, device " IMS REGISTER succeeded") { inc(pcscf, field("pcscf")) }
	index($0, device " [dev] IMS inbound request") && index($0, "\"method\": \"NOTIFY\"") {
		if (match($0, /Contact: <sip:[^:;>@]+/)) inc(sbc, substr($0, RSTART + 14, RLENGTH - 14))
	}
	# The recovery REGISTER after a peer reset: its final status, "challenged"
	# when a 401/407 was not followed by a final response, "none" when nothing
	# came back before the next REGISTER flow.
	function refresh_done(status) {
		inc(refresh, status)
		if (pending_delay != "") jitter_refresh = jitter_refresh " " pending_delay ":" status
		refresh_pending = 0; challenged = 0; pending_delay = ""
	}
	index($0, device " [dev] IMS experiment delays the port-s recovery REGISTER") { jitters++; next_delay = field("delay") }
	index($0, device " [dev] IMS REGISTER attempt context") {
		if (refresh_pending) refresh_done(challenged ? "challenged" : "none")
		refresh_pending = (field("kind") == "refresh" && field("trigger") == "port-s flow failed" &&
			field("port_s_close_kind") == "peer_reset")
		pending_delay = refresh_pending ? next_delay : ""
		next_delay = ""
	}
	index($0, device " IMS REGISTER response") && refresh_pending {
		status = field("status")
		if (status == "401" || status == "407") challenged = 1
		else refresh_done(status)
	}
	# One [dev] exchange line per SUBSCRIBE(reg); the WARN line is rate-limited.
	index($0, device " [dev] IMS SUBSCRIBE exchange") && index($0, "\"event\": \"reg\"") &&
		index($0, "\"status\": 480") && /User not registered/ { not_registered++ }
	index($0, device " [dev] IMS SUBSCRIBE exchange") && index($0, "\"event\": \"mwi\"") { mwi_sent++ }
	# notify-dialog: the response log says whether each NOTIFY 2xx carried
	# Contact and exactly the Record-Route lines of the NOTIFY.
	index($0, device " [dev] IMS inbound response") && index($0, "\"method\": \"NOTIFY\"") && index($0, "SIP/2.0 2") {
		if (!/err=<nil>|"err": null/) notify_write_failed++
		else if (index($0, "\"contact\": true") && index($0, "\"record_route_copied\": true")) notify_dialog++
		else notify_plain++
	}
	index($0, device " IMS runtime reconnect requested") { reconnects++ }
	index($0, device) && index($0, "\"experiment\": \"" experiment "\"") { active++ }
	index($0, "[dev] IMS inner pcap opened") { pcaps++ }
	/\[dev\] IMS inner pcap (unavailable|reached its size cap|write failed)/ { pcap_problem++ }
	/\[dev\] IMS inner pcap closed/ && /dropped(=|": )[1-9]/ { pcap_problem++ }
	index($0, device " [dev] IMS experiment disabled the port-s keepalive") && /err=<nil>|"err": null/ { ka_off++ }
	index($0, device " [dev] IMS experiment could not disable the port-s keepalive") { ka_fail++ }
	index($0, device " [dev] IMS experiment disabled the port-s keepalive") && !/err=<nil>|"err": null/ { ka_fail++ }
	END {
		if (refresh_pending) refresh_done(challenged ? "challenged" : "none")
		if (open_count == 0 && now - gap_start > max_gap) max_gap = now - gap_start
		unclosed = 0
		for (gen in opened) if (!(gen in closed)) unclosed++
		valid = (active > 0 && pcaps > 0 && pcap_problem == 0) ? "yes" : "no"
		if (index(experiment, "no-port-s-keepalive") && (ka_off == 0 || ka_fail > 0)) valid = "no"
		if (index(experiment, "recovery-jitter") && resets > 0 && jitters == 0) valid = "no"
		if (index(experiment, "notify-dialog") && nopened > 0 && (notify_dialog == 0 || notify_plain > 0)) valid = "no"
		if (index(experiment, "no-mwi") && mwi_sent > 0) valid = "no"
		# A run with no port-s for minutes says nothing about how long port-s lives.
		warn = ""
		if (nopened == 0 || max_gap > 300) warn = warn " no-port-s-" int(max_gap) "s"
		if (not_registered > 0) warn = warn " user-not-registered-" not_registered
		if (notify_write_failed > 0) warn = warn " notify-200-write-failed-" notify_write_failed
		printf "%s valid=%s warn=[%s ] opened=%d peer_reset=%d survived_to_end=[%s ] other_close=[%s ] unclosed=%d reset_lifetimes=[%s ] cycle_s=[%s ] refresh_after_reset=[%s ] jitter_refresh=[%s ] pcscf=[%s ] sbc=[%s ] user_not_registered=%d runtime_reconnects=%d max_port_s_gap_s=%d register_lines=%d pcap_files=%d pcap_problems=%d keepalive_off=%d keepalive_fail=%d jitter_delays=%d\n",
			experiment, valid, warn, nopened, resets, survived, other, unclosed, reset_lives, cycles, list(refresh), jitter_refresh, list(pcscf), list(sbc),
			not_registered, reconnects, max_gap, active, pcaps, pcap_problem, ka_off, ka_fail, jitters
	}' "$1"
}

: > "$OUT/summary.txt"
index=0
for experiment in $EXPERIMENTS; do
	index=$((index + 1))
	experiment=$(normalise "$experiment")
	name=$(printf '%02d-%s' "$index" "$(printf '%s' "$experiment" | tr ',' '+')")
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
