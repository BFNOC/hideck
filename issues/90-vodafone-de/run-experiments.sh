#!/bin/sh
# hideck#90: run each port-s experiment for a fixed time, one after another,
# and summarise how long port-s lived under each. Needs root and systemd.
#
#   DEVICE=wwan0 sh run-experiments.sh
#
# Settings (environment):
#   DEVICE       log tag of the Vodafone DE card, as in "[wwan0] IMS ..." (required)
#   MINUTES      minutes per experiment (default 25)
#   SERVICE      systemd unit (default hideck)
#   OUT          output directory (default ./issue90-out-<time>)
#   EXPERIMENTS  space-separated list (default: all, baseline first)
#   LOG_FILE     read this HiDeck log file instead of journalctl
set -eu

: "${DEVICE:?set DEVICE to the Vodafone DE log tag, e.g. DEVICE=wwan0}"
MINUTES=${MINUTES:-25}
SERVICE=${SERVICE:-hideck}
OUT=${OUT:-$(pwd)/issue90-out-$(date +%Y%m%d-%H%M%S)}
EXPERIMENTS=${EXPERIMENTS:-"baseline no-options no-alias via-port-c via-port-s no-outbound no-port-s-keepalive"}
DROPIN_DIR=/etc/systemd/system/$SERVICE.service.d
DROPIN=$DROPIN_DIR/issue90.conf

mkdir -p "$OUT" "$DROPIN_DIR"

cleanup() {
	rm -f "$DROPIN"
	systemctl daemon-reload
	systemctl restart "$SERVICE"
	echo "experiment settings removed; $SERVICE restarted with its normal environment"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

logs_since() { # $1 = epoch seconds, $2 = LOG_FILE line offset
	if [ -n "${LOG_FILE:-}" ]; then
		tail -n "+$2" "$LOG_FILE"
	else
		journalctl -u "$SERVICE" --since "@$1" --no-pager -o cat
	fi
}

echo "experiment opened closed peer_reset other_close still_open_at_end lifetimes" > "$OUT/summary.txt"
for experiment in $EXPERIMENTS; do
	printf '[Service]\nEnvironment=HIDECK_DEV90_EXPERIMENT=%s\nEnvironment=HIDECK_DEV90_PCAP_DIR=%s\n' \
		"$experiment" "$OUT/pcap-$experiment" > "$DROPIN"
	systemctl daemon-reload
	start=$(date +%s)
	offset=1
	[ -n "${LOG_FILE:-}" ] && offset=$(($(wc -l < "$LOG_FILE") + 1))
	systemctl restart "$SERVICE"
	echo "$(date '+%F %T') $experiment: running for $MINUTES minutes"
	sleep $((MINUTES * 60))

	log=$OUT/$experiment.log
	logs_since "$start" "$offset" | grep -E '\[dev\]|IMS (REGISTER|port-s|refresh|re-REGISTER|reset|registered|registration|protected|peer|P-CSCF|SMS readiness|immediate|downlink path|OPTIONS|SUBSCRIBE)|ims_reconnect' > "$log" || true
	lifecycle=$(grep -F "[$DEVICE] IMS port-s lifecycle" "$log" || true)
	opened=$(printf '%s\n' "$lifecycle" | grep -c '"event": "opened"' || true)
	closed=$(printf '%s\n' "$lifecycle" | grep -c '"event": "closed"' || true)
	reset=$(printf '%s\n' "$lifecycle" | grep '"event": "closed"' | grep -c '"close_kind": "peer_reset"' || true)
	last=$(printf '%s\n' "$lifecycle" | grep -oE '"event": "(opened|closed)"' | tail -1)
	still_open=no
	[ "$last" = '"event": "opened"' ] && still_open=yes
	lifetimes=$(printf '%s\n' "$lifecycle" | grep '"event": "closed"' |
		grep -oE '"lifetime": "[^"]*"' | sed -E 's/.*"([^"]*)"$/\1/' | tr '\n' ' ')
	active=$(grep -F "[$DEVICE]" "$log" | grep -c "\"experiment\": \"$experiment\"" || true)
	echo "$experiment $opened $closed $reset $((closed - reset)) $still_open ${lifetimes:-none}" >> "$OUT/summary.txt"
	echo "  opened=$opened closed=$closed peer_reset=$reset still_open=$still_open register_lines_with_experiment=$active"
	if [ "$active" -eq 0 ]; then
		echo "  WARNING: no [$DEVICE] REGISTER line reports experiment=$experiment; check DEVICE and the carrier preset"
	fi
done

echo
cat "$OUT/summary.txt"
echo "logs and pcaps: $OUT"
