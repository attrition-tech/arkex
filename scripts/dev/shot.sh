#!/bin/bash
# Screenshot a tmux pane running the TUI.
# usage: scripts/dev/shot.sh <out.png> [tmux-session]   (default session: arkextest)
# Needs: tmux, python3, agent-browser (preinstalled in Amp orbs).
set -e
out="$1"; session="${2:-arkextest}"
here="$(cd "$(dirname "$0")" && pwd)"
tmux capture-pane -t "$session" -e -p > /tmp/pane.ansi
python3 "$here/ansi2html.py" /tmp/pane.ansi /tmp/tui.html
agent-browser open "file:///tmp/tui.html" >/dev/null
agent-browser set viewport 1100 800 2 >/dev/null
agent-browser screenshot "$out" >/dev/null
echo "saved $out"
