#!/bin/bash
# Start the TUI in a detached tmux session against the fake OpenAI server for manual/visual testing.
# usage: scripts/dev/tui.sh [binary] [home]     (defaults: /tmp/arkex, /tmp/arkexhome)
# Prereqs: fake server on 127.0.0.1:8765 (python3 scripts/fake_openai.py), a config in $home
# with a "fake" connection (see scripts/fake_config.json), and `go build -o /tmp/arkex ./cmd/arkex`.
# Type:  tmux send-keys -t arkextest -l 'text'; sleep 0.5; tmux send-keys -t arkextest Enter
# Read:  tmux capture-pane -t arkextest -p
set -e
bin="${1:-/tmp/arkex}"; home="${2:-/tmp/arkexhome}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
tmux kill-session -t arkextest 2>/dev/null || true
tmux new-session -d -s arkextest -x 120 -y 34 -c "$repo" \
  "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE TERM=xterm-256color COLORTERM=truecolor HOME=$home ARKEX_HOME=$home FAKE_KEY=sk-test BROWSER=true $bin --mode auto -m fake/deepseek-v4-flash 2>/tmp/arkex.err; sleep 300"
echo "session arkextest started"
