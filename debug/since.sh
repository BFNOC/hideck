#!/bin/sh
# Print HiDeck log lines from a given time on, across all daily files.
#   sh debug/since.sh "2026-11-05 10:00:00" [logs-dir]
# Lines without a timestamp (stack traces, the startup banner) follow the line
# before them.
set -eu
FROM=${1:?usage: sh debug/since.sh "YYYY-MM-DD HH:MM:SS" [logs-dir]}
LOGS=${2:-}
if [ -z "$LOGS" ]; then
	dir=$(systemctl show -p WorkingDirectory --value hideck 2>/dev/null || true)
	for candidate in "${dir:+$dir/logs}" ./logs /app/logs; do
		if [ -n "$candidate" ] && [ -d "$candidate" ]; then LOGS=$candidate; break; fi
	done
fi
[ -n "$LOGS" ] && [ -d "$LOGS" ] || { echo "HiDeck logs directory not found; pass it as the second argument" >&2; exit 2; }
for file in "$LOGS"/app-*.log; do
	awk -v a="$FROM" '/^\[[0-9][0-9][0-9][0-9]-/ { keep = substr($0, 2, 19) >= a } keep' "$file"
done
