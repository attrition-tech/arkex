#!/usr/bin/env python3
"""Tiny OpenAI-compatible streaming server for arkex smoke tests.

Usage: fake_openai.py [host:port]   (default 127.0.0.1:8765)

Behaviour is keyed off the last user message:
  contains "ping"          -> streams "pong"
  contains "run"           -> asks to run a bash command, then echoes the result
  contains "patch <path>"  -> asks to edit <path> (hello -> hello, world), then confirms
  contains "write"         -> writes demo/ApplicationsPage.jsx (62 lines), then confirms
  contains "markdown"      -> streams a markdown sample (heading, list, code, table)
  contains "overflow"      -> rejects the request as too long (window 262144) until the
                              conversation has been compacted, then answers
  non-streaming request    -> a JSON completion holding a fake hand-off summary
  otherwise, first turn    -> reasoning + text + a `read go.mod` tool call
  otherwise, after a tool  -> a short markdown answer
"""
import json
import socket
import struct
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


def chunk(delta, finish=None):
    return json.dumps({
        "id": "c", "object": "chat.completion.chunk", "created": 1, "model": "m",
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    })


USAGE = json.dumps({
    "id": "c", "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": [],
    "usage": {"prompt_tokens": 42, "completion_tokens": 7, "total_tokens": 49},
})


def tool_call(cid, name, args):
    return chunk({"role": "assistant", "tool_calls": [{
        "index": 0, "id": cid, "type": "function",
        "function": {"name": name, "arguments": json.dumps(args)},
    }]})


