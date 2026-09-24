#!/usr/bin/env python3
"""Real TUI/PTY workspace approval regression test.

Start scripts/fake_openai.py on 8765 first. Usage:
  python3 scripts/dev/workspace_e2e.py /tmp/arkex /tmp/workspace-captures
Uses disposable data and an isolated tmux server; no real model or credentials.
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
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        text = screen()
        if predicate(text):
            return text
        time.sleep(.1)
    raise AssertionError(label + "\n" + screen())


def request(name, args):
    prompt = "workspace-test " + json.dumps({"name": name, "args": args})
    tmux("send-keys", "-t", "test", "-l", prompt)
    tmux("send-keys", "-t", "test", "Enter")


def approval():
    return wait(lambda s: "needs your permission" in s, "approval missing")


def settled():
    # Approval dismissal can briefly expose the idle footer before queued
    # agent events arrive. Require a stable idle screen, not that one frame.
    since = None
    def idle(text):
        nonlocal since
        if "needs your permission" in text or "command palette" not in text:
            since = None
            return False
        if since is None:
            since = time.monotonic()
        return time.monotonic() - since >= .5
    wait(idle, "run did not finish")


def capture(name):
    (captures / (name + ".ansi")).write_text(tmux("capture-pane", "-t", "test", "-e", "-p"))


with tempfile.TemporaryDirectory(prefix="arkex-policy-") as temp:
    home = Path(temp)
    root = home / "nsutm"
    for d in [root / "falak", root / "frontend/.tooling", home / "shared", home / "other"]:
        d.mkdir(parents=True)
    (root / "frontend/.tooling/activate").write_text("printf 'SIBLING_OK\\n'\n")
    (home / "shared/read.txt").write_text("READ_FIXTURE\n")
    cfg = json.loads((repo / "scripts/fake_config.json").read_text())

    def launch(mode="auto", permissions=None):
        cfg["permissions"] = permissions or {}
        (home / "config.json").write_text(json.dumps(cfg))
        cmd = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(
            shlex.quote(x) for x in ["TERM=xterm-256color", "COLORTERM=truecolor",
                                    "HOME=" + str(home), "ARKEX_HOME=" + str(home),
                                    "SHELL=/bin/sh", binary, "--mode", mode])
        tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "36", "-c", str(root), cmd)
        wait(lambda s: "command palette" in s, "startup failed")

    def stop():
        tmux("kill-session", "-t", "test")

    try:
        launch()
        request("bash", {"workdir": "falak", "command": "cat <<'EOF'\nHEREDOC_OK\nEOF\n. ../frontend/.tooling/activate\npwd"})
        wait(lambda s: "SIBLING_OK" in s and "RESULT:" in s, "sibling command failed")
        settled()
        assert "needs your permission" not in screen()
        capture("sibling-no-approval")
        print("PASS: sibling heredoc executes in falak without approval", flush=True)

        request("write", {"path": str(home / "shared/once.txt"), "content": "ONCE"})
        approval()
        tmux("send-keys", "-t", "test", "Enter")
        settled()
        assert (home / "shared/once.txt").read_text() == "ONCE"
        request("write", {"path": str(home / "shared/denied.txt"), "content": "MUST_NOT_EXIST"})
        approval()
        tmux("send-keys", "-t", "test", "n")
        settled()
        assert not (home / "shared/denied.txt").exists()
        print("PASS: Allow once does not persist; Deny prevents filesystem write", flush=True)

        request("read", {"path": str(home / "shared/read.txt")})
        assert "read-only" in approval()
        capture("read-only-trust")
        tmux("send-keys", "-t", "test", "a")
        settled()
        request("write", {"path": str(home / "shared/read-trust-is-not-write.txt"), "content": "DENIED"})
        approval()
        tmux("send-keys", "-t", "test", "n")
        settled()
        assert not (home / "shared/read-trust-is-not-write.txt").exists()
        print("PASS: read-only trust cannot authorize a write", flush=True)

        command = "\n".join(["# inspectable line " + str(i) for i in range(16)]) + "\nprintf TRUSTED > first.txt"
        request("bash", {"workdir": str(home / "shared"), "command": command})
        text = approval()
        assert "Trust directory" in text and "not a sandbox" in text
        capture("trust-command-top")
        tmux("send-keys", "-t", "test", "PageDown", "PageDown", "PageDown", "PageDown")
        wait(lambda s: "15–22 of 22" in s and any("│" in line and "printf TRUSTED" in line for line in s.splitlines()),
             "full command tail not scrollable in approval panel")
        capture("trust-command-tail")
        tmux("resize-window", "-t", "test", "-x", "24", "-y", "8")
        wait(lambda s: "Deny" in s, "tiny approval actions missing")
        capture("trust-small")
        tmux("resize-window", "-t", "test", "-x", "120", "-y", "36")
        wait(lambda s: "Trust directory" in s, "resize failed")
        # Exercise actual SGR mouse selection of the trust button.
        lines = screen().splitlines()
        row = next(i for i, line in enumerate(lines) if "Allow once" in line and "Deny" in line)
        col = lines[row].index("Trust directory") + 2
        tmux("send-keys", "-t", "test", "-l", f"\x1b[<0;{col+1};{row+1}M\x1b[<0;{col+1};{row+1}m")
        settled()
        assert (home / "shared/first.txt").read_text() == "TRUSTED"
        request("write", {"path": str(home / "shared/sub/second.txt"), "content": "SUBTREE"})
        settled()
        assert (home / "shared/sub/second.txt").read_text() == "SUBTREE"
        print("PASS: full command scroll, 24x8 resize, mouse trust, subtree write without reapproval", flush=True)

        request("bash", {"command": "printf X > " + str(home / "shared/allowed.txt") + "; printf BAD > " + str(home / "other/denied.txt")})
        text = approval()
        assert "other" in text
        tmux("send-keys", "-t", "test", "n")
        settled()
        assert not (home / "other/denied.txt").exists()
        assert not (home / "shared/allowed.txt").exists()
        print("PASS: trusted first path does not hide second outside path; whole command blocked", flush=True)

        stop()
        launch()
        request("write", {"path": str(home / "shared/restart.txt"), "content": "DENIED"})
        approval()
        tmux("send-keys", "-t", "test", "n")
        settled()
        assert not (home / "shared/restart.txt").exists()
        print("PASS: directory trust expires on process exit", flush=True)
        stop()

        for mode in ["build", "plan", "auto"]:
            launch(mode, {"bash": "deny", "read": "deny", "write": "deny"})
            request("bash", {"command": "touch denied-" + mode})
            wait(lambda s: "denied by config" in s, "explicit deny missing in " + mode)
            settled()
            assert not (root / ("denied-" + mode)).exists()
            stop()
        print("PASS: explicit config deny blocks execution in Build, Plan and Auto", flush=True)
    finally:
        subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)

print("ALL WORKSPACE E2E CHECKS PASSED")
