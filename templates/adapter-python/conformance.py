#!/usr/bin/env python3
"""Record and replay this adapter's conformance transcripts.

    python3 conformance.py record [case ...]   drive the adapter, write response.jsonl
    python3 conformance.py verify [case ...]   drive it again, diff against the recording

The recorder is the replayer. A transcript that was written by hand would be a
claim about the adapter rather than an observation of it, so `record` runs the
real binary in a real process and writes down what came back — and `verify`
takes the same path so that "recorded" and "checked" cannot drift apart.

This script exists so that a copy of this template verifies itself with nothing
but Python. It is not the authority: `recall doctor --conformance <adapter>`
replays the same directory through Recall's own engine, and that is what your
suite is finally held to. The two agree because the format is specified — see
`docs/adapter-protocol.md#conformance` and `cmd/recall-stream/conformance/FORMAT.md`
in the Recall tree — not because this file is clever.
"""

from __future__ import annotations

import json
import os
import queue
import shutil
import subprocess
import sys
import tempfile
import threading

HERE = os.path.dirname(os.path.abspath(__file__))
SUITE = os.path.join(HERE, "conformance")
ADAPTER = os.path.join(HERE, "recall_notes.py")

# Generous on purpose: a case that hits this has hung, and the difference
# between two seconds and twenty does not change that.
RESPONSE_TIMEOUT = 20.0
DRAIN_TIMEOUT = 2.0

VOLATILE_MARK = "<volatile>"


def load_manifest(case_dir: str) -> dict:
    """Read one manifest and hold it to the format.

    The checks here are about the transcript, not the adapter. A manifest that
    names a different case than its directory, or a description nobody wrote,
    is a defect in the suite, and catching it here is the difference between a
    clear failure and a confusing replay.
    """
    with open(os.path.join(case_dir, "manifest.json"), encoding="utf-8") as handle:
        manifest = json.load(handle)
    name = os.path.basename(case_dir.rstrip(os.sep))
    if manifest.get("case") != name:
        raise SystemExit("manifest names case %r, directory is %r" % (manifest.get("case"), name))
    if not (manifest.get("description") or "").strip():
        raise SystemExit(
            "case %s carries no description; a transcript nobody can read is not documentation"
            % name
        )
    if manifest.get("flow") != "lockstep":
        raise SystemExit("case %s declares flow %r; the format defines only lockstep" % (name, manifest.get("flow")))
    return manifest


def read_lines(path: str) -> list[str]:
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as handle:
        return [line.strip() for line in handle if line.strip()]


def bind(lines: list[str], manifest: dict, fixture: str, workdir: str) -> list[str]:
    """Substitute the placeholders, textually, before anything parses the line.

    A path goes into a JSON string literal, so it is escaped as JSON first. A
    temporary directory rarely needs it; a suite that failed only on a machine
    whose paths contain a backslash would be a miserable thing to debug.
    """
    declared = manifest.get("placeholders") or {}
    values = {"FIXTURE": fixture, "WORKDIR": workdir}
    out = []
    for i, line in enumerate(lines, start=1):
        for token, value in values.items():
            if "${%s}" % token in line:
                if token not in declared:
                    raise SystemExit(
                        "case %s: request %d uses ${%s}, which the manifest does not declare"
                        % (manifest["case"], i, token)
                    )
                line = line.replace("${%s}" % token, json.dumps(value)[1:-1])
        if "${" in line:
            raise SystemExit("case %s: request %d carries an unbound placeholder" % (manifest["case"], i))
        out.append(line)
    return out


