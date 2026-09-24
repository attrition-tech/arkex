#!/usr/bin/env python3
"""Native binary smoke test: python scripts/smoke.py path/to/arkex[.exe].

No credentials or external network; isolated home/workspace, ephemeral HTTP port.
Runs on Linux, macOS and Windows without a POSIX shell.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from http.server import HTTPServer

from fake_openai import Handler


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
            env = dict(os.environ, ARKEX_HOME=str(home), HOME=str(home), USERPROFILE=str(home), NO_COLOR="1")

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
            result = run("--json", "--mode", "auto", "-p", "what is in go.mod?")
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


if __name__ == "__main__":
    main()
