#!/usr/bin/env python3
"""Self-tests for invariants a copied adapter must preserve."""

from __future__ import annotations

import json
import os
import tempfile
import unittest
from unittest import mock

import recall_notes as notes


def note_text(**changes) -> str:
    fields = {
        "id": "note-one",
        "title": "A title",
        "date": "2026-07-25T12:00:00Z",
        "tags": "alpha, beta",
        "aliases": "alias-one, alias-two",
        "sensitivity": "confidential",
        "derived_from": "tasks:td-123, mail:message-9",
        "body": "Lead body.\n\n## A heading\nSection body.",
    }
    fields.update(changes)
    return (
        "id: {id}\n"
        "title: {title}\n"
        "date: {date}\n"
        "tags: {tags}\n"
        "aliases: {aliases}\n"
        "sensitivity: {sensitivity}\n"
        "derived_from: {derived_from}\n\n"
        "{body}\n"
    ).format(**fields)


class TextInvariantTests(unittest.TestCase):
    def test_clip_never_exceeds_byte_limit(self):
        for value in ("abcdef", "éclair", "😀😀😀", "abc 😀 def", " \tword  "):
            for limit in range(len(value.encode("utf-8")) + 2):
                with self.subTest(value=value, limit=limit):
                    clipped = notes.clip(value, limit)
                    self.assertLessEqual(len(clipped.encode("utf-8")), limit)
                    clipped.encode("utf-8").decode("utf-8")

    def test_safe_text_removes_every_prohibited_control(self):
        c0 = "".join(chr(code) for code in range(0x00, 0x20) if code not in (0x09, 0x0A))
        c1 = "".join(chr(code) for code in range(0x7F, 0xA0))
        bidi = "".join(chr(code) for code in range(0x202A, 0x202F))
        bidi += "".join(chr(code) for code in range(0x2066, 0x206A))
        cleaned = notes.safe_text("left\r\nmiddle\r" + c0 + c1 + bidi + "\u2028\u2029right")
        self.assertNotIn("\r", cleaned)
        self.assertIsNone(notes._CONTROL.search(cleaned))
        self.assertNotIn("\u2028", cleaned)
        self.assertNotIn("\u2029", cleaned)
        self.assertEqual(cleaned, "left\nmiddle\n\n\n\nright")