def drive(requests: list[str]) -> tuple[list[str], str]:
    """Send one transcript to a live process under flow `lockstep`.

    Lines go out in file order; a request waits for its own response before the
    next request is sent, and a notification goes out immediately. The second
    half is the whole reason a cancellation case is recordable at all — waiting
    for the response to the search being cancelled would deadlock.
    """
    proc = subprocess.Popen(
        [sys.executable, ADAPTER],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )
    frames: "queue.Queue[str | None]" = queue.Queue()

    def reader():
        for line in proc.stdout:
            if line.strip():
                frames.put(line.strip())
        frames.put(None)

    thread = threading.Thread(target=reader, daemon=True)
    thread.start()

    got: list[str] = []

    def await_one(timeout: float) -> bool:
        try:
            frame = frames.get(timeout=timeout)
        except queue.Empty:
            raise SystemExit("no response within %ss" % timeout)
        if frame is None:
            raise SystemExit("the adapter closed stdout with a request outstanding")
        got.append(frame)
        return True

    pending = False
    try:
        for line in requests:
            frame = json.loads(line)
            notification = frame.get("method") and frame.get("id") is None
            if pending and not notification:
                await_one(RESPONSE_TIMEOUT)
                pending = False
            proc.stdin.write(line + "\n")
            proc.stdin.flush()
            pending = pending or not notification
        if pending:
            await_one(RESPONSE_TIMEOUT)

        # A clean exit closes stdout. Draining proves nothing was written after
        # the last expected reply, which is how an adapter that answers
        # shutdown and then keeps talking is caught.
        proc.stdin.close()
        while True:
            try:
                frame = frames.get(timeout=DRAIN_TIMEOUT)
            except queue.Empty:
                break
            if frame is None:
                break
            got.append(frame)
    finally:
        try:
            proc.stdin.close()
        except OSError:
            pass
        try:
            proc.wait(timeout=2)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        stderr = proc.stderr.read()
        proc.stderr.close()
        proc.stdout.close()
    return got, stderr


def mask(value, path: list[str]):
    """Blank what a JSON pointer reaches. `*` matches every element or member."""
    if not path:
        return
    head, rest = path[0], path[1:]
    if isinstance(value, dict):
        keys = list(value) if head == "*" else ([head] if head in value else [])
        for key in keys:
            if rest:
                mask(value[key], rest)
            else:
                value[key] = VOLATILE_MARK
    elif isinstance(value, list):
        if head != "*":
            return
        for i, item in enumerate(value):
            if rest:
                mask(item, rest)
            else:
                value[i] = VOLATILE_MARK


def normalize(line: str, volatile: list[str]):
    frame = json.loads(line)
    for pointer in volatile:
        mask(frame, [p for p in pointer.strip("/").split("/") if p])
    return frame


def run_case(case_dir: str, record: bool) -> bool:
    manifest = load_manifest(case_dir)
    name = manifest["case"]
    requests = read_lines(os.path.join(case_dir, "request.jsonl"))
    if not requests:
        raise SystemExit("case %s sends nothing" % name)

    # A fresh, empty workdir per case and per run. Reusing one would let a
    # second replay observe a warm index the recording never had.
    workdir = tempfile.mkdtemp(prefix="recall-notes-%s-" % name)
    try:
        bound = bind(requests, manifest, os.path.join(case_dir, "fixture"), os.path.realpath(workdir))
        got, stderr = drive(bound)
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    expected = manifest.get("responses")
    if expected != len(got):
        print("%-20s FAIL manifest declares %s responses, the run produced %d"
              % (name, expected, len(got)))
        if stderr.strip():
            print(indent(stderr))
        return False

    path = os.path.join(case_dir, "response.jsonl")
    if record:
        with open(path, "w", encoding="utf-8") as handle:
            for line in got:
                handle.write(line + "\n")
        print("%-20s recorded %d responses" % (name, len(got)))
        return True

    want = read_lines(path)
    volatile = manifest.get("volatile") or []
    ok = True
    for i, (w, g) in enumerate(zip(want, got), start=1):
        if normalize(w, volatile) != normalize(g, volatile):
            ok = False
            print("%-20s FAIL response %d differs\n  want: %s\n   got: %s" % (name, i, w, g))
    if ok:
        print("%-20s ok   %d responses, as recorded" % (name, len(got)))
    elif stderr.strip():
        print(indent(stderr))
    return ok


def indent(text: str) -> str:
    return "\n".join("    " + line for line in text.strip().split("\n"))


def main(argv: list[str]) -> int:
    if not argv or argv[0] not in ("record", "verify"):
        print(__doc__.strip(), file=sys.stderr)
        return 2
    record = argv[0] == "record"
    names = argv[1:] or sorted(
        n for n in os.listdir(SUITE) if os.path.isdir(os.path.join(SUITE, n))
    )
    if not names:
        # A suite that quietly lost its cases must not report a clean pass:
        # "verified nothing" may never read as "verified".
        print("no conformance cases found under %s" % SUITE, file=sys.stderr)
        return 1
    failed = [name for name in names if not run_case(os.path.join(SUITE, name), record)]
    if failed:
        print("\n%d of %d cases failed: %s" % (len(failed), len(names), ", ".join(failed)))
        return 1
    print("\n%d cases %s" % (len(names), "recorded" if record else "replayed as recorded"))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
