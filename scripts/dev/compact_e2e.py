#!/usr/bin/env python3
"""Compaction through the real TUI/HTTP adapter, using disposable local sessions.
Usage: python3 scripts/dev/compact_e2e.py /tmp/arkex /tmp/compact-captures
No credentials or external model calls. Captures retain terminal ANSI for shot tooling.
"""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

binary = str(Path(sys.argv[1]).resolve())
captures = Path(sys.argv[2]).resolve()
captures.mkdir(parents=True, exist_ok=True)
socket = "arkex-compact-" + str(os.getpid())
release = threading.Event()
requests = []
mode = "success"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        requests.append(body)
        summary = "Write an updated hand-off summary" in str(body.get("messages", []))
        if summary:
            assert body["stream"] and not body.get("tools")
            release.wait(20)
            text, finish, tokens = "The user requested local work. No publication is authorized.", "stop", 500
            if mode == "length":
                text, finish = "This summary was cut short", "length"
        else:
            compacted = "Original conversation history:" in str(body.get("messages", []))
            text = "CONTINUED_AFTER_COMPACTION" if compacted else ("Recorded evidence. " * 400 + "\nOriginal decision AZ-719 is recorded; tests remain pending.")
            finish, tokens = "stop", 400 if compacted else 14000
        try:
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.end_headers()
            for delta, reason in [({"role": "assistant", "content": text}, None), ({}, finish)]:
                chunk = {"id": "c", "object": "chat.completion.chunk", "model": "m", "created": 1,
                         "choices": [{"index": 0, "delta": delta, "finish_reason": reason}]}
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.write(("data: " + json.dumps({"id": "c", "object": "chat.completion.chunk", "model": "m", "created": 1,
                             "choices": [], "usage": {"prompt_tokens": tokens, "completion_tokens": 12, "total_tokens": tokens + 12}}) + "\n\ndata: [DONE]\n\n").encode())
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


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


def send(text):
    tmux("send-keys", "-t", "test", "-l", text)
    tmux("send-keys", "-t", "test", "Enter")


def capture(name):
    (captures / (name + ".ansi")).write_text(tmux("capture-pane", "-t", "test", "-e", "-N", "-p"))


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    for mode in ("success", "length", "cancel"):
        with tempfile.TemporaryDirectory(prefix="arkex-compact-") as temp:
            home = Path(temp)
            requests.clear()
            release.clear()
            config = {"version": 2, "connections": {"fake": {"kind": "llm-server", "api": "openai-compat",
                      "baseUrl": f"http://127.0.0.1:{server.server_port}/v1", "apiKey": "test",
                      "models": [{"id": "m", "contextWindow": 16000}]}}, "default": "fake/m"}
            (home / "config.json").write_text(json.dumps(config))
            cmd = "env -u NO_COLOR -u CLICOLOR -u CLICOLOR_FORCE " + " ".join(shlex.quote(x) for x in [
                "TERM=xterm-256color", "COLORTERM=truecolor", "HOME=" + temp, "ARKEX_HOME=" + temp,
                binary, "--no-tools"])
            try:
                tmux("new-session", "-d", "-s", "test", "-x", "120", "-y", "30", "-c", temp, cmd)
                wait(lambda s: "Ask a question, describe a task" in s, "startup")
                send("Remember AZ-719. Make local changes only; do not publish.")
                wait(lambda s: "Original decision AZ-719" in s, "first response")
                send("Continue from that decision.")
                wait(lambda s: "waiting for model" in s, "compaction phase not visible")
                capture(mode + "-waiting")
                session_path = next((home / "sessions").glob("*/*.json"))
                saved = json.loads(session_path.read_text())
                assert "Continue from that decision" in json.dumps(saved["messages"]), "current prompt not saved before summary"
                if mode == "cancel":
                    tmux("send-keys", "-t", "test", "Escape")
                    wait(lambda s: "Press Esc again" in s, "cancel confirmation")
                    tmux("send-keys", "-t", "test", "Escape")
                    wait(lambda s: "cancelled" in s and "waiting for model" not in s, "cancel stuck")
                release.set()
                if mode == "success":
                    wait(lambda s: "CONTINUED_AFTER_COMPACTION" in s, "continuation failed")
                elif mode == "length":
                    wait(lambda s: "summary incomplete" in s, "truncated summary accepted")
                time.sleep(.3)
                capture(mode + "-done")
                saved = json.loads(session_path.read_text())
                assert "AZ-719" in json.dumps(saved["history"]), "original history lost"
                if mode == "success":
                    assert "Original conversation history:" in json.dumps(saved["messages"])
                    assert len(requests) == 3, len(requests)
                    assert requests[-1]["messages"][-1]["content"] == "Continue from that decision."
                    # Restart the actual CLI with its saved context, not a hand-built fixture.
                    env = dict(os.environ, ARKEX_HOME=temp)
                    result = subprocess.run([binary, "--no-tools", "--resume", saved["id"], "--print", "Continue again"],
                                            cwd=temp, env=env, capture_output=True, text=True, timeout=15)
                    assert result.returncode == 0 and "CONTINUED_AFTER_COMPACTION" in result.stdout, result.stderr
                else:
                    assert "Original conversation history:" not in json.dumps(saved["messages"])
                    assert len(requests) == 2, "failed compaction sent another request"
                print(f"PASS {mode}: real TUI, phase visible, saved original, replacement/continuation checked", flush=True)
            finally:
                release.set()
                subprocess.run(["tmux", "-L", socket, "kill-server"], capture_output=True)
    print("ALL COMPACTION E2E CHECKS PASSED")
finally:
    server.shutdown()
    server.server_close()