class ContentIdentityTests(unittest.TestCase):
    def fingerprint(self, text: str, ordinal: int = 1) -> str:
        record = notes.parse_note("note.md", text)
        return notes.fingerprint(record, ordinal, record["sections"][ordinal])

    def test_fingerprint_covers_every_material_field(self):
        baseline = self.fingerprint(note_text())
        changes = {
            "title": "Another title",
            "date": "2026-07-26T12:00:00Z",
            "tags": "alpha, gamma",
            "aliases": "alias-one, alias-three",
            "sensitivity": "restricted",
            "derived_from": "tasks:td-999",
            "body": "Changed lead.\n\n## A heading\nSection body.",
            "heading": "Lead body.\n\n## Another heading\nSection body.",
            "section_body": "Lead body.\n\n## A heading\nChanged section body.",
        }
        for field, value in changes.items():
            key = "body" if field in ("heading", "section_body") else field
            with self.subTest(field=field):
                self.assertNotEqual(self.fingerprint(note_text(**{key: value})), baseline)

    def test_watermark_digest_covers_raw_source_content(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as workdir:
            path = os.path.join(root, "note.md")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(note_text())
            first = notes.build(workdir, root, 1, 10)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(note_text(title="Another title"))
            second = notes.build(workdir, root, 2, 10)
            self.assertNotEqual(first.digest, second.digest)
            self.assertNotEqual(first.watermark(), second.watermark())


class PublicationInvariantTests(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        self.workdir = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.root.name, "note.md")
        self.write(note_text())
        self.adapter = self.new_adapter()
        self.first = self.adapter.current()

    def tearDown(self):
        self.workdir.cleanup()
        self.root.cleanup()

    def write(self, body: str):
        with open(self.path, "w", encoding="utf-8") as handle:
            handle.write(body)

    def new_adapter(self) -> notes.Adapter:
        adapter = notes.Adapter()
        adapter.initialize(
            {
                "protocol_version_min": 1,
                "protocol_version_max": 1,
                "source_id": "notes",
                "location": self.root.name,
                "workdir": self.workdir.name,
                "settings": {},
            },
            notes.Cancellation(0),
        )
        return adapter

    def test_checkpoint_failure_never_publishes_staged_generation(self):
        checkpoint = os.path.join(self.workdir.name, notes.CHECKPOINT_FILE)
        with open(checkpoint, "rb") as handle:
            published = handle.read()
        self.write(note_text(title="staged but not published"))
        staged = []

        def reject(_workdir, snapshot):
            staged.append(snapshot.generation_id())
            return False

        with mock.patch.object(notes, "save_checkpoint", side_effect=reject):
            current = self.adapter.current(full=True)
        self.assertIs(current, self.first)
        self.assertEqual(self.adapter.generation, self.first.generation)
        with open(checkpoint, "rb") as handle:
            self.assertEqual(handle.read(), published)

        # Simulate a crash after staging and another source change. The durable
        # checkpoint still says generation 1, so the restart chooses sequence 2
        # again; the digest binding keeps changed content from reusing identity.
        self.write(note_text(title="different content after restart"))
        restarted = self.new_adapter()
        recovered = restarted.current()
        self.assertNotEqual(recovered.generation_id(), staged[0])
        self.assertEqual(recovered.generation, self.first.generation + 1)

    def test_generation_failure_retains_published_checkpoint_and_index(self):
        checkpoint = os.path.join(self.workdir.name, notes.CHECKPOINT_FILE)
        with open(checkpoint, "rb") as handle:
            published = handle.read()
        self.write(note_text(body="Changed body"))
        real_replace = os.replace

        def fail_generation(source, destination):
            if os.path.basename(source).startswith("build-"):
                raise OSError("disk full")
            return real_replace(source, destination)

        with mock.patch.object(notes.os, "replace", side_effect=fail_generation):
            current = self.adapter.current(full=True)
        self.assertIs(current, self.first)
        self.assertEqual(self.adapter.generation, self.first.generation)
        self.assertIn("generation build failed", self.adapter.refresh_failure)
        with open(checkpoint, "rb") as handle:
            self.assertEqual(handle.read(), published)
        connection = self.adapter.open_index(self.first)
        try:
            self.assertEqual(
                connection.execute("SELECT COUNT(*) FROM section").fetchone()[0], 2
            )
        finally:
            connection.close()

    def test_checkpoint_rename_failure_publishes_nothing(self):
        with tempfile.TemporaryDirectory() as empty_workdir:
            staged = notes.build(empty_workdir, self.root.name, 1, 10)
            real_replace = os.replace

            def fail_checkpoint(source, destination):
                if destination.endswith(notes.CHECKPOINT_FILE):
                    raise OSError("read-only checkpoint")
                return real_replace(source, destination)

            with mock.patch.object(notes.os, "replace", side_effect=fail_checkpoint):
                self.assertFalse(notes.save_checkpoint(empty_workdir, staged))
            self.assertFalse(
                os.path.exists(os.path.join(empty_workdir, notes.CHECKPOINT_FILE))
            )

    def test_checkpoint_round_trip_binds_exact_index(self):
        with open(
            os.path.join(self.workdir.name, notes.CHECKPOINT_FILE), encoding="utf-8"
        ) as handle:
            checkpoint = json.load(handle)
        loaded = notes.snapshot_from_checkpoint(self.workdir.name, checkpoint)
        self.assertIsNotNone(loaded)
        self.assertEqual(loaded.generation_id(), self.first.generation_id())
        checkpoint["index_file"] = "../outside.sqlite"
        self.assertIsNone(notes.snapshot_from_checkpoint(self.workdir.name, checkpoint))

    def test_partial_snapshot_never_confirms_candidates(self):
        self.write(note_text(body="needle"))
        with open(os.path.join(self.root.name, "z-note.md"), "w", encoding="utf-8") as handle:
            handle.write(note_text(id="note-two", body="needle"))
        partial = notes.Adapter()
        partial.initialize(
            {
                "protocol_version_min": 1,
                "protocol_version_max": 1,
                "source_id": "notes",
                "location": self.root.name,
                "workdir": tempfile.mkdtemp(dir=self.workdir.name),
                "settings": {"max_notes": 1},
            },
            notes.Cancellation(0),
        )
        result = partial.search(
            {"query": "needle", "limit": 10}, notes.Cancellation(0)
        )
        self.assertEqual(result["outcome"], "partial")
        self.assertTrue(result["candidates"])
        self.assertTrue(
            all("confirmed_at" not in candidate for candidate in result["candidates"])
        )
        self.assertEqual(
            result["candidates"][0]["derived_from"],
            ["tasks:td-123", "mail:message-9"],
        )


if __name__ == "__main__":
    unittest.main()
