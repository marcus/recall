#!/usr/bin/env python3
"""recall-notes: a template external adapter for Recall, in dependency-free Python.

It exists to be copied. The source it serves is deliberately dull — a directory
of Markdown notes with a small header block — so that nothing here is about
notes and everything is about the contract: how a version is negotiated, where
an index may be written, what a locator promises, what a partial answer looks
like, and how a source names the store it opened.

Read it top to bottom once; then delete `notes.py`-specific parsing and keep the
frame. `docs/writing-an-adapter.md` in the Recall tree is the prose companion,
and `conformance/` beside this file is the same behavior recorded frame by frame.

Boundary. This adapter owns parsing, its index, ranking within this source, and
locator semantics. It owns no identity: `source_uid`, the source prior, and the
sensitivity floor come from configuration, and the core overwrites the source
part of every locator returned here. It writes only inside the workdir supplied
at the handshake, and only two things there: the published index and the
checkpoint beside it.

Transport. Newline-delimited JSON-RPC 2.0. stdout carries protocol frames and
nothing else; stderr is free-form logging that the core captures into
diagnostics and never parses. One `print()` to stdout in the wrong place breaks
every request that follows it, which is why every write here goes through
`_emit`.

Concurrency. Requests are served on threads, because a single-threaded loop
cannot read `recall/cancel` while the request being cancelled is still running:
the notification would sit unread in the pipe until the work it was meant to
abandon had finished. An adapter that genuinely cannot serve two requests at
once declares `max_concurrency: 1` in its manifest instead of pretending.

Usage:

    recall_notes.py            serve the protocol on stdin/stdout
    recall_notes.py --version  print the adapter identity and exit
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import sqlite3
import sys
import threading
import time
from datetime import datetime, timezone

# --------------------------------------------------------------------------
# Identity and constants
# --------------------------------------------------------------------------

# ADAPTER_ID is this implementation's identity in manifests and reports. It is
# not the source's identity: one adapter serves many configured instances.
ADAPTER_ID = "recall-notes/1"
DISPLAY_NAME = "Notes"

# The protocol version range this build speaks. Negotiation happens once, in
# recall/initialize; a range with no overlap fails the handshake rather than
# degrading to a version neither end implements.
PROTOCOL_MIN = 1
PROTOCOL_MAX = 1

# INDEX_CONFIG identifies the retrieval configuration a generation was built
# under. Change it whenever tokenization or scoring changes. Without it a
# scoring change silently moves ranking with nothing in the generation
# recording it, and an evaluation comparing two generations would credit the
# difference to whatever else was under test.
INDEX_CONFIG = (
    "notes/2 tokenizer=ident-runs scoring=field-weighted-coverage "
    "fingerprint=material-v2 publication=checkpoint-pointer"
)

# Defaults for the settings block below.
DEFAULT_MAX_CANDIDATES = 50
DEFAULT_MAX_NOTES = 5000

# EXCERPT_BYTES bounds a candidate's preview. A candidate is a pointer, not a
# payload; the locator is how a caller gets the rest.
EXCERPT_BYTES = 240

# The checkpoint is the publication pointer. Index generations are immutable
# files whose names bind the sequence number to the full corpus digest.
INDEX_PREFIX = "index-gen-"
CHECKPOINT_FILE = "checkpoint.json"

# Recall error codes, from docs/adapter-protocol.md. The JSON-RPC codes below
# -32000 are the standard ones.
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
INTERNAL_ERROR = -32603
SOURCE_UNAVAILABLE = -32000
SOURCE_DENIED = -32001
LOCATOR_UNKNOWN = -32002
LOCATOR_EXPIRED = -32003
SOURCE_NOT_CONFIGURED = -32004
AS_OF_UNSUPPORTED = -32005
BUDGET_EXCEEDED = -32006
DEADLINE_EXCEEDED = -32007

# The ordered sensitivity scale. A candidate may raise its source's floor and
# may never lower it, so the only operation this adapter performs on it is a
# maximum.
SENSITIVITY_ORDER = ["public", "internal", "confidential", "restricted"]

# The floor this adapter reports when configuration names none. Configuration
# can raise it; nothing here can lower what configuration set.
DEFAULT_SENSITIVITY = "internal"

# Field weights. Ranking is deliberately simple and explainable: term coverage
# over fields of differing authority, with an exact identifier hit promoted as
# a partition rather than as a bonus, mirroring the core's own exact-match
# promotion.
WEIGHT_TITLE = 1.0
WEIGHT_HEADING = 0.8
WEIGHT_TAG = 0.6
WEIGHT_BODY = 0.4


class ProtocolError(Exception):
    """A failure that has a wire code.

    Everything an adapter refuses should raise one of these. An exception that
    reaches the dispatcher without a code is reported as -32603 internal_error,
    which is honest but tells a reader nothing about which contract was hit.
    """

    def __init__(self, code: int, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


# --------------------------------------------------------------------------
# Text hygiene
#
# Retrieved content is data, never instructions, and it is on its way to a
# terminal and to a model. Two functions rather than one: single-line fields
# (title, headings, provenance, every string in metadata) must not be able to
# forge a line, and multi-line evidence must keep its newlines while losing
# everything a terminal would act on.
#
# What is stripped, and why each: C0 and C1 controls carry ANSI colour and
# cursor movement, which a terminal obeys; bidi overrides and isolates reorder
# what a reader sees without changing what a program matched; U+2028 and U+2029
# are line breaks that most whitespace splitters do not recognize, so they slip
# through a naive "collapse the whitespace" pass.
# --------------------------------------------------------------------------

# Written as escapes on purpose. A source file that spelled these characters
# out would carry an invisible bidi override and a line separator that most
# tools do not split on, and would break the next program to read it.
_CONTROL = re.compile(
    "[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f"  # C0 and C1, ESC included
    "\u202a-\u202e\u2066-\u2069]"  # bidi overrides and isolates
)
_LINE_SEPARATORS = ("\u2028", "\u2029")


def safe_text(s: str) -> str:
    """Strip prohibited controls, keeping tabs and normalized newlines.

    CR is normalized explicitly before the C0 pass. Leaving it in a string is
    dangerous even when a terminal happens to render it as a line break: CR
    returns the cursor to column zero and lets later text overwrite what a
    reader just saw.
    """
    s = s.replace("\r\n", "\n").replace("\r", "\n")
    for sep in _LINE_SEPARATORS:
        s = s.replace(sep, "\n")
    return _CONTROL.sub("", s)


def one_line(s: str) -> str:
    """Collapse a value that must occupy exactly one line and forge nothing."""
    return " ".join(_CONTROL.sub("", s).split())


def clip(s: str, limit: int) -> str:
    """Bound a preview in bytes, including the ellipsis that marks a cut."""
    limit = max(0, limit)
    raw = s.encode("utf-8")
    if len(raw) <= limit:
        return s
    marker = "…"
    marker_bytes = marker.encode("utf-8")
    if limit < len(marker_bytes):
        return raw[:limit].decode("utf-8", "ignore")
    out = raw[: limit - len(marker_bytes)].decode("utf-8", "ignore").rstrip()
    return out + marker


def clip_bytes(s: str, limit: int) -> str:
    """Cut at a UTF-8 boundary so a truncated expansion is still text."""
    return s.encode("utf-8")[: max(0, limit)].decode("utf-8", "ignore")


def rfc3339(when: float) -> str:
    """Render an epoch timestamp the way the wire schema requires."""
    return datetime.fromtimestamp(when, timezone.utc).isoformat().replace("+00:00", "Z")


def parse_time(text: str) -> float:
    """Read an RFC 3339 instant. Refusing a bad one is better than guessing."""
    cleaned = text.strip().replace("Z", "+00:00")
    parsed = datetime.fromisoformat(cleaned)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.timestamp()


def max_sensitivity(a: str, b: str) -> str:
    """Return the stricter of two classifications."""
    ia = SENSITIVITY_ORDER.index(a) if a in SENSITIVITY_ORDER else 1
    ib = SENSITIVITY_ORDER.index(b) if b in SENSITIVITY_ORDER else 1
    return SENSITIVITY_ORDER[max(ia, ib)]


# --------------------------------------------------------------------------
# The note format
#
# A note is a header block of `key: value` lines, a blank line, and a body.
# Sections are `## ` headings inside the body; the text before the first
# heading is section 0.
#
# `date:` is mandatory and there is deliberately no fallback to the file's
# mtime. An mtime is a property of this checkout rather than of the record, so
# a corpus indexed on two machines would carry two different event times for
# one note, and a recorded conformance transcript would stop matching the
# moment someone cloned the repository.
# --------------------------------------------------------------------------

_ID_SAFE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
_LOCAL_PART = re.compile(r"^([A-Za-z0-9][A-Za-z0-9._-]*)#([0-9]+)$")
_TOKEN_SPLIT = re.compile(r"[^0-9a-z\-_./:]+")


def tokenize(text: str) -> list[str]:
    """Split on anything that cannot appear inside an identifier.

    Identifier punctuation stays inside the token, so "td-7f7640" is one token
    and can be compared for equality against a record id. A tokenizer that
    split on "-" would make exact identifier matching impossible for every
    system whose ids contain one, which is most of them.
    """
    out = []
    for raw in _TOKEN_SPLIT.split(text.lower()):
        token = raw.strip("-_./:")
        if token:
            out.append(token)
    return out


class ParseError(Exception):
    """A note that could not be read. Counted, never silently skipped."""


def parse_note(path: str, text: str) -> dict:
    """Turn one file into a record, or refuse it with a reason."""
    header: dict[str, str] = {}
    lines = text.splitlines()
    cut = 0
    for i, line in enumerate(lines):
        if not line.strip():
            cut = i + 1
            break
        key, sep, value = line.partition(":")
        if not sep:
            raise ParseError("header line %d is not `key: value`" % (i + 1))
        header[key.strip().lower()] = value.strip()
        cut = i + 1
    body = "\n".join(lines[cut:]).strip()

    note_id = header.get("id") or os.path.splitext(os.path.basename(path))[0]
    if not _ID_SAFE.match(note_id):
        raise ParseError("id %r is not usable in a locator" % note_id)
    if "date" not in header:
        raise ParseError("no `date:` header, and an mtime is not an event time")
    try:
        event_epoch = parse_time(header["date"])
    except ValueError as exc:
        raise ParseError("date %r is not an RFC 3339 instant" % header["date"]) from exc

    sensitivity = header.get("sensitivity", DEFAULT_SENSITIVITY)
    if sensitivity not in SENSITIVITY_ORDER:
        raise ParseError("sensitivity %r is not on the scale" % sensitivity)

    tags = [t.strip().lower() for t in header.get("tags", "").split(",") if t.strip()]
    title = one_line(header.get("title") or note_id)

    # Aliases are the only thing besides the note's own id that this adapter
    # will treat as an exact identifier. Tags are deliberately NOT aliases: a
    # tag is a topical label, so promoting on one would make the query "policy"
    # an exact hit on every note anyone ever tagged `policy`, and
    # exact_identifier would stop meaning "you named this record".
    aliases = [a.strip().lower() for a in header.get("aliases", "").split(",") if a.strip()]
    derived_from = [
        one_line(locator)
        for locator in header.get("derived_from", "").split(",")
        if locator.strip()
    ]
    for locator in derived_from:
        if ":" not in locator or locator.startswith(":") or locator.endswith(":"):
            raise ParseError("derived_from %r is not <source_id>:<local>" % locator)

    return {
        "id": note_id,
        "file": os.path.basename(path),
        "title": title,
        "tags": tags,
        "aliases": aliases,
        "derived_from": derived_from,
        "sensitivity": sensitivity,
        "event_epoch": event_epoch,
        "event_time": rfc3339(event_epoch),
        "sections": split_sections(body),
    }


def split_sections(body: str) -> list[dict]:
    """Split a body into `## ` sections, keeping the lead text as section 0.

    Sections are why `candidate_id` and `source_record_id` differ here. Several
    sections of one note are several candidates and one record: corroboration
    collapses on `source_record_id`, so a per-section value would let this one
    source corroborate itself and promote a note for agreeing with itself.
    """
    sections: list[dict] = []
    heading = ""
    buffer: list[str] = []

    def flush():
        text = "\n".join(buffer).strip()
        if text or heading or not sections:
            sections.append({"heading": one_line(heading), "text": safe_text(text)})

    for line in body.splitlines():
        if line.startswith("## "):
            flush()
            heading = line[3:].strip()
            buffer = []
            continue
        buffer.append(line)
    flush()
    return sections


def fingerprint(record: dict, ordinal: int, section: dict) -> str:
    """A normalized content hash for one section.

    It is advisory and it is what makes a duplicate configuration harmless
    until an operator corrects it: two instances that reached the same note
    through different configuration produce the same fingerprint and collapse
    into one piece of evidence. It is deliberately built from the record's own
    identity and content and from nothing about where this instance found it —
    the location, the watermark, and the generation are precisely what two
    instances over one store disagree about, and a fingerprint built on any of
    them would differ for the same note and defeat itself.
    """
    material = {
        "aliases": sorted(record["aliases"]),
        "body": section["text"],
        "candidate_ordinal": ordinal,
        "date": record["event_time"],
        "derived_from": sorted(record["derived_from"]),
        "file": record["file"],
        "heading": section["heading"],
        "note_id": record["id"],
        "record_sections": record["sections"],
        "sections": len(record["sections"]),
        "sensitivity": record["sensitivity"],
        "tags": sorted(record["tags"]),
        "title": record["title"],
    }
    encoded = json.dumps(
        material, ensure_ascii=False, separators=(",", ":"), sort_keys=True
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


# --------------------------------------------------------------------------
# Settings
# --------------------------------------------------------------------------


def settings_schema() -> dict:
    """Declare the settings block.

    `recall doctor` validates a configuration against this without starting a
    query, so every key here must be one the code below actually reads. A
    declared key with no code path is configuration that appears to work and
    does nothing, and unknown keys are rejected rather than ignored so that a
    misspelling fails loudly at the handshake.
    """
    return {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "notes_dir": {
                "type": "string",
                "description": (
                    "Directory of notes, relative to the source's location. "
                    "Empty means the location itself. A value that resolves outside "
                    "the location is refused."
                ),
            },
            "max_notes": {
                "type": "integer",
                "minimum": 1,
                "description": (
                    "Cap on how many note files one scan reads. Hitting it makes the "
                    "index partial, and the partial coverage is reported rather than "
                    "letting a short list look like a complete corpus."
                ),
            },
            "max_candidates": {
                "type": "integer",
                "minimum": 1,
                "description": "Cap on candidates returned by one search.",
            },
            "debug_stall_ms": {
                "type": "integer",
                "minimum": 0,
                "description": (
                    "Artificial pre-scan delay, for recording the cancellation "
                    "conformance case. Leave unset in real configuration."
                ),
            },
        },
    }


def parse_settings(raw: dict | None) -> dict:
    """Decode and validate the settings block."""
    raw = raw or {}
    known = set(settings_schema()["properties"])
    unknown = sorted(set(raw) - known)
    if unknown:
        raise ProtocolError(
            INVALID_PARAMS,
            "notes settings: unknown key(s) %s; a misspelled setting that silently "
            "did nothing would be configuration with no code path behind it"
            % ", ".join(unknown),
        )
    out = {
        "notes_dir": raw.get("notes_dir", ""),
        "max_notes": int(raw.get("max_notes", DEFAULT_MAX_NOTES)),
        "max_candidates": int(raw.get("max_candidates", DEFAULT_MAX_CANDIDATES)),
        "debug_stall_ms": int(raw.get("debug_stall_ms", 0)),
    }
    if not isinstance(out["notes_dir"], str):
        raise ProtocolError(INVALID_PARAMS, "notes settings: notes_dir must be a string")
    for key in ("max_notes", "max_candidates"):
        if out[key] < 1:
            raise ProtocolError(INVALID_PARAMS, "notes settings: %s must be positive" % key)
    if out["debug_stall_ms"] < 0:
        raise ProtocolError(INVALID_PARAMS, "notes settings: debug_stall_ms cannot be negative")
    return out


def resolve_notes_dir(location: str, name: str) -> str:
    """Turn a location plus a configured sub-path into one absolute directory.

    Settings are adapter-owned and are not validated when configuration loads,
    so this is the layer that has to refuse "../../.ssh" — nothing above it
    will. The check is on the resolved path rather than on the text, so a
    symlink out of the location is caught too.
    """
    location = (location or "").strip()
    if not location:
        raise ProtocolError(
            INVALID_PARAMS,
            "notes: this source has no location, so there is no corpus to read",
        )
    base = os.path.realpath(location)
    if not name:
        return base
    if os.path.isabs(name):
        raise ProtocolError(
            INVALID_PARAMS,
            "notes settings: notes_dir %r must be relative to the location" % name,
        )
    full = os.path.realpath(os.path.join(base, name))
    if full != base and not full.startswith(base + os.sep):
        raise ProtocolError(
            INVALID_PARAMS,
            "notes settings: notes_dir %r resolves outside the configured location" % name,
        )
    return full


def store_identity(notes_dir: str) -> str:
    """Name the store THIS INSTANCE opened, for `recall doctor`'s isolation check.

    Two enabled instances of one adapter reporting the same value is a
    configuration error no single source can see: lineage groups on source_uid
    plus source_record_id, so one note reaching the core through two instances
    arrives as two independent pieces of evidence and collects the corroboration
    bonus for agreeing with itself.

    Three properties make the value worth publishing:

    - It names what was OPENED. `notes_dir` here is a realpath, so a source
      configured at a directory and another at a symlink to it compare equal.
      A value copied from the configured text would compare equal exactly when
      the configuration was already consistent, which is the case that was
      never in doubt.
    - Setting it CLAIMS EXCLUSIVITY: this store is mine alone. An adapter for
      which two instances over one store is a legitimate configuration leaves
      it unset, and absent means "makes no such claim", never "unknown".
    - It is hashed. The check compares for equality within one adapter and
      never reads the value, so a digest serves it exactly as well as the path
      — and a digest cannot leak a home directory into a diagnostic, a log, or
      a committed conformance transcript.
    """
    return "dir:" + hashlib.sha256(notes_dir.encode("utf-8")).hexdigest()[:16]


# --------------------------------------------------------------------------
# The index
#
# The index is a rebuildable projection and never the source of truth. Each
# generation is built into a new immutable file and made durable. The checkpoint
# is then atomically replaced as the sole publication pointer, so failed builds,
# failed checkpoints, and crashes all leave the previous generation readable.
# --------------------------------------------------------------------------

SCHEMA = """
CREATE TABLE section (
  candidate_id TEXT PRIMARY KEY,
  note_id      TEXT NOT NULL,
  ordinal      INTEGER NOT NULL,
  sections     INTEGER NOT NULL,
  file         TEXT NOT NULL,
  title        TEXT NOT NULL,
  heading      TEXT NOT NULL,
  body         TEXT NOT NULL,
  tags         TEXT NOT NULL,
  event_epoch  REAL NOT NULL,
  event_time   TEXT NOT NULL,
  sensitivity  TEXT NOT NULL,
  derived_from TEXT NOT NULL,
  fingerprint  TEXT NOT NULL
);
CREATE TABLE term (
  term         TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  weight       REAL NOT NULL
);
CREATE INDEX term_lookup ON term(term);
CREATE TABLE ident (
  ident        TEXT NOT NULL,
  candidate_id TEXT NOT NULL
);
CREATE INDEX ident_lookup ON ident(ident);
"""


class Snapshot:
    """One published generation: what it holds, and how complete it is."""

    def __init__(
        self,
        generation,
        built_at,
        notes,
        sections,
        failed,
        truncated,
        unreadable,
        digest,
        latest,
        signature,
        index_file,
    ):
        self.generation = generation
        self.built_at = built_at
        self.notes = notes
        self.sections = sections
        self.failed = failed
        self.truncated = truncated
        self.unreadable = unreadable
        self.digest = digest
        self.latest = latest
        self.signature = signature
        self.index_file = index_file

    @property
    def coverage(self) -> str:
        """complete only when every note in the directory reached the index."""
        if self.failed or self.truncated or self.unreadable:
            return "partial"
        return "complete"

    def watermark(self) -> str:
        """Freshness evidence a caller can compare across two searches.

        Everything in it comes from the corpus — how many notes, the newest
        event time, and a digest over each note's identity and content. Nothing
        in it comes from this machine: a watermark carrying an mtime or a path
        would differ between two hosts indexing the same corpus, which is
        exactly when a caller most needs to see that they agree.
        """
        latest = rfc3339(self.latest) if self.latest else "none"
        return "notes=%d sections=%d latest=%s digest=%s" % (
            self.notes,
            self.sections,
            latest,
            self.digest,
        )

    def generation_id(self) -> str:
        # The digest is part of the identity. If a process dies after staging
        # generation N+1 but before publishing its checkpoint, a restart may
        # choose N+1 again; changed content still cannot reuse the old identity.
        return "gen-%d-%s" % (self.generation, self.digest)


def scan(notes_dir: str, max_notes: int) -> tuple[list[dict], list[str], bool, str]:
    """Read the corpus. Returns records, the files that failed, and truncation.

    A directory that cannot be listed is source_unavailable and never an empty
    corpus: "the source could not be reached" and "the source holds nothing"
    are different answers, and a caller that cannot tell them apart will read
    the first as proof of absence.
    """
    try:
        names = sorted(
            n for n in os.listdir(notes_dir) if n.endswith(".md") and not n.startswith(".")
        )
    except OSError as exc:
        raise ProtocolError(
            SOURCE_UNAVAILABLE,
            # The base name only: diagnostics carry no local paths.
            "notes directory %r cannot be listed: %s"
            % (os.path.basename(notes_dir), exc.strerror or "unreadable"),
        ) from exc

    truncated = len(names) > max_notes
    records: list[dict] = []
    unreadable: list[str] = []
    source_digest = hashlib.sha256()
    for name in names:
        source_digest.update(b"name\x00" + name.encode("utf-8") + b"\x00")
    for name in names[:max_notes]:
        path = os.path.join(notes_dir, name)
        try:
            with open(path, "rb") as handle:
                raw = handle.read()
            source_digest.update(b"content\x00" + raw + b"\x00")
            text = raw.decode("utf-8")
            records.append(parse_note(path, text))
        except (OSError, UnicodeDecodeError, ParseError) as exc:
            # One bad note must not take the corpus down, and it must not
            # disappear either. It is counted, named by base name, and the
            # coverage that results says the index is partial.
            log("skipping %s: %s" % (name, exc))
            unreadable.append(name)
    return records, unreadable, truncated, source_digest.hexdigest()


def build(workdir: str, notes_dir: str, generation: int, max_notes: int) -> Snapshot:
    """Build one durable immutable generation, not yet published."""
    records, unreadable, truncated, source_digest = scan(notes_dir, max_notes)

    tmp = os.path.join(workdir, "build-%d.sqlite" % generation)
    if os.path.exists(tmp):
        os.remove(tmp)
    conn = sqlite3.connect(tmp)
    try:
        conn.executescript(SCHEMA)
        rows, terms, idents = [], [], []
        latest = 0.0
        sections = 0
        for record in sorted(records, key=lambda r: r["id"]):
            latest = max(latest, record["event_epoch"])
            for ordinal, section in enumerate(record["sections"]):
                candidate_id = "%s#%d" % (record["id"], ordinal)
                print_fp = fingerprint(record, ordinal, section)
                sections += 1
                rows.append(
                    (
                        candidate_id,
                        record["id"],
                        ordinal,
                        len(record["sections"]),
                        record["file"],
                        record["title"],
                        section["heading"],
                        section["text"],
                        ",".join(record["tags"]),
                        record["event_epoch"],
                        record["event_time"],
                        record["sensitivity"],
                        json.dumps(record["derived_from"], separators=(",", ":")),
                        print_fp,
                    )
                )
                for term, weight in weigh(record, section).items():
                    terms.append((term, candidate_id, weight))
                for ident in {record["id"].lower(), *record["aliases"]}:
                    idents.append((ident, candidate_id))

        conn.executemany(
            "INSERT INTO section VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", rows
        )
        conn.executemany("INSERT INTO term VALUES (?,?,?)", terms)
        conn.executemany("INSERT INTO ident VALUES (?,?)", idents)
        conn.commit()
    finally:
        conn.close()

    # Make the SQLite bytes durable before the checkpoint is allowed to name
    # them. The final file is immutable and still private at this point: the
    # checkpoint below is the publication pointer.
    with open(tmp, "rb") as handle:
        os.fsync(handle.fileno())
    index_file = "%s%d-%s.sqlite" % (INDEX_PREFIX, generation, source_digest)
    os.replace(tmp, os.path.join(workdir, index_file))
    fsync_directory(workdir)

    return Snapshot(
        generation=generation,
        built_at=time.time(),
        notes=len(records),
        sections=sections,
        failed=len(unreadable),
        truncated=truncated,
        unreadable=unreadable,
        digest=source_digest,
        latest=latest,
        signature=corpus_signature(notes_dir, max_notes),
        index_file=index_file,
    )


def weigh(record: dict, section: dict) -> dict[str, float]:
    """Score one section's terms by the most authoritative field they appear in."""
    weights: dict[str, float] = {}

    def add(text: str, weight: float):
        for token in tokenize(text):
            if weights.get(token, 0.0) < weight:
                weights[token] = weight

    add(record["title"], WEIGHT_TITLE)
    add(section["heading"], WEIGHT_HEADING)
    add(" ".join(record["tags"] + record["aliases"]), WEIGHT_TAG)
    add(section["text"], WEIGHT_BODY)
    return weights