class Handler(BaseHTTPRequestHandler):
    network_tests = {}

    def reset_stream(self, content=""):
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", "1000000")
        self.end_headers()
        self.wfile.write(("data: " + chunk({"role": "assistant", "content": content}) + "\n\n").encode())
        self.wfile.flush()
        time.sleep(.2)
        # A real TCP reset on close, not graceful EOF (which older arkex
        # already retries). This produces a raw SSE socket-read error.
        self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        self.close_connection = True
        self.rfile.close()
        self.wfile.close()
        self.connection.close()

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path == "/network-test-state":
            self.send_json(200, self.network_tests)
            return
        if not self.path.endswith("/models"):
            self.send_error(404)
            return
        if self.headers.get("authorization", "") == "Bearer bad":
            self.send_error(401)
            return
        body = json.dumps({"object": "list", "data": [
            {"id": "m", "object": "model"},
            {"id": "deepseek-v4-flash", "object": "model"},
            {"id": "qwen3-next-fp8", "object": "model"},
            {"id": "plain-chat", "object": "model"},
        ]}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def send_json(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        msgs = body.get("messages", [])
        tool_msgs = [m for m in msgs if m.get("role") == "tool"]
        last_user = str([m for m in msgs if m.get("role") == "user"][-1]["content"]).lower()
        compacted = any("summary of the conversation so far" in str(m.get("content", "")).lower() for m in msgs)
        delay = 0.02

        if not body.get("stream"):
            # Compaction and opt-in session titles use non-streaming completions.
            title_request = any("Suggest a short descriptive session title" in str(m.get("content", ""))
                                for m in msgs if m.get("role") == "system")
            content = "Improve deployment checks" if title_request else f"The user sent {len(msgs)} messages; the last one asked: {last_user[:60]!r}."
            reply = {"id": "s1", "object": "chat.completion", "created": 1, "model": "m",
                     "choices": [{"index": 0, "finish_reason": "stop", "message": {
                         "role": "assistant",
                         "content": content}}],
                     "usage": {"prompt_tokens": 1200, "completion_tokens": 30, "total_tokens": 1230}}
            self.send_json(200, reply)
            return
        if "overflow" in last_user and not compacted:
            self.send_json(400, {"error": {
                "message": "This model's maximum context length is 262144 tokens. However, your messages resulted in 245761 tokens. Please reduce the length of the messages.",
                "type": "invalid_request_error", "param": "messages", "code": "context_length_exceeded"}})
            return

        if last_user.startswith("network-test "):
            # Local deterministic fault injection, used by network_e2e.py.
            mode, key = last_user.split()[1:3]
            state = self.network_tests.setdefault(key, {"requests": 0})
            state["requests"] += 1
            attempt = state["requests"]
            state["has_tool_result"] = any("TOOL_ONCE" in str(m.get("content")) for m in tool_msgs)
            state["has_partial_history"] = any("UNFINISHED_NETWORK" in str(m.get("content")) for m in msgs)
            if attempt == 1:
                parts = [tool_call("network_command", "bash", {"command": "printf X >> executions; echo TOOL_ONCE"}), chunk({}, "tool_calls")]
            elif (mode == "recover" and attempt == 2) or (mode in ("exhaust", "cancel", "waitcancel") and attempt <= 9):
                self.reset_stream()
                return
            elif mode in ("partial", "partialcancel") and attempt == 2:
                self.reset_stream("UNFINISHED_NETWORK")
                return
            elif mode in ("malformed", "incomplete", "malformedexhaust") and attempt <= (4 if mode == "malformedexhaust" else 3):
                if mode == "incomplete":
                    parts = [chunk({"content": "UNFINISHED_NETWORK"})]
                else:
                    parts = [chunk({"content": '<|DSML|parameter name="command" string="true">UNFINISHED_NETWORK'}), chunk({}, "stop")]
            else:
                parts = [chunk({"content": "NETWORK_RECOVERED"}), chunk({}, "stop")]
        elif last_user == "grouping-test":
            user_index = max(i for i, m in enumerate(msgs) if m.get("role") == "user")
            step = sum(m.get("role") == "tool" for m in msgs[user_index+1:])
            if step < 9:
                if step == 2:
                    name, args = "read", {"path": "intentionally-missing.txt"}
                else:
                    command = f"printf 'RESULT_{step}\\n'"
                    if step == 7:
                        command = "printf 'FIRST_LINE\\n'\nprintf 'SECOND_LINE\\n'"
                    elif step == 8:
                        command = "sleep 20; printf 'LAST_RESULT\\n'"
                    name, args = "bash", {"command": command}
                parts = [chunk({"role": "assistant", "reasoning_content": f"Checking action {step + 1}."}),
                         tool_call(f"group_{step}", name, args), chunk({}, "tool_calls")]
            else:
                parts = [chunk({"content": "GROUPING_COMPLETE"}), chunk({}, "stop")]
        elif last_user.startswith("workspace-test "):
            # Explicit tool requests for the workspace policy E2E harness.
            user_index = max(i for i, m in enumerate(msgs) if m.get("role") == "user")
            results = [m for m in msgs[user_index+1:] if m.get("role") == "tool"]
            if results:
                parts = [chunk({"role": "assistant", "content": "RESULT: " + str(results[-1]["content"])}), chunk({}, "stop")]
            else:
                spec = json.loads(msgs[user_index]["content"].split(" ", 1)[1])
                parts = [tool_call("workspace_call", spec["name"], spec["args"]), chunk({}, "tool_calls")]
        elif "ping" in last_user or "pong" in last_user:
            parts = [chunk({"role": "assistant", "content": "pong"}), chunk({}, "stop")]
        elif "run" in last_user and not tool_msgs:
            cmd = "sleep 30; echo late" if "slow" in last_user else "echo hello from bash"
            parts = [tool_call("call_b", "bash", {"command": cmd}), chunk({}, "tool_calls")]
        elif "run" in last_user:
            parts = [chunk({"role": "assistant", "content": "Tool said: " + str(tool_msgs[-1]["content"])[:80]}),
                     chunk({}, "stop")]
        elif "patch" in last_user and not tool_msgs:
            path = last_user.split("patch", 1)[1].strip() or "sample.txt"
            parts = [tool_call("call_e", "edit", {"path": path, "old_string": "hello", "new_string": "hello, world\nsecond line"}),
                     chunk({}, "tool_calls")]
        elif "patch" in last_user:
            parts = [chunk({"role": "assistant", "content": "Edited: " + str(tool_msgs[-1]["content"])[:80]}), chunk({}, "stop")]
        elif "write" in last_user and not tool_msgs:
            content = "\n".join(f"line {i}" for i in range(62)) + "\n"
            parts = [chunk({"role": "assistant", "content": "Writing the file."}),
                     tool_call("call_w", "write", {"path": "demo/ApplicationsPage.jsx", "content": content}),
                     chunk({}, "tool_calls")]
        elif "write" in last_user:
            # Reproduces a streaming glitch: a whitespace-only reasoning delta in the middle of text.
            parts = [chunk({"role": "assistant", "content": "Extern"}), chunk({"reasoning_content": "\n"}),
                     chunk({"content": "ally managed file written."}), chunk({}, "stop")]
        elif "long" in last_user:
            # A fast model: ~1500 word-sized deltas, 2 ms apart (for perf checks).
            para = ("The quick brown fox jumps over the lazy dog while the agent streams text. ")
            text = "## Long answer\n\n" + "\n\n".join(
                f"Paragraph {i}. " + para * 3 + f"\n\n- point one\n- point two\n\n```go\nfmt.Println({i})\n```"
                for i in range(1, 41))
            words = text.split(" ")
            parts = [chunk({"role": "assistant", "content": w + " "}) for w in words]
            parts.append(chunk({}, "stop"))
            delay = 0.002
        elif "overflow" in last_user:
            parts = [chunk({"role": "assistant", "content": "Recovered: the conversation was compacted and this request fit."}),
                     chunk({}, "stop")]
        elif "markdown" in last_user:
            text = ("## Report\n\nThis is **bold**, this is `inline code`, and [a link](https://example.com).\n\n"
                    "1. first\n2. second\n\n```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n"
                    "| col | val |\n|---|---|\n| a | 1 |\n| b | 2 |\n\n> quoted line\n\nDone.")
            parts = [chunk({"role": "assistant", "content": text[i:i+7]}) for i in range(0, len(text), 7)]
            parts.append(chunk({}, "stop"))
        elif not tool_msgs:
            parts = [
                chunk({"role": "assistant", "reasoning_content": "The user wants the file. I will read it."}),
                chunk({"content": "Let me look at that file."}),
                tool_call("call_1", "read", {"path": "go.mod"}),
                chunk({}, "tool_calls"),
            ]
        else:
            text = "Here is what I found:\n\n- module `github.com/attrition-tech/arkex`\n- Go 1.27\n\nDone."
            parts = [chunk({"role": "assistant", "content": w + " "}) for w in text.split(" ")]
            parts.append(chunk({}, "stop"))

        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        for p in parts:
            self.wfile.write(f"data: {p}\n\n".encode())
            self.wfile.flush()
            time.sleep(delay)
        self.wfile.write(f"data: {USAGE}\n\ndata: [DONE]\n\n".encode())


if __name__ == "__main__":
    host, _, port = (sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1:8765").rpartition(":")
    HTTPServer((host or "127.0.0.1", int(port)), Handler).serve_forever()
