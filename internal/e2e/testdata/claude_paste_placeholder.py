# Fake Claude Code Worker for paste-submission E2E tests.
#
# Claude Code collapses a multi-line paste into a "[Pasted text #N +M lines]"
# placeholder. An Enter that arrives while the placeholder is being created is
# swallowed, so the paste stays unsubmitted in the input box. This fake keeps
# the pane in raw mode with bracketed paste enabled, shows the same
# placeholder, and drops every Enter received within SWALLOW_SECONDS of a
# multi-line paste. A later Enter submits the paste as a new turn.
import os
import re
import subprocess
import sys
import termios
import time
import tty

SWALLOW_SECONDS = 1.0
PASTE_START = b"\x1b[200~"
PASTE_END = b"\x1b[201~"

root = os.environ["POSSE_TEST_ROOT"]
pane = os.environ["HERDR_PANE_ID"]
posse = os.environ["POSSE_E2E_POSSE_BIN"]


def report(state):
    subprocess.run(["herdr", "pane", "report-agent", pane, "--source", "posse.fake", "--agent", "claude", "--state", state], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def append(name, text):
    with open(os.path.join(root, name), "a") as log:
        log.write(text + "\n")


def run(*args):
    with open(os.path.join(root, "worker-delivery.log"), "a") as log:
        subprocess.run(list(args), stdout=log, stderr=subprocess.STDOUT)


def lead():
    report("idle")
    for line in sys.stdin:
        append("lead-prompts.log", line.rstrip("\n"))


if not os.getcwd().startswith(os.environ["POSSE_E2E_WORKTREES"] + "/"):
    lead()
    sys.exit(0)

task_id = None
pr_url = None


def handle(text):
    global task_id, pr_url
    report("working")
    append("worker-input.log", text)
    if text.startswith("Read ") and text.endswith(" and follow it."):
        task_id = os.path.basename(os.path.dirname(text[len("Read "):-len(" and follow it.")]))
        pr_url = "https://github.com/acme/shop/pull/%d" % (16 + int(task_id[1:]))
        with open("e2e-worker-%s.txt" % task_id, "w") as change:
            change.write("worker change %s\n" % task_id)
        subprocess.run(["git", "add", "e2e-worker-%s.txt" % task_id], check=True)
        subprocess.run(["git", "commit", "-q", "-m", "worker change " + task_id], check=True)
        run(posse, "publish", "Worker committed " + task_id)
        run(posse, "signal", "done", "Worker committed " + task_id, "--pr", pr_url)
        report("idle")
        open(os.path.join(root, "worker-idle-" + task_id), "w").close()
        return
    match = re.search(r"\| turn ([^\]]+)\]", text)
    if "Required CI checks failed" in text:
        with open("e2e-fix-%s.txt" % task_id, "w") as change:
            change.write("fix change %s\n" % task_id)
        subprocess.run(["git", "add", "e2e-fix-%s.txt" % task_id], check=True)
        subprocess.run(["git", "commit", "-q", "-m", "fix worker change " + task_id], check=True)
        run(posse, "publish", "Worker fixed " + task_id)
    if match:
        run(posse, "signal", "done", "Worker handled instruction", "--pr", pr_url, "--turn", match.group(1))
    report("idle")


def render(display):
    sys.stdout.write("\r\x1b[2K> " + display)
    sys.stdout.flush()


attributes = termios.tcgetattr(0)
tty.setraw(0)
try:
    sys.stdout.write("\x1b[?2004h")
    render("")
    report("idle")
    pending = b""
    pasted = None
    display = ""
    pastes = 0
    swallow_until = 0.0
    while True:
        chunk = os.read(0, 65536)
        if not chunk:
            break
        pending += chunk
        while pending:
            if pending.startswith(PASTE_START):
                end = pending.find(PASTE_END)
                if end < 0:
                    break
                text = pending[len(PASTE_START):end].decode()
                pending = pending[end + len(PASTE_END):]
                pasted = text
                if "\n" in text:
                    pastes += 1
                    display = "[Pasted text #%d +%d lines]" % (pastes, text.count("\n"))
                    swallow_until = time.monotonic() + SWALLOW_SECONDS
                else:
                    display = text
                render(display)
                continue
            if PASTE_START.startswith(pending):
                break
            byte, pending = pending[:1], pending[1:]
            if byte != b"\r":
                continue
            if time.monotonic() < swallow_until:
                append("swallowed-enter.log", display)
                continue
            if pasted is None:
                continue
            text, pasted, display = pasted, None, ""
            sys.stdout.write("\r\n")
            render(display)
            handle(text)
finally:
    termios.tcsetattr(0, termios.TCSADRAIN, attributes)
