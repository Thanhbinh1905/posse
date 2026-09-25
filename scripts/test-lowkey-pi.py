#!/usr/bin/env python3
"""Smoke the generated extension in an isolated real Herdr/Pi Lead TUI.

Requires herdr, pi, Go and Python 3. No external model or credentials are used.
"""
import json
import os
from pathlib import Path
import re
import sqlite3
from contextlib import closing
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parent.parent


class Model(BaseHTTPRequestHandler):
    calls = 0
    requests = []
    lock = threading.Lock()

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers["Content-Length"])
        body = json.loads(self.rfile.read(length))
        with self.lock:
            type(self).calls += 1
            type(self).requests.append(body)
        text = json.dumps(body.get("messages", []))
        last = body.get("messages", [])[-1]
        latest_user = next((json.dumps(msg) for msg in reversed(body.get("messages", [])) if msg.get("role") == "user"), "")
        if "custom probe" in latest_user and last.get("role") == "tool":
            delta = {"content": "FINAL_CUSTOM_REPLY"}
            reason = "stop"
        elif "custom probe" in latest_user:
            delta = {"content": "MID_CUSTOM_TEXT", "tool_calls": [{"index": 0, "id": "call_custom", "type": "function", "function": {"name": "smoke_tool", "arguments": "{}"}}]}
            reason = "tool_calls"
        elif "tool probe" in latest_user and last.get("role") == "tool":
            delta = {"content": "FINAL_TOOL_REPLY"}
            reason = "stop"
        elif "tool probe" in latest_user:
            delta = {"content": "MID_TURN_TEXT", "tool_calls": [{"index": 0, "id": "call_probe", "type": "function", "function": {"name": "bash", "arguments": json.dumps({"command": "printf TOOL_RESULT_MARKER"})}}]}
            reason = "tool_calls"
        elif "thinking probe" in latest_user:
            delta = {"reasoning_content": "PRIVATE_THINKING_PROBE", "content": "Probe answered."}
            reason = "stop"
        elif "context probe" in latest_user:
            delta = {"content": "Context answered."}
            reason = "stop"
        elif last.get("role") == "tool":
            answer = "I have acknowledged the Notice. User, please choose A or B." if "needs-decision" in latest_user else "The pull request opened; I reported and acknowledged it."
            delta = {"content": answer}
            reason = "stop"
        elif "needs-decision" in latest_user:
            ids = re.findall(r"Notice ids: ([0-9]+)", text)
            assert ids, text[-500:]
            delta = {"tool_calls": [{"index": 0, "id": "call_ack", "type": "function", "function": {"name": "bash", "arguments": json.dumps({"command": f"posse ack {ids[-1]}"})}}]}
            reason = "tool_calls"
        else:
            delta = {"tool_calls": [{"index": 0, "id": "call_ack", "type": "function", "function": {"name": "bash", "arguments": json.dumps({"command": "posse ack all"})}}]}
            reason = "tool_calls"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        chunks = [delta]
        if delta.get("content") == "FINAL_TOOL_REPLY":
            chunks = [{"content": "FINAL_TOOL_"}, {"content": "REPLY"}]
        elif delta.get("content") == "MID_TURN_TEXT":
            chunks = [{"content": "MID_TURN_TEXT"}, {"tool_calls": delta["tool_calls"]}]
        for chunk in chunks:
            payload = {"choices": [{"index": 0, "delta": {"role": "assistant", **chunk}, "finish_reason": None}]}
            self.wfile.write(b"data: " + json.dumps(payload).encode() + b"\n\n")
            self.wfile.flush()
            if len(chunks) > 1:
                time.sleep(1 if delta.get("content") == "FINAL_TOOL_REPLY" else .6)
        self.wfile.write(b"data: " + json.dumps({"choices": [{"index": 0, "delta": {}, "finish_reason": reason}]}).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


def main():
    with tempfile.TemporaryDirectory(prefix="posse-pi-lowkey-") as tmp:
        tmp = Path(tmp)
        env = {k: v for k, v in os.environ.items() if not k.startswith("HERDR_") and k not in ("PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL")}
        # Reuse downloaded Go modules/build artifacts; only the Herdr, Posse
        # and Pi session state belongs in this disposable home.
        env.update(HOME=str(tmp), XDG_CONFIG_HOME=str(tmp / "config"), POSSE_HOME=str(tmp / "home"), PI_CODING_AGENT_DIR=str(tmp / "pi"), PI_OFFLINE="1")
        for name in ("GOMODCACHE", "GOCACHE"):
            env[name] = subprocess.check_output(("go", "env", name), text=True).strip()
        (tmp / "config").mkdir()
        (tmp / ".zshrc").write_text("# isolated test shell\n")
        (tmp / "pi").mkdir()
        (tmp / "bin").mkdir()
        env["PATH"] = str(tmp / "bin") + os.pathsep + env["PATH"]
        repo = tmp / "repo"
        repo.mkdir()

        def run(*args, cwd=repo, timeout=30):
            return subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout, check=True).stdout

        run("git", "init", "-q", "-b", "main")
        run("git", "-c", "user.name=Smoke", "-c", "user.email=smoke@example.test", "commit", "-q", "--allow-empty", "-m", "initial")
        run("go", "build", "-o", str(tmp / "bin" / "posse"), "./cmd/posse", cwd=ROOT, timeout=120)
        run("posse", "config", "set", "lowkey.lead", "false")
        server = ThreadingHTTPServer(("127.0.0.1", 0), Model)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        port = server.server_port
        (tmp / "pi" / "models.json").write_text(json.dumps({"providers": {"smoke": {"baseUrl": f"http://127.0.0.1:{port}/v1", "api": "openai-completions", "apiKey": "test", "models": [{"id": "smoke", "contextWindow": 128000, "reasoning": True}]}}}))
        original_settings = {"defaultProvider": "smoke", "defaultModel": "smoke", "defaultThinkingLevel": "xhigh"}
        (tmp / "pi" / "settings.json").write_text(json.dumps(original_settings))
        extensions = tmp / "pi" / "extensions"
        extensions.mkdir()
        (extensions / "smoke.ts").write_text('''import { Type } from "@earendil-works/pi-ai";
export default function (pi) {
    pi.registerTool({ name: "smoke_tool", label: "Smoke tool", description: "Custom smoke probe",
        parameters: Type.Object({}),
        async execute() { return { content: [{ type: "text", text: "CUSTOM_RESULT_MARKER" }], details: undefined }; }
    });
}
''')
        session = "lowkey-smoke-" + str(os.getpid())
        herdr = ("herdr", "--session", session)
        log = (tmp / "server.log").open("w")
        process = subprocess.Popen((*herdr, "server"), env=env, cwd=repo, stdout=log, stderr=log)
        try:
            def api(*args):
                return json.loads(run(*herdr, *args))

            def until(check, label, timeout=35):
                end = time.monotonic() + timeout
                while time.monotonic() < end:
                    try:
                        result = check()
                        if result:
                            return result
                    except (subprocess.CalledProcessError, sqlite3.OperationalError):
                        pass
                    time.sleep(.2)
                raise AssertionError(f"timed out waiting for {label}; Herdr log: {log.name}")

            workspace = until(lambda: api("workspace", "create", "--cwd", str(repo), "--label", "smoke", "--no-focus"), "isolated Herdr server")
            pane = workspace["result"]["root_pane"]["pane_id"]
            run(*herdr, "pane", "run", pane, f"PATH={tmp / 'bin'}:$PATH POSSE_HOME={tmp / 'home'} XDG_CONFIG_HOME={tmp / 'config'} PI_CODING_AGENT_DIR={tmp / 'pi'} PI_OFFLINE=1 {tmp / 'bin' / 'posse'} up --pi --name smoke --yes")
            try:
                until(lambda: any(a.get("agent_status") == "idle" for a in api("agent", "list")["result"]["agents"]), "Pi Lead startup")
            except AssertionError as exc:
                raise AssertionError(f"{exc}; agents={api('agent', 'list')}; pane={run(*herdr, 'pane', 'read', pane, '--source', 'recent-unwrapped', '--lines', '70')[-2500:]}; server={(tmp / 'server.log').read_text()[-1500:]}") from exc
            db = tmp / "home" / "posse.db"

            def notice(kind, summary):
                with closing(sqlite3.connect(db)) as con:
                    project = con.execute("SELECT id FROM projects WHERE name='smoke'").fetchone()[0]
                    cursor = con.execute("INSERT INTO notices(project_id, kind, summary, created_at) VALUES (?,?,?,?)", (project, kind, summary, int(time.time() * 1000)))
                    con.commit()
                    return cursor.lastrowid

            def state(id):
                with closing(sqlite3.connect(db)) as con:
                    return con.execute("SELECT delivered_at IS NOT NULL, acked_at IS NOT NULL FROM notices WHERE id=?", (id,)).fetchone()

            def screen_text():
                return run(*herdr, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "80")

            run(*herdr, "pane", "run", pane, "tool probe")
            until(lambda: "FINAL_TOOL_REPLY" in screen_text(), "tool reply before lowkey")
            assert "MID_TURN_TEXT" in screen_text() and "TOOL_RESULT_MARKER" in screen_text(), "tool call not visible before lowkey"
            run(*herdr, "pane", "run", pane, "thinking probe")
            until(lambda: "PRIVATE_THINKING_PROBE" in screen_text(), "visible expanded thinking before lowkey")
            run(*herdr, "pane", "run", pane, "/lowkey on")
            until(lambda: "lowkey on" in screen_text(), "in-session lowkey on")
            assert "PRIVATE_THINKING_PROBE" not in screen_text(), "old thinking remained visible after lowkey on"
            assert "tool probe" in screen_text() and "FINAL_TOOL_REPLY" in screen_text(), "User request or final reply hidden"
            assert "MID_TURN_TEXT" not in screen_text() and "TOOL_RESULT_MARKER" not in screen_text(), "tool step remained visible after lowkey on"
            assert "Thinking..." not in screen_text(), "collapsed thinking label remained visible"
            run(*herdr, "pane", "run", pane, "tool probe")
            until(lambda: Model.calls >= 4, "tool preamble started while lowkey on")
            time.sleep(.3)
            assert "MID_TURN_TEXT" not in screen_text(), "streaming tool preamble leaked"
            until(lambda: screen_text().count("FINAL_TOOL_") >= 2 and screen_text().count("FINAL_TOOL_REPLY") == 1, "streaming final reply while lowkey on")
            until(lambda: screen_text().count("FINAL_TOOL_REPLY") == 2, "second final reply while lowkey on")
            assert "MID_TURN_TEXT" not in screen_text() and "TOOL_RESULT_MARKER" not in screen_text(), "new tool row or mid-turn text visible"
            run(*herdr, "pane", "run", pane, "custom probe")
            until(lambda: "FINAL_CUSTOM_REPLY" in screen_text(), "custom tool final reply")
            assert "MID_CUSTOM_TEXT" not in screen_text() and "CUSTOM_RESULT_MARKER" not in screen_text(), "custom tool row visible"
            run(*herdr, "pane", "run", pane, "/export " + str(tmp / "export.jsonl"))
            until(lambda: (tmp / "export.jsonl").exists(), "session export")
            exported = (tmp / "export.jsonl").read_text()
            assert all(marker in exported for marker in ("MID_TURN_TEXT", "TOOL_RESULT_MARKER", "MID_CUSTOM_TEXT", "CUSTOM_RESULT_MARKER")), "hidden content missing from export"
            run(*herdr, "pane", "run", pane, "/lowkey off")
            until(lambda: "PRIVATE_THINKING_PROBE" in screen_text() and "MID_CUSTOM_TEXT" in screen_text() and "CUSTOM_RESULT_MARKER" in screen_text(), "hidden rows restored after lowkey off")
            settings = json.loads((tmp / "pi" / "settings.json").read_text())
            assert settings["defaultThinkingLevel"] == "xhigh" and "hideThinkingBlock" not in settings, f"lowkey changed thinking settings: {settings}"
            run(*herdr, "pane", "run", pane, "/lowkey")
            until(lambda: "PRIVATE_THINKING_PROBE" not in screen_text(), "thinking hidden again by toggle")
            run(*herdr, "pane", "run", pane, "context probe")
            until(lambda: "Context answered." in screen_text(), "next model turn")
            assert all(marker in json.dumps(Model.requests[-1]["messages"]) for marker in ("PRIVATE_THINKING_PROBE", "MID_TURN_TEXT", "TOOL_RESULT_MARKER", "MID_CUSTOM_TEXT", "CUSTOM_RESULT_MARKER")), "hidden content missing from model context"
            run(*herdr, "pane", "run", pane, "/lowkey status")
            until(lambda: "lowkey on" in screen_text(), "in-session status")
            project_dir = tmp / "home" / "projects" / "smoke"
            project_dir.chmod(0o555)
            try:
                run(*herdr, "pane", "run", pane, "/lowkey off")
                until(lambda: "lowkey unchanged:" in screen_text(), "failed write notification")
                assert "PRIVATE_THINKING_PROBE" not in screen_text(), "failed write changed presentation"
            finally:
                project_dir.chmod(0o755)
            assert "lowkey: on" in run("posse", "lowkey", "status"), "failed write changed setting"
            routine = notice("pr_opened", "quiet PR")
            until(lambda: state(routine) == (1, 1), "quiet routine ack")
            assert Model.calls == 8 and "[posse | Posse -> Lead" not in screen_text(), f"routine generated a visible turn: calls={Model.calls} screen={screen_text()[-1800:]}"
            run(*herdr, "pane", "send-text", pane, "unfinished User draft")
            decision = notice("needs_decision", "Choose A or B")
            until(lambda: state(decision) == (1, 1), "actionable Lead ack")
            until(lambda: "please choose A or B" in screen_text(), "actionable Lead report")
            assert "[posse | Posse -> Lead" not in screen_text(), "raw hidden wake was rendered"
            sessions = list((tmp / "pi" / "sessions").rglob("*.jsonl"))
            assert sessions, "Pi session was not persisted"
            entries = [json.loads(line) for path in sessions for line in path.read_text().splitlines()]
            wakes = [entry for entry in entries if entry.get("type") == "custom_message" and entry.get("customType") == "posse-notices"]
            assert all(any(marker in json.dumps(entry) for entry in entries) for marker in ("PRIVATE_THINKING_PROBE", "MID_TURN_TEXT", "TOOL_RESULT_MARKER", "MID_CUSTOM_TEXT", "CUSTOM_RESULT_MARKER")), "hidden content missing from session storage"
            assert len(wakes) == 1 and wakes[0]["details"]["origin"] == "posse" and wakes[0]["details"]["type"] == "notice" and wakes[0]["details"]["tasks"] == ["project"] and wakes[0]["details"]["ids"] == [decision] and wakes[0]["display"] is False, wakes
            assert not any(entry.get("message", {}).get("role") == "user" and "[posse |" in json.dumps(entry["message"]) for entry in entries), "Posse wake impersonated User chat"
            assert "unfinished User draft" in screen_text(), "Notice clobbered the User's composer"
            run("posse", "config", "set", "lowkey.lead", "false", "--project", "smoke")
            visible = notice("pr_opened", "visible PR")
            until(lambda: state(visible) == (1, 1), "visible ack")
            until(lambda: "The pull request opened" in screen_text(), "visible Lead report")
            assert "PRIVATE_THINKING_PROBE" in screen_text(), "external lowkey change did not restore thinking"
            assert "[posse | Posse -> Lead project | notice #" in screen_text(), "lowkey-off wake was not visible"
            entries = [json.loads(line) for path in sessions for line in path.read_text().splitlines()]
            wakes = [entry for entry in entries if entry.get("type") == "custom_message" and entry.get("customType") == "posse-notices"]
            assert len(wakes) == 2 and wakes[-1]["display"] is True and wakes[-1]["details"]["origin"] == "posse", wakes
            assert not any(entry.get("message", {}).get("role") == "user" and "[posse |" in json.dumps(entry["message"]) for entry in entries), "Visible Posse wake impersonated User chat"
            print("PASS: thinking, built-in and custom tool rows, and tool-step text hidden on lowkey; restored off; streamed reply, context, storage and export retained; Notice reporting")
        finally:
            subprocess.run((*herdr, "session", "stop", session), env=env, cwd=repo, capture_output=True, timeout=10)
            process.wait(timeout=10)
            log.close()
            server.shutdown()


if __name__ == "__main__":
    main()
