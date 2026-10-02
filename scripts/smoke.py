#!/usr/bin/env python3
"""Native binary smoke test: python scripts/smoke.py path/to/arkex.

No credentials or external network; isolated home/workspace, ephemeral HTTP port.
Runs on Linux and macOS without relying on shell behavior.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from datetime import datetime, timedelta, timezone
from http.server import HTTPServer

from fake_openai import Handler


class ChatGPTHandler(Handler):
    """Exercise the public Responses wire format with disposable credentials."""

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        self.server.requests.append(body)
        assert self.path == "/v1/responses"
        assert self.headers["authorization"] == "Bearer smoke-access"
        assert not self.headers.get("ChatGPT-Account-ID")
        assert body["store"] is False and body["stream"] is True
        assert "previous_response_id" not in body and "max_output_tokens" not in body
        namespace = body["tools"][0]
        assert namespace["type"] == "namespace" and namespace["name"] == "arkex"
        assert "read" in [tool["name"] for tool in namespace["tools"]]
        history = body["input"]
        user = next(item for item in history if item.get("role") == "user")
        fail = "fail" in str(user)
        incomplete = "incomplete" in str(user)
        results = [item for item in history if item.get("type") == "function_call_output"]
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()

        def event(kind, **fields):
            self.wfile.write(("data: " + json.dumps({"type": kind, **fields}) + "\n\n").encode())
            self.wfile.flush()

        if not results:
            call = {"type": "function_call", "id": "fc_1", "call_id": "read_1",
                    "namespace": "arkex", "name": "read", "arguments": ""}
            event("response.output_item.added", output_index=0, item=call)
            event("response.output_item.done", output_index=0,
                  item={**call, "arguments": json.dumps({"path": "go.mod"})})
        else:
            assert len(results) == 1 and results[0]["call_id"] == "read_1"
            assert "module example.com/chatgpt-smoke" in results[0]["output"]
            call = next(item for item in history if item.get("type") == "function_call")
            assert call["namespace"] == "arkex" and call["name"] == "read"
            assert call["call_id"] == "read_1"
            item = {"type": "message", "id": "msg_1", "role": "assistant", "content": []}
            event("response.output_item.added", output_index=0, item=item)
            event("response.output_text.delta", output_index=0, content_index=0,
                  item_id="msg_1", delta="Verified the module.")
            event("response.output_item.done", output_index=0,
                  item={**item, "content": [{"type": "output_text", "text": "Verified the module."}]})
        if fail:
            event("response.failed", response={"error": {
                "code": "subscription_sharing_usage_limit_exceeded", "message": "Plan limit reached"}})
        elif incomplete:
            # No reason: the SDK alone used to mistake this for tool-call success.
            event("response.incomplete", response={"status": "incomplete"})
        else:
            event("response.completed", response={"status": "completed", "usage": {
                "input_tokens": 100, "output_tokens": 20, "total_tokens": 120}})


def chatgpt_smoke(binary):
    server = HTTPServer(("127.0.0.1", 0), ChatGPTHandler)
    server.requests = []
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="arkex chatgpt smoke ") as directory:
            root = Path(directory)
            home, work = root / "home", root / "workspace"
            home.mkdir()
            work.mkdir()
            (work / "go.mod").write_text("module example.com/chatgpt-smoke\n")
            (home / "config.json").write_text(json.dumps({"version": 4, "connections": {
                "chatgpt": {"kind": "subscription", "subscription": "chatgpt", "api": "openai",
                            "baseUrl": f"http://127.0.0.1:{server.server_port}/v1",
                            "models": [{"id": "test-model", "contextWindow": 100000}]}},
                "default": "chatgpt/test-model"}))
            credentials = {"version": 1, "chatgpt": {"chatgpt": {
                "clientId": "oaiapp_smoke", "issuer": "https://auth.openai.com", "subject": "smoke-user",
                "accessToken": "smoke-access", "scopes": "resource.invoke chatgpt.tokens.use.direct",
                "expiresAt": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat()}}}
            auth = home / "auth.json"
            auth.write_text(json.dumps(credentials))
            auth.chmod(0o600)
            env = dict(os.environ, ARKEX_HOME=str(home), HOME=str(home), NO_COLOR="1")
            for prompt in ("read module", "fail before executing tools", "incomplete before executing tools"):
                server.requests.clear()
                result = subprocess.run([binary, "--json", "-p", prompt], cwd=work, env=env,
                                        capture_output=True, text=True, timeout=60)
                events = [json.loads(line) for line in result.stdout.splitlines()]
                assert events and events[-1]["type"] == "run_end", (result.stdout, result.stderr)
                tools = [e["data"] for e in events if e["type"] == "tool_result"]
                if prompt == "read module":
                    assert result.returncode == 0, result.stderr
                    assert len(server.requests) == 2 and len(tools) == 1, events
                    assert "module example.com/chatgpt-smoke" in tools[0]["output"]
                    assert not tools[0]["is_error"] and not events[-1]["data"].get("error")
                    assert any(e["type"] == "text_delta" and "Verified the module." in json.dumps(e) for e in events)
                else:
                    assert result.returncode != 0 and events[-1]["data"].get("error"), events
                    assert not tools and len(server.requests) == 1, events
                assert list(work.iterdir()) == [work / "go.mod"], "workspace polluted"
            print("PASS: ChatGPT native binary namespaced file-tool round trip; failed/incomplete responses execute no tools")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    binary = str(Path(sys.argv[1]).resolve())
    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="arkex smoke ") as directory:
            root = Path(directory)
            home = root / "home"
            work = root / "workspace"
            home.mkdir()
            work.mkdir()
            config = json.loads(Path(__file__).with_name("fake_config.json").read_text())
            config["connections"]["fake"]["baseUrl"] = f"http://127.0.0.1:{server.server_port}/v1"
            (home / "config.json").write_text(json.dumps(config), encoding="utf-8")
            (work / "go.mod").write_text("module example.com/arkex-smoke\n", encoding="utf-8")
            env = dict(os.environ, ARKEX_HOME=str(home), HOME=str(home), NO_COLOR="1")

            def run(*args):
                result = subprocess.run([binary, *args], cwd=work, env=env,
                                        capture_output=True, text=True, encoding="utf-8", timeout=60)
                if result.returncode:
                    raise AssertionError(f"{args}: exit {result.returncode}\n{result.stdout}\n{result.stderr}")
                return result

            assert "deepseek-v4-flash" in run("models", "list").stdout
            assert run("-p", "ping").stdout.strip() == "pong"
            events = [json.loads(line) for line in run("--json", "-p", "ping").stdout.splitlines()]
            assert any(e["type"] == "text_delta" and "pong" in json.dumps(e["data"]) for e in events), events
            assert events[-1]["type"] == "run_end", events
            assert not events[-1]["data"].get("error"), events[-1]
            result = run("--json", "-p", "what is in go.mod?")
            events = [json.loads(line) for line in result.stdout.splitlines()]
            results = [e["data"] for e in events if e["type"] == "tool_result"]
            assert len(results) == 1 and not results[0]["is_error"], results
            assert "module example.com/arkex-smoke" in results[0]["output"], results
            assert events[-1]["type"] == "run_end" and not events[-1]["data"].get("error"), events[-1]
            assert list(work.iterdir()) == [work / "go.mod"], "workspace polluted"
            print("PASS: native binary model listing, streaming print/JSON, file-tool loop, isolated workspace")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
    chatgpt_smoke(binary)


if __name__ == "__main__":
    main()