def load_checkpoint(workdir: str) -> dict:
    """Read the last published boundary, or nothing.

    It is not a resume point for records — this index is rebuilt from the
    corpus — it is what makes generation numbers monotonic across restarts, so
    one id never names two different builds of one workdir.
    """
    try:
        with open(os.path.join(workdir, CHECKPOINT_FILE), "r", encoding="utf-8") as handle:
            data = json.load(handle)
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def snapshot_from_checkpoint(workdir: str, data: dict) -> Snapshot | None:
    """Reopen only a checkpoint that names its exact immutable generation."""
    try:
        generation = int(data["generation"])
        digest = str(data["digest"])
        index_file = str(data["index_file"])
        expected = "%s%d-%s.sqlite" % (INDEX_PREFIX, generation, digest)
        if index_file != expected or os.path.basename(index_file) != index_file:
            return None
        if not os.path.isfile(os.path.join(workdir, index_file)):
            return None
        return Snapshot(
            generation=generation,
            built_at=parse_time(data["built_at"]),
            notes=int(data["notes"]),
            sections=int(data["sections"]),
            failed=int(data["failed"]),
            truncated=bool(data["truncated"]),
            unreadable=list(data["unreadable"]),
            digest=digest,
            latest=float(data["latest"]),
            signature=str(data["signature"]),
            index_file=index_file,
        )
    except (KeyError, TypeError, ValueError):
        return None


