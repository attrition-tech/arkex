#!/usr/bin/env python3
"""Real TUI/PTY confinement checks against scripts/fake_openai.py on port 8765.

Usage: python3 scripts/dev/workspace_e2e.py /tmp/arkex /tmp/workspace-captures
Disposable home/workspace, no real credentials or external model.
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
socket = "arkex-policy-e2e-" + str(os.getpid())


def tmux(*args):
    return subprocess.check_output(["tmux", "-L", socket, *args], text=True)


def screen():
    return tmux("capture-pane", "-t", "test", "-p")


def wait(predicate, label):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        text = screen()
        if predicate(text):
            return text
        time.sleep(.1)
    raise AssertionError(label + "\n" + screen())


def capture(name):
    (captures / (name + ".ansi")).write_text(tmux("capture-pane", "-t", "test", "-e", "-p"))


with tempfile.TemporaryDirectory(prefix="arkex-policy-") as temp:
    home = Path(temp)
    root = home / "workspace"
    root.mkdir()
    (home / "fixture.txt").write_text("OUTSIDE_READ_OK")
    (root / "escape").symlink_to(home, target_is_directory=True)
    cfg = json.loads((repo / "scripts/fake_config.json").read_text())

    def launch(permissions=None):
        cfg["permissions"] = permissions or {}
        (home / "config.json").write_text(json.dumps(cfg))
        cmd = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(
            shlex.quote(x) for x in ["TERM=xterm-256color", "COLORTERM=truecolor",
                                    "HOME=" + str(home), "ARKEX_HOME=" + str(home),
                                    "SHELL=/bin/sh", binary])
        tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "36", "-c", str(root), cmd)
        wait(lambda s: "Ask a question, describe a task" in s, "startup failed")
        # Let initial terminal capability replies settle before typing.
        time.sleep(.5)

    def request(name, args, expected):
        # Start a fresh transcript so stale results cannot satisfy the wait.
        tmux("send-keys", "-t", "test", "-l", "/new")
        tmux("send-keys", "-t", "test", "Enter")
        wait(lambda s: "RESULT:" not in s and "Ask a question, describe a task" in s, "new conversation")
        time.sleep(.2)
        prompt = "workspace-test " + json.dumps({"name": name, "args": args})
        tmux("send-keys", "-t", "test", "-l", prompt)
        tmux("send-keys", "-t", "test", "Enter")
        text = wait(lambda s: "RESULT:" in s and expected in s, "tool result missing: " + expected)
        assert "needs your permission" not in text
        time.sleep(.4)
        return text

    try:
        launch()
        request("write", {"path": "inside.txt", "content": "WORKSPACE_OK"}, "wrote")
        assert (root / "inside.txt").read_text() == "WORKSPACE_OK"
        capture("workspace-no-approval")
        request("read", {"path": str(home / "fixture.txt")}, "OUTSIDE_READ_OK")
        capture("outside-read")
        request("write", {"path": str(home / "forbidden.txt"), "content": "BAD"}, "write blocked")
        assert not (home / "forbidden.txt").exists()
        capture("outside-write-blocked")
        request("bash", {"command": "echo BAD > escape/forbidden.txt"}, "Read-only file system")
        assert not (home / "forbidden.txt").exists()
        capture("symlink-write-blocked")
        request("bash", {"command": 'printf SCRATCH_OK > "$TMPDIR/check"; cat "$TMPDIR/check"'}, "SCRATCH_OK")
        capture("scratch-no-approval")
        tmux("kill-session", "-t", "test")
        launch({"bash": "deny"})
        request("bash", {"command": "touch denied"}, "denied by config")
        assert not (root / "denied").exists()
        capture("explicit-deny")
        print("PASS: ordinary writes and scratch need no approval; outside reads work; outside/symlink writes and explicit denies block execution")
    finally:
        subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)

print("ALL WORKSPACE E2E CHECKS PASSED")
