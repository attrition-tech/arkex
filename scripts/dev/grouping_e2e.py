#!/usr/bin/env python3
"""Real CLI/PTY live-history and command-detail regression test (Unix).

Start scripts/fake_openai.py on 8765 first, then run:
  python3 scripts/dev/grouping_e2e.py /tmp/arkex /tmp/grouping-captures
Only a fake provider and disposable local files are used.
"""
import json
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
socket = "arkex-grouping-" + str(os.getpid())


def tmux(*args):
    return subprocess.check_output(["tmux", "-L", socket, *args], text=True)


def screen():
    return tmux("capture-pane", "-t", "test", "-p")


def wait(predicate, label):
    deadline = time.monotonic() + 35
    while time.monotonic() < deadline:
        text = screen()
        if predicate(text):
            return text
        time.sleep(.05)
    raise AssertionError(label + "\n" + screen())


def click(label):
    rows = screen().splitlines()
    y = next(i + 1 for i, line in enumerate(rows) if label in line)
    # Raw SGR mouse press/release through the terminal, not a model method.
    tmux("send-keys", "-t", "test", "-l", f"\x1b[<0;6;{y}M\x1b[<0;6;{y}m")


def capture(name):
    (captures / (name + ".ansi")).write_text(
        tmux("capture-pane", "-t", "test", "-e", "-N", "-p"), encoding="utf-8")


with tempfile.TemporaryDirectory(prefix="arkex-grouping-") as temp:
    home = Path(temp)
    config = json.loads((repo / "scripts/fake_config.json").read_text())
    config["permissions"] = {"bash": "allow", "read": "allow"}
    (home / "config.json").write_text(json.dumps(config))
    command = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(shlex.quote(x) for x in [
        "TERM=xterm-256color", "COLORTERM=truecolor", "HOME=" + temp, "ARKEX_HOME=" + temp,
        "SHELL=/bin/sh", binary, "--mode", "auto"])
    try:
        tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "42", "-c", temp, command)
        wait(lambda s: "command palette" in s, "startup")
        tmux("send-keys", "-t", "test", "-l", "grouping-test")
        tmux("send-keys", "-t", "test", "Enter")
        live = wait(lambda s: "Earlier work · 6 actions · 1 failed" in s and "sleep 20" in s, "live archive")
        # The 150 ms roll transition deliberately moves rows. Click only
        # after it settles; the running command leaves a 20-second window.
        time.sleep(.4)
        live = screen()
        assert live.count("Earlier work") == 1, live
        assert "Multiline command · 2 lines" in live and "RESULT_6" in live, live
        assert "intentionally-missing.txt" not in live, live
        capture("live-collapsed")

        click("Multiline command")
        wait(lambda s: "printf 'FIRST_LINE" in s and "printf 'SECOND_LINE" in s and "SECOND_LINE\n" in s,
             "multiline source and output")
        capture("command-expanded")
        click("Multiline command")
        click("Earlier work")
        wait(lambda s: "intentionally-missing.txt" in s, "archive expansion")
        click("intentionally-missing.txt")
        wait(lambda s: "no such file" in s.lower(), "failure details")
        capture("failure-expanded")
        click("Earlier work")
        wait(lambda s: "intentionally-missing.txt" not in s, "archive collapse")
        done = wait(lambda s: "GROUPING_COMPLETE" in s and "Earlier work" not in s and "1 failed attempt" in s,
                    "completed turn folded after final stream event")
        assert "Earlier work" not in done and "1 failed attempt" in done, done
        capture("completed")
        print("PASS: real CLI + HTTP tools + PTY mouse: one live archive, failure count, two recent calls, running call, multiline source/output, failure expansion, completion")
    finally:
        subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)