def fsync_directory(path: str):
    """Durably record renames on platforms that permit directory fsync."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def save_checkpoint(workdir: str, snap: Snapshot) -> bool:
    """Atomically publish a generation after both files are durable."""
    payload = {
        "generation": snap.generation,
        "built_at": rfc3339(snap.built_at),
        "watermark": snap.watermark(),
        "notes": snap.notes,
        "sections": snap.sections,
        "failed": snap.failed,
        "truncated": snap.truncated,
        "unreadable": snap.unreadable,
        "coverage": snap.coverage,
        "digest": snap.digest,
        "latest": snap.latest,
        "signature": snap.signature,
        "index_file": snap.index_file,
    }
    path = os.path.join(workdir, CHECKPOINT_FILE)
    temporary = path + ".tmp"
    try:
        with open(temporary, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        fsync_directory(workdir)
        return True
    except OSError as exc:
        # The staged immutable index is not published without this pointer.
        log("checkpoint unwritable: %s" % exc)
        try:
            os.remove(temporary)
        except OSError:
            pass
        return False


def corpus_signature(notes_dir: str, max_notes: int) -> str:
    """A cheap fingerprint of the directory, for deciding whether to rebuild.

    Sizes and mtimes are fine to read: they never leave this function. They are
    not fine to publish, which is why the watermark is built from note content
    instead.
    """
    try:
        names = sorted(n for n in os.listdir(notes_dir) if n.endswith(".md"))
    except OSError:
        return "unreadable"
    digest = hashlib.sha256()
    for name in names[:max_notes]:
        try:
            st = os.stat(os.path.join(notes_dir, name))
        except OSError:
            digest.update(b"missing:" + name.encode("utf-8"))
            continue
        digest.update(("%s:%d:%d\n" % (name, st.st_size, st.st_mtime_ns)).encode("utf-8"))
    return digest.hexdigest()


# --------------------------------------------------------------------------
# The adapter
# --------------------------------------------------------------------------


class Cancellation:
    """One request's deadline and its cancel flag.

    This is the whole of what an adapter owes a cancellation: notice, return,
    and do not answer. A late result is worse than an error, because the core
    has already told its caller the source did not answer.
    """

    def __init__(self, deadline: float):
        self.deadline = deadline
        self.event = threading.Event()

    def cancel(self):
        self.event.set()

    def check(self):
        if self.event.is_set():
            raise ProtocolError(DEADLINE_EXCEEDED, "request cancelled")
        if self.deadline and time.time() > self.deadline:
            raise ProtocolError(DEADLINE_EXCEEDED, "deadline exceeded")

    def sleep(self, seconds: float):
        """Wait, returning the moment the request is abandoned."""
        if self.event.wait(seconds):
            raise ProtocolError(DEADLINE_EXCEEDED, "request cancelled")
        self.check()


class Adapter:
    """The six handlers, plus the state one handshake establishes."""

    def __init__(self):
        self.lock = threading.RLock()
        self.build_lock = threading.Lock()
        self.ready = False
        self.closed = False
        self.source_id = ""
        self.workdir = ""
        self.notes_dir = ""
        self.settings: dict = {}
        self.floor = DEFAULT_SENSITIVITY
        self.generation = 0
        self.snapshot: Snapshot | None = None
        self.signature = ""
        self.checkpoint_ok = True
        self.refresh_failure = ""

    # -- handshake ---------------------------------------------------------

    def initialize(self, params: dict, _cancel: Cancellation) -> dict:
        """Negotiate the version, validate settings, adopt the workdir.

        No corpus bytes are read here. Building an index inside the handshake
        competes with the core's handshake timeout on any real corpus, which is
        the reason recall/refresh exists.
        """
        want_min = int(params.get("protocol_version_min", 0))
        want_max = int(params.get("protocol_version_max", 0))
        version = min(PROTOCOL_MAX, want_max)
        if version < want_min or version < PROTOCOL_MIN:
            # Failing is the contract. Degrading to a version neither end
            # implements is exactly what a handshake exists to prevent.
            raise ProtocolError(
                INVALID_PARAMS,
                "protocol version %d is outside the requested range %d..%d"
                % (PROTOCOL_MAX, want_min, want_max),
            )

        settings = parse_settings(params.get("settings"))
        notes_dir = resolve_notes_dir(params.get("location", ""), settings["notes_dir"])
        workdir = (params.get("workdir") or "").strip()
        if not workdir:
            raise ProtocolError(
                INVALID_PARAMS,
                "notes: the handshake supplied no workdir, and this adapter has "
                "nowhere else it may write",
            )

        prior = snapshot_from_checkpoint(workdir, load_checkpoint(workdir))
        with self.lock:
            if self.closed:
                raise ProtocolError(SOURCE_UNAVAILABLE, "adapter is closed")
            self.source_id = params.get("source_id", "")
            self.workdir = workdir
            self.notes_dir = notes_dir
            self.settings = settings
            # Only the durable checkpoint publishes a generation. A staged
            # immutable index with no checkpoint is intentionally invisible.
            self.generation = prior.generation if prior is not None else 0
            self.snapshot = prior
            self.signature = prior.signature if prior is not None else ""
            self.checkpoint_ok = True
            self.refresh_failure = ""
            self.ready = True

        return {
            "protocol_version": version,
            "adapter_id": ADAPTER_ID,
            "display_name": DISPLAY_NAME,
            "record_types": ["document"],
            "query_modes": ["exact", "lexical", "temporal"],
            "freshness_modes": ["indexed"],
            # Every note carries an immutable `date:`, so restricting to notes
            # written at or before a boundary is a filter over history the
            # source already stores. Snapshot would be a lie: this corpus keeps
            # no revision history, so the set of notes present at a past instant
            # cannot be reconstructed from it.
            "as_of_support": "filter",
            "capabilities": ["search", "expand", "checkpoint"],
            "freshness_policy": (
                "indexed: the projection is rebuilt when the note directory changes "
                "and on recall/refresh; a note that fails to parse is counted, and "
                "coverage reports partial until it is fixed"
            ),
            "sensitivity": DEFAULT_SENSITIVITY,
            "settings_schema": settings_schema(),
        }

    def session(self) -> tuple[dict, str, str]:
        with self.lock:
            if self.closed:
                raise ProtocolError(SOURCE_UNAVAILABLE, "adapter is closed")
            if not self.ready:
                raise ProtocolError(
                    SOURCE_UNAVAILABLE, "notes adapter has not completed a handshake"
                )
            return dict(self.settings), self.source_id, self.floor

    # -- the projection ----------------------------------------------------

    def current(self, full: bool = False) -> Snapshot:
        """Return a generation that has consumed the corpus as it stands now."""
        settings, _, _ = self.session()
        with self.build_lock:
            with self.lock:
                previous, notes_dir, workdir = self.snapshot, self.notes_dir, self.workdir
                generation, signature = self.generation, self.signature
            fresh = corpus_signature(notes_dir, settings["max_notes"])
            if previous is not None and not full and fresh == signature:
                return previous

            try:
                snap = build(workdir, notes_dir, generation + 1, settings["max_notes"])
            except ProtocolError as exc:
                with self.lock:
                    self.refresh_failure = exc.message
                if previous is not None:
                    return previous
                raise
            except (OSError, sqlite3.Error) as exc:
                message = "generation build failed: %s" % exc
                with self.lock:
                    self.refresh_failure = message
                if previous is not None:
                    return previous
                raise ProtocolError(SOURCE_UNAVAILABLE, message) from exc
            if not save_checkpoint(workdir, snap):
                message = "checkpoint publication failed; previous generation retained"
                with self.lock:
                    self.checkpoint_ok = False
                    self.refresh_failure = message
                if previous is not None:
                    return previous
                raise ProtocolError(SOURCE_UNAVAILABLE, message)
            with self.lock:
                self.snapshot = snap
                self.generation = snap.generation
                self.signature = snap.signature
                self.checkpoint_ok = True
                self.refresh_failure = ""
            return snap

    # -- health ------------------------------------------------------------

    def health(self, _params: dict, _cancel: Cancellation) -> dict:
        try:
            snap = self.current()
        except ProtocolError as exc:
            return self.unhealthy(exc)
        return self.health_of(snap)

    def refresh(self, params: dict, _cancel: Cancellation) -> dict:
        """Bring the projection up to date and report the resulting health.

        This is what the `checkpoint` capability means. A build that fails is
        reported through the returned health — stale watermark, degraded status,
        the reason in diagnostics — and not as an error: a frame carries a
        result or an error and never both, so erroring would discard the health
        of the generation that is still published and still answering.
        """
        try:
            snap = self.current(full=bool(params.get("full")))
        except ProtocolError as exc:
            return self.unhealthy(exc)
        return self.health_of(snap)

    def health_of(self, snap: Snapshot) -> dict:
        with self.lock:
            checkpoint_ok = self.checkpoint_ok
            refresh_failure = self.refresh_failure
            notes_dir = self.notes_dir
        diagnostics = {
            "notes_dir": os.path.basename(notes_dir),
            "store_identity": store_identity(notes_dir),
        }
        status = "healthy"
        if snap.unreadable:
            diagnostics["unreadable"] = snap.unreadable
        if snap.truncated:
            diagnostics["listing_truncated"] = True
        if not checkpoint_ok:
            diagnostics["checkpoint_unwritable"] = True
            status = "degraded"
        if refresh_failure:
            diagnostics["refresh_failure"] = one_line(refresh_failure)
            status = "degraded"
        if snap.coverage != "complete":
            # Partial coverage is degraded, not healthy. No freshness policy
            # here declares this particular partial boundary acceptable, and a
            # recent index timestamp alone is not health.
            status = "degraded"
        return {
            "status": status,
            "checked_at": rfc3339(time.time()),
            "last_success_at": rfc3339(snap.built_at),
            "source_watermark": snap.watermark(),
            "index_watermark": snap.watermark(),
            "index_generation": snap.generation_id(),
            "index_config": INDEX_CONFIG,
            "record_count": snap.notes + snap.failed,
            "indexed_count": snap.notes,
            "failed_count": snap.failed,
            "coverage": snap.coverage,
            "diagnostics": diagnostics,
        }

    def unhealthy(self, exc: ProtocolError) -> dict:
        """Render a failed probe. An unreachable source is never healthy and
        never has a known coverage."""
        status = "denied" if exc.code == SOURCE_DENIED else "unavailable"
        report = {
            "status": status,
            "checked_at": rfc3339(time.time()),
            "coverage": "unknown",
            "diagnostics": {"reason": exc.message},
        }
        with self.lock:
            snap = self.snapshot
        if snap is not None:
            # The generation already published is the one still answering, so a
            # failed probe reports it rather than erasing it.
            report["index_generation"] = snap.generation_id()
            report["index_watermark"] = snap.watermark()
            report["indexed_count"] = snap.notes
            report["last_success_at"] = rfc3339(snap.built_at)
        return report

    # -- search ------------------------------------------------------------

    def search(self, params: dict, cancel: Cancellation) -> dict:
        settings, source_id, floor = self.session()
        started = time.time()
        if settings["debug_stall_ms"]:
            cancel.sleep(settings["debug_stall_ms"] / 1000.0)
        cancel.check()

        snap = self.current()
        cancel.check()

        terms = tokenize(params.get("query", ""))
        filters = params.get("filters") or {}

        # Filters this adapter cannot evaluate. It has no entity extraction and
        # no notion of a project, so it can neither apply them nor prove that a
        # note satisfies them. Applying them by guessing would invent matches;
        # dropping every candidate would manufacture an absence. What is left is
        # to answer the broader question and say plainly that this is what
        # happened, which is what `partial` plus a named diagnostic means.
        unapplied = [
            name
            for name in ("entities", "project")
            if filters.get(name)
        ]

        where, args = [], []
        if params.get("as_of"):
            where.append("s.event_epoch <= ?")
            args.append(parse_time(params["as_of"]))
        if filters.get("since"):
            where.append("s.event_epoch >= ?")
            args.append(parse_time(filters["since"]))
        if filters.get("until"):
            where.append("s.event_epoch <= ?")
            args.append(parse_time(filters["until"]))
        types = filters.get("record_types") or []
        if types and "document" not in types:
            # A filter this adapter CAN apply, and applying it to zero matches
            # is a success with no candidates rather than a partial answer.
            where.append("0")

        rows, scores, exact = self.query(snap, terms, where, args)
        cancel.check()

        hits = []
        for row in rows:
            candidate_id = row["candidate_id"]
            hits.append(
                {
                    "row": row,
                    "score": scores.get(candidate_id, 0.0),
                    "exact": candidate_id in exact,
                }
            )
        matched = len(hits)

        # Exact identifier hits are a partition, not a bonus; then score, then
        # newest first, then id, so the order is total and reproducible.
        hits.sort(
            key=lambda h: (
                not h["exact"],
                -h["score"],
                -h["row"]["event_epoch"],
                h["row"]["candidate_id"],
            )
        )
        limit = settings["max_candidates"]
        if params.get("limit"):
            limit = min(limit, int(params["limit"]))
        hits = hits[:limit]

        candidates = [
            self.candidate(hit, rank, source_id, floor, snap, len(terms))
            for rank, hit in enumerate(hits, start=1)
        ]

        outcome = "success"
        if snap.coverage != "complete" or unapplied:
            # Stated, never implied by a short list. A note that failed to parse
            # is unknown, not absent, and a filter that was not applied means
            # this list answers a broader question than the one asked.
            outcome = "partial"

        diagnostics = {
            "query_mode": "exact" if exact else ("temporal" if not terms else "lexical"),
            "terms": len(terms),
            # Sections, not notes: this is what the search actually looked at,
            # and health's indexed_count is notes. Two different populations
            # deserve two different names, or a reader comparing them across
            # two reports concludes the index lost records.
            "indexed_sections": snap.sections,
            # Counted before the limit was applied, so a caller comparing it
            # with the list can see that a cap cut the tail.
            "matched": matched,
            "coverage": snap.coverage,
            "failed_notes": snap.failed,
            "generation": snap.generation_id(),
            "elapsed_ms": int((time.time() - started) * 1000),
        }
        if snap.unreadable:
            diagnostics["unreadable"] = snap.unreadable
        if snap.truncated:
            diagnostics["listing_truncated"] = True
        if unapplied:
            diagnostics["unapplied_filters"] = unapplied
        return {
            "candidates": candidates,
            "diagnostics": diagnostics,
            "source_watermark": snap.watermark(),
            "outcome": outcome,
        }

    def query(self, snap: Snapshot, terms, where, args):
        """Run one search against the published index, read-only."""
        conn = self.open_index(snap)
        try:
            conn.row_factory = sqlite3.Row
            clause = (" AND " + " AND ".join(where)) if where else ""
            if not terms:
                # A query with no terms is a time-window browse, which is a real
                # question to ask a note corpus: what was written in this span.
                rows = conn.execute(
                    "SELECT * FROM section s WHERE 1" + clause + " ORDER BY s.candidate_id",
                    args,
                ).fetchall()
                return rows, {}, set()

            marks = ",".join("?" * len(terms))
            scored = conn.execute(
                "SELECT t.candidate_id AS candidate_id, SUM(t.weight) AS score "
                "FROM term t JOIN section s ON s.candidate_id = t.candidate_id "
                "WHERE t.term IN (" + marks + ")" + clause + " GROUP BY t.candidate_id",
                list(terms) + args,
            ).fetchall()
            scores = {r["candidate_id"]: r["score"] / len(terms) for r in scored}

            exact = {
                r["candidate_id"]
                for r in conn.execute(
                    "SELECT i.candidate_id AS candidate_id FROM ident i "
                    "JOIN section s ON s.candidate_id = i.candidate_id "
                    "WHERE i.ident IN (" + marks + ")" + clause,
                    list(terms) + args,
                ).fetchall()
            }
            ids = sorted(set(scores) | exact)
            if not ids:
                return [], scores, exact
            rows = conn.execute(
                "SELECT * FROM section s WHERE s.candidate_id IN ("
                + ",".join("?" * len(ids))
                + ")",
                ids,
            ).fetchall()
            return rows, scores, exact
        finally:
            conn.close()

    def open_index(self, snap: Snapshot) -> sqlite3.Connection:
        """Open the published generation read-only.

        Read-only is the default the spec asks for, and it is also what makes a
        search safe to run while a refresh is building: the builder writes a
        different file and publishes by rename, so a reader is never looking at
        a database being mutated underneath it.
        """
        with self.lock:
            path = os.path.join(self.workdir, snap.index_file)
        try:
            return sqlite3.connect("file:%s?mode=ro" % path, uri=True)
        except sqlite3.Error as exc:
            raise ProtocolError(SOURCE_UNAVAILABLE, "index is unreadable: %s" % exc) from exc

    def candidate(self, hit, rank, source_id, floor, snap, term_count) -> dict:
        """Render one section for fusion.

        The locator, the lineage-relevant identity, and the timestamps are the
        whole of what this source contributes. `source_uid` is left out
        entirely: identity is stamped by the core, and an adapter that named
        its own would be claiming something configuration did not give it.
        """
        row = hit["row"]
        if hit["exact"]:
            signals = ["exact_identifier"]
        elif term_count == 0:
            signals = ["field"]  # the time window selected these, not the text
        else:
            signals = ["lexical"]

        preview = row["body"] or row["heading"]
        metadata = {
            # A base name, never a path. Metadata reaches a terminal and a
            # model, and neither needs this machine's directory layout.
            "file": one_line(row["file"]),
            "section": row["ordinal"],
            "sections": row["sections"],
            "tags": [one_line(t) for t in row["tags"].split(",") if t],
        }
        if row["heading"]:
            # Omitted rather than sent empty. A key that is always present and
            # sometimes meaningless teaches a consumer to ignore it.
            metadata["heading"] = one_line(row["heading"])
        candidate = {
            "candidate_id": row["candidate_id"],
            # One note is one record however many sections it has. Corroboration
            # collapses on this value.
            "source_record_id": row["note_id"],
            "locator": "%s:%s" % (source_id, row["candidate_id"]),
            "record_type": "document",
            "title": one_line(
                row["title"] if row["ordinal"] == 0 else "%s — %s" % (row["title"], row["heading"])
            ),
            "excerpt": clip(one_line(preview), EXCERPT_BYTES),
            "local_rank": rank,
            "local_score": round(hit["score"], 6),
            "match_signals": signals,
            # observed_at is when this index read the note.
            "observed_at": rfc3339(snap.built_at),
            "event_time": row["event_time"],
            "source_revision": snap.generation_id(),
            # May raise the source's floor, never lower it.
            "sensitivity": max_sensitivity(floor, row["sensitivity"]),
            "metadata": metadata,
            "content_fingerprint": row["fingerprint"],
        }
        derived_from = json.loads(row["derived_from"])
        if derived_from:
            candidate["derived_from"] = [one_line(locator) for locator in derived_from]
        # A partial pass observed this record, but it did not confirm the whole
        # source boundary. Claiming confirmed_at there would turn an incomplete
        # snapshot into false absence evidence for everything it missed.
        if snap.coverage == "complete":
            candidate["confirmed_at"] = rfc3339(snap.built_at)
        return candidate

    # -- expand ------------------------------------------------------------

    def expand(self, params: dict, cancel: Cancellation) -> dict:
        self.session()
        local = str(params.get("locator", "")).split(":", 1)[-1]
        match = _LOCAL_PART.match(local)
        if not match:
            # A reference this adapter cannot read is a statement about the
            # reference. It is a different fact from one it can read that no
            # longer resolves, and the two codes are what let a caller tell a
            # typo from a source that moved on.
            raise ProtocolError(
                LOCATOR_UNKNOWN,
                "%r is not a notes locator, want <note-id>#<section>" % local,
            )
        note_id, ordinal = match.group(1), int(match.group(2))

        snap = self.current()
        cancel.check()

        conn = self.open_index(snap)
        try:
            conn.row_factory = sqlite3.Row
            row = conn.execute(
                "SELECT * FROM section WHERE candidate_id = ?", (local,)
            ).fetchone()
            if row is None:
                held = conn.execute(
                    "SELECT COUNT(*) AS n FROM section WHERE note_id = ?", (note_id,)
                ).fetchone()["n"]
                if held:
                    raise ProtocolError(
                        LOCATOR_EXPIRED,
                        "note %s now has %d section(s); the locator names section %d"
                        % (note_id, held, ordinal),
                    )
                raise ProtocolError(
                    LOCATOR_EXPIRED, "%s holds no note %s" % (snap.generation_id(), note_id)
                )
            siblings = conn.execute(
                "SELECT * FROM section WHERE note_id = ? AND candidate_id != ? "
                "ORDER BY ordinal",
                (note_id, local),
            ).fetchall()
        finally:
            conn.close()

        content = render(row, params.get("detail", "excerpt"), siblings)
        budget = int(params.get("budget_bytes", 0))
        truncated, boundary = False, ""
        if budget and len(content.encode("utf-8")) > budget:
            content = clip_bytes(content, budget)
            truncated, boundary = True, "budget_bytes"
        result = {
            "content": content,
            "source_revision": snap.generation_id(),
            "truncated": truncated,
            # Base name plus section, so provenance is checkable without
            # publishing where this machine keeps its notes.
            "provenance": "%s#%d" % (one_line(row["file"]), row["ordinal"]),
        }
        if boundary:
            # Present only when something was cut, so its presence is the
            # answer to "which limit applied" rather than a field to interpret.
            result["truncation_boundary"] = boundary
        return result

    # -- lifecycle ---------------------------------------------------------

    def shutdown(self, _params: dict, _cancel: Cancellation) -> dict:
        with self.lock:
            self.closed = True
            self.ready = False
            self.snapshot = None
        return {}


def render(row, detail: str, siblings) -> str:
    """Turn a section into evidence at one detail level.

    The levels widen rather than reshape: each one's output starts with the
    previous one's, so a caller comparing a summary against a full expansion
    sees added lines and not rewritten ones. An unknown level is not an
    invitation to guess how much to reveal.
    """
    head = "%s\ndocument section %d/%d, written %s" % (
        one_line(row["title"]),
        row["ordinal"] + 1,
        row["sections"],
        row["event_time"],
    )
    if row["heading"]:
        head += "\nsection: " + one_line(row["heading"])
    if detail == "summary":
        return head

    body = safe_text(row["body"])
    if detail == "excerpt":
        return head + "\n\n" + clip(body, EXCERPT_BYTES) if body else head
    if detail not in ("full", "context"):
        return head

    out = head + ("\n\n" + body if body else "")
    if row["tags"]:
        out += "\ntags: " + ", ".join(one_line(t) for t in row["tags"].split(",") if t)
    if detail == "context" and siblings:
        out += "\n\nRest of this note:"
        for other in siblings:
            # Each neighbour keeps its own locator. Grouping sections into one
            # expansion must not cost a reader the ability to expand one.
            out += "\n- %s %s" % (
                other["candidate_id"],
                clip(one_line(other["heading"] or other["body"]), 120),
            )
    return out


# --------------------------------------------------------------------------
# JSON-RPC plumbing
# --------------------------------------------------------------------------

_STDOUT_LOCK = threading.Lock()


def log(message: str):
    """Free-form logging. stderr only, and never parsed by anyone."""
    print("recall-notes: " + message, file=sys.stderr, flush=True)


def _emit(frame: dict):
    """Write one protocol frame. The only writer to stdout in this program."""
    line = json.dumps(frame, separators=(",", ":"), sort_keys=True)
    with _STDOUT_LOCK:
        sys.stdout.write(line + "\n")
        sys.stdout.flush()


class Server:
    """Reads frames, dispatches them on threads, and writes replies."""

    def __init__(self, adapter: Adapter):
        self.adapter = adapter
        self.inflight: dict[str, Cancellation] = {}
        self.lock = threading.Lock()
        self.threads: list[threading.Thread] = []
        self.handlers = {
            "recall/initialize": adapter.initialize,
            "recall/search": adapter.search,
            "recall/expand": adapter.expand,
            "recall/health": adapter.health,
            "recall/refresh": adapter.refresh,
        }

    def run(self, stream=sys.stdin) -> int:
        for line in stream:
            line = line.strip()
            if not line:
                continue
            try:
                frame = json.loads(line)
            except ValueError:
                # A malformed line cannot be answered — there is no id to answer
                # to — so it is dropped and the stream stays aligned.
                log("dropping unparseable frame")
                continue
            if not isinstance(frame, dict) or frame.get("jsonrpc") != "2.0":
                log("dropping frame that is not JSON-RPC 2.0")
                continue

            method, ident = frame.get("method"), frame.get("id")
            if method == "recall/cancel":
                self.cancel(frame.get("params") or {})
                continue
            if ident is None:
                continue  # a notification this adapter does not implement
            if method == "recall/shutdown":
                self.finish()
                _emit({"jsonrpc": "2.0", "id": ident, "result": {}})
                return 0
            if method not in self.handlers:
                _emit(error_frame(ident, METHOD_NOT_FOUND, "unknown method %r" % method))
                continue

            thread = threading.Thread(
                target=self.serve, args=(ident, method, frame.get("params") or {}), daemon=True
            )
            with self.lock:
                self.threads.append(thread)
            thread.start()

        # stdin closed. In-flight work has nowhere to reply to.
        self.finish()
        return 0

    def serve(self, ident, method: str, params: dict):
        deadline = 0.0
        if params.get("deadline"):
            try:
                deadline = parse_time(params["deadline"])
            except ValueError:
                _emit(error_frame(ident, INVALID_PARAMS, "deadline is not an RFC 3339 instant"))
                return
        elif method != "recall/initialize":
            # Every request but the handshake carries a deadline. Treating a
            # missing one as "no bound" would turn a caller's oversight into an
            # unbounded request, which is the opposite of what it should mean.
            _emit(error_frame(ident, INVALID_PARAMS, "request carries no deadline"))
            return

        cancel = Cancellation(deadline)
        key = json.dumps(ident)
        with self.lock:
            self.inflight[key] = cancel
        try:
            result = self.handlers[method](params, cancel)
            _emit({"jsonrpc": "2.0", "id": ident, "result": result})
        except ProtocolError as exc:
            _emit(error_frame(ident, exc.code, exc.message))
        except Exception as exc:  # noqa: BLE001 - a crash must still answer
            # An adapter that dies silently is reported by the core as a source
            # that did not answer, which is correct but tells nobody why. An
            # unexpected failure is still a failure with a code.
            log("%s: unhandled %s: %s" % (method, type(exc).__name__, exc))
            _emit(error_frame(ident, INTERNAL_ERROR, "%s: %s" % (type(exc).__name__, exc)))
        finally:
            with self.lock:
                self.inflight.pop(key, None)

    def cancel(self, params: dict):
        key = json.dumps(params.get("id"))
        with self.lock:
            pending = self.inflight.get(key)
        if pending is not None:
            pending.cancel()

    def finish(self):
        """Let in-flight work finish so nothing writes after the last reply."""
        with self.lock:
            threads = list(self.threads)
        for thread in threads:
            thread.join(timeout=5.0)


def error_frame(ident, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": ident, "error": {"code": code, "message": message}}


def main(argv: list[str]) -> int:
    if "--version" in argv or "-version" in argv:
        print("%s %s" % (ADAPTER_ID, "template"))
        return 0
    return Server(Adapter()).run()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
