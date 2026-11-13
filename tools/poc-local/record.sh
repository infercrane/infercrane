#!/bin/sh
# Opens the recording layout for the local PoC (macOS, iTerm2 + Chrome):
#
#   +------------------------+------------------------+--------------------+
#   | run log                | hardware tracker       |                    |
#   +------------------------+------------------------+  progress chart    |
#   | results (optimize      | bottleneck analysis    |  (autoresearch     |
#   |   watch, the real CLI) |                        |   style, Chrome)   |
#   +------------------------+------------------------+                    |
#   | measurement method     | engine internals       |                    |
#   +------------------------+------------------------+--------------------+
#
# Run `make poc-local-up` first. Start your screen recorder during the
# 10-second countdown in the run-log pane. Set RECORD_DASHBOARD=1 to also open
# the real dashboard and the web live view in Chrome (sign-in key: printed here).
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
web=${INFERCRANE_WEB_DIR:-$root/../web}
state=$root/.infercrane/poc
api_url=http://127.0.0.1:18080

test -f "$state/state.json" || { echo "run 'make poc-local-up' first" >&2; exit 1; }
curl -fsS -o /dev/null "$api_url/readyz" 2>/dev/null || {
  echo "the control plane is not reachable on $api_url; run 'make poc-local-up' again" >&2
  exit 1
}

if [ "${RECORD_DASHBOARD:-0}" = 1 ]; then
  console_url=http://127.0.0.1:3200
  if ! curl -fsS -o /dev/null "$console_url/sign-in/" 2>/dev/null; then
    test -d "$web/node_modules" || { echo "run 'npm install' in $web first" >&2; exit 1; }
    secret_file=$state/console-session-secret
    test -f "$secret_file" || { openssl rand -hex 32 >"$secret_file"; chmod 600 "$secret_file"; }
    (
      cd "$web/apps/console"
      INFERCRANE_CONSOLE_AUTH_MODE=local INFERCRANE_CONSOLE_ORIGIN=$console_url \
      INFERCRANE_CONTROL_API_URL=$api_url INFERCRANE_LOCAL_SESSION_SECRET=$(cat "$secret_file") \
      nohup ../../node_modules/.bin/next dev --hostname 127.0.0.1 --port 3200 >"$state/console.log" 2>&1 &
      echo $! >"$state/console.pid"
    )
  fi
  curl -fsS -o /dev/null http://127.0.0.1:3300/ 2>/dev/null || {
    nohup python3 "$root/tools/poc-local/live.py" >"$state/live.log" 2>&1 &
    echo $! >"$state/live.pid"
  }
  rm -f "$web/apps/console/CLAUDE.md"   # next dev may create it; the web checks reject it
  python3 -c "import json,sys; print('Dashboard sign-in key (local test key): ' + json.load(open(sys.argv[1]))['api_key'])" "$state/state.json"
  open -a "Google Chrome" "http://127.0.0.1:3300/" "$console_url/optimization"
fi

# The progress chart (autoresearch style) on the right; started here if needed.
curl -fsS -o /dev/null http://127.0.0.1:3300/progress 2>/dev/null || {
  nohup python3 "$root/tools/poc-local/live.py" >"$state/live.log" 2>&1 &
  echo $! >"$state/live.pid"
  sleep 2
}

osascript - "$root" <<'EOF'
on run argv
  set repo to quoted form of (item 1 of argv)
  set panes to "cd " & repo & " && clear && python3 tools/poc-local/panes.py "
  tell application "Finder" to set screenBounds to bounds of window of desktop
  set screenW to item 3 of screenBounds
  set screenH to item 4 of screenBounds
  set splitX to (screenW * 0.66) as integer
  tell application "iTerm"
    activate
    set w to (create window with default profile)
    set bounds of w to {0, 25, splitX, screenH}
    set runLog to current session of w
    tell runLog to set colTwo to (split vertically with default profile)
    tell runLog to set resultsPane to (split horizontally with default profile)
    tell resultsPane to set methodPane to (split horizontally with default profile)
    tell colTwo to set bottleneckPane to (split horizontally with default profile)
    tell bottleneckPane to set enginePane to (split horizontally with default profile)
    tell runLog to write text "cd " & repo & " && clear && echo 'InferCrane local PoC: starting in 10 s (start recording now)' && sleep 10 && make poc-local-run"
    tell resultsPane to write text panes & "results"
    tell methodPane to write text panes & "method"
    tell colTwo to write text panes & "hardware"
    tell bottleneckPane to write text panes & "bottleneck"
    tell enginePane to write text panes & "engine"
  end tell
  set midY to (screenH * 0.56) as integer
  tell application "Google Chrome"
    activate
    set jw to make new window
    set bounds of jw to {splitX, 25, screenW, midY}
    set URL of active tab of jw to "http://127.0.0.1:3300/journey"
    set pw to make new window
    set bounds of pw to {splitX, midY, screenW, screenH}
    set URL of active tab of pw to "http://127.0.0.1:3300/progress"
  end tell
end run
EOF
echo "Layout ready. Close the iTerm window to stop the panes; 'make poc-local-down' removes the cluster."
echo "Recommendation journey: http://127.0.0.1:3300/journey · progress chart: http://127.0.0.1:3300/progress"
