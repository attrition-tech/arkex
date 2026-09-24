#!/usr/bin/env python3
"""Real CLI/PTY network recovery tests; run fake_openai.py on port 8765 first.
Usage: python3 scripts/dev/network_e2e.py /tmp/arkex /tmp/network-captures
Simulates lost sockets, not OS suspend. Uses only disposable local data.
"""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time
from urllib.request import urlopen

binary = str(Path(sys.argv[1]).resolve())
captures = Path(sys.argv[2]).resolve()
captures.mkdir(parents=True, exist_ok=True)
repo = Path(__file__).resolve().parents[2]
socket = "arkex-network-" + str(os.getpid())


def tmux(*args):
    return subprocess.check_output(["tmux", "-L", socket, *args], text=True)


def screen():
    return tmux("capture-pane", "-t", "test", "-p")


def wait(predicate, label):
    deadline = time.monotonic() + 25
    while time.monotonic() < deadline:
        text = screen()
        if predicate(text):
            return text
        time.sleep(.1)
    raise AssertionError(label + "\n" + screen())


def state(key):
    with urlopen("http://127.0.0.1:8765/network-test-state", timeout=2) as r:
        return json.load(r).get(key, {"requests": 0})


def capture(name):
    (captures / (name + ".ansi")).write_text(tmux("capture-pane", "-t", "test", "-e", "-N", "-p"))


for mode in ["recover", "partial", "exhaust", "cancel", "partialcancel", "waitcancel", "malformed", "incomplete", "malformedexhaust"]:
    with tempfile.TemporaryDirectory(prefix="arkex-network-") as temp:
        home = Path(temp)
        (home / "config.json").write_text((repo / "scripts/fake_config.json").read_text())
        key = mode + "-" + str(os.getpid())
        cmd = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(shlex.quote(x) for x in [
            "TERM=xterm-256color", "COLORTERM=truecolor", "HOME=" + temp, "ARKEX_HOME=" + temp,
            "SHELL=/bin/sh", binary, "--mode", "auto"])
        try:
            tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "36", "-c", temp, cmd)
            wait(lambda s: "command palette" in s, "startup failed")
            tmux("send-keys", "-t", "test", "-l", "network-test " + mode + " " + key)
            tmux("send-keys", "-t", "test", "Enter")
            if mode == "malformedexhaust":
                wait(lambda s: "2 retries exhausted" in s and "Try again" in s, "missing invalid-response exhaustion dialog")
                assert state(key)["requests"] == 4
                capture(mode)
                tmux("send-keys", "-t", "test", "Enter")
            elif mode == "waitcancel":
                wait(lambda s: "retry" in s.lower() and state(key)["requests"] == 2, "backoff not displayed")
                tmux("send-keys", "-t", "test", "Escape")
                wait(lambda s: "Press Esc again" in s, "missing stop confirmation")
                time.sleep(.2)
                tmux("send-keys", "-t", "test", "Escape")
            elif mode in ("partial", "partialcancel"):
                wait(lambda s: "Try again" in s and "Response interrupted" in s, "partial response did not ask")
                assert state(key)["requests"] == 2
                capture(mode)
                time.sleep(.5)
                assert state(key)["requests"] == 2, "partial response restarted without consent"
                tmux("send-keys", "-t", "test", "Enter" if mode == "partial" else "Escape")
                if mode == "partialcancel":
                    wait(lambda s: "Press Esc again" in s, "missing retry cancel confirmation")
                    time.sleep(.2)
                    tmux("send-keys", "-t", "test", "Escape")
            elif mode in ("exhaust", "cancel"):
                wait(lambda s: "3 retries exhausted" in s and "Try again" in s, "missing exhaustion dialog")
                assert state(key)["requests"] == 5
                capture(mode)
                if mode == "exhaust":
                    tmux("send-keys", "-t", "test", "Enter")
                    wait(lambda s: "3 retries exhausted" in s and state(key)["requests"] == 9, "second retry cycle failed")
                    tmux("send-keys", "-t", "test", "Enter")
                else:
                    # Click Cancel through the actual mouse input path.
                    lines = screen().splitlines()
                    row = next(i for i, line in enumerate(lines) if "Try again" in line and "Cancel" in line)
                    col = lines[row].index("Cancel") + 1
                    tmux("send-keys", "-t", "test", "-l", f"\x1b[<0;{col+1};{row+1}M\x1b[<0;{col+1};{row+1}m")
            canceled = mode in ("cancel", "partialcancel", "waitcancel")
            want = "cancelled" if canceled else "NETWORK_RECOVERED"
            wait(lambda s: want in s and "command palette" in s, "run did not settle")
            expected = {"recover": 3, "partial": 3, "exhaust": 10, "cancel": 5, "partialcancel": 2, "waitcancel": 2,
                        "malformed": 4, "incomplete": 4, "malformedexhaust": 5}[mode]
            time.sleep(1.3) # a canceled 1-second backoff must not send another request
            observed = state(key)
            assert observed["requests"] == expected, observed
            assert observed["has_tool_result"], "completed tool result missing from retry"
            assert not observed["has_partial_history"], "failed response leaked into retry history"
            assert (home / "executions").read_text() == "X", "completed command ran more than once"
            if not canceled:
                assert "UNFINISHED_NETWORK" not in screen(), "interrupted response remains visible"
                assert "Interrupted response discarded" not in screen(), "successful retry polluted transcript"
            capture(mode + "-done")
            print(f"PASS {mode}: requests={expected}; command executions=1; completed result retained; partial history absent", flush=True)
        finally:
            subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)

print("ALL NETWORK E2E CHECKS PASSED")
