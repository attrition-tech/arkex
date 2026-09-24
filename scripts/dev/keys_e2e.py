#!/usr/bin/env python3
"""Real PTY confirmation/history checks. Start fake_openai.py on 8765 first.
Usage: python3 scripts/dev/keys_e2e.py /tmp/arkex /tmp/key-captures
"""
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time

binary = str(Path(sys.argv[1]).resolve())
captures = Path(sys.argv[2]).resolve()
captures.mkdir(parents=True, exist_ok=True)
repo = Path(__file__).resolve().parents[2]
socket = "arkex-keys-" + str(os.getpid())


def tmux(*args):
    return subprocess.check_output(["tmux", "-L", socket, *args], text=True)


def screen():
    return tmux("capture-pane", "-t", "test", "-p")


def wait(predicate, label):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        text = screen()
        if predicate(text):
            return text
        time.sleep(.05)
    raise AssertionError(label + "\n" + screen())


def key(stroke):
    tmux("send-keys", "-t", "test", stroke)
    time.sleep(.2)  # Esc must not combine with the next key as an Alt prefix.


def text(value):
    tmux("send-keys", "-t", "test", "-l", value)


def capture(name):
    (captures / (name + ".ansi")).write_text(tmux("capture-pane", "-t", "test", "-e", "-N", "-p"))


def composer(value):
    wait(lambda s: value in "\n".join(s.splitlines()[-6:]), "composer missing " + value)


with tempfile.TemporaryDirectory(prefix="arkex-keys-") as temp:
    home = Path(temp)
    (home / "config.json").write_text((repo / "scripts/fake_config.json").read_text())
    cmd = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(shlex.quote(x) for x in [
        "TERM=xterm-256color", "COLORTERM=truecolor", "HOME=" + temp, "ARKEX_HOME=" + temp,
        "SHELL=/bin/sh", binary, "--mode", "auto"])
    try:
        tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "36", "-c", temp, cmd)
        tmux("set-option", "-t", "test", "remain-on-exit", "on")
        wait(lambda s: "command palette" in s, "startup")
        for prompt in ["ping first", "ping second"]:
            before = screen().count("pong")
            text(prompt)
            key("Enter")
            wait(lambda s: s.count("pong") > before and "command palette" in s, "response")
            time.sleep(.5)
        def prompt_rows():
            ansi = tmux("capture-pane", "-t", "test", "-e", "-N", "-p")
            return [next(line for line in ansi.splitlines() if prompt in line)
                    for prompt in ("ping first", "ping second")]
        baseline = prompt_rows()
        for stroke, selected in [("Tab", 1), ("Tab", 0), ("Tab", 0), ("BTab", 1), ("BTab", None)]:
            key(stroke)
            composer("Ask anything")
            rows = prompt_rows()
            assert [row != base for row, base in zip(rows, baseline)] == [selected == 0, selected == 1], "wrong highlighted prompt"
            if selected == 0:
                capture("prompt-highlight")
        key("Tab")
        text("unsent draft")
        key("BTab")
        composer("unsent draft")
        capture("history")
        print("PASS Tab/Shift+Tab: one transcript prompt highlighted, composer untouched, boundaries and typing", flush=True)

        key("C-c")
        wait(lambda s: "again within 2s to exit arkex" in s, "idle confirmation")
        composer("unsent draft")
        capture("exit-notice")
        time.sleep(2.1)
        assert "again within 2s" not in screen(), "notice did not expire"
        key("C-c")
        wait(lambda s: "again within 2s" in s, "expired confirmation must re-arm")
        key("Escape")
        assert "again within 2s" not in screen(), "different key did not disarm"
        print("PASS idle confirmation: first press preserves draft, expires, and different key disarms", flush=True)

        # Clear deliberately, not via Ctrl+C. Then exercise a real running tool.
        key("C-a")
        key("C-k")
        text("run slow")
        key("Enter")
        wait(lambda s: "sleep 30" in s, "slow tool not running")
        key("Escape")
        wait(lambda s: "again within 2s to stop this run" in s, "stop confirmation")
        assert "cancelled" not in screen()
        capture("stop-notice")
        key("Escape")
        wait(lambda s: "cancelled" in s and "command palette" in s, "second Esc did not cancel")
        print("PASS running tool: first Esc leaves it running; second cancels and leaves session open", flush=True)

        text("/quit")
        key("Enter")
        wait(lambda s: "Exit arkex?" in s and "Stay" in s, "quit dialog")
        capture("quit-dialog")
        key("Enter")
        wait(lambda s: "Exit arkex?" not in s and "command palette" in s, "default Stay")
        key("C-c")
        wait(lambda s: "again within 2s to exit arkex" in s, "exit confirmation")
        key("C-c")
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if tmux("display-message", "-p", "-t", "test", "#{pane_dead}").strip() == "1":
                break
            time.sleep(.1)
        else:
            raise AssertionError("double Ctrl+C did not exit")
        assert tmux("display-message", "-p", "-t", "test", "#{pane_dead_status}").strip() == "0"
        print("PASS /quit defaults to Stay; double Ctrl+C exits with status 0", flush=True)
    finally:
        subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)

print("ALL KEYBOARD E2E CHECKS PASSED")
