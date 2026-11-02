#!/usr/bin/env python3
"""Tests for kanban/pi_board.py — the mirror from nvrWay's cards to pi-kanban's task store.

Stdlib only (host Python 3.14 has no pip, docs/kanban.md), so it runs as:

    python3 kanban/test_pi_board.py

Everything touches a TemporaryDirectory, never the real ~/.pi/agent/kanban, so the
tests are runnable anywhere and cannot damage a live board.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import pi_board  # noqa: E402


def card(cid: str, step: str, status: str, depends=()) -> dict:
    return {
        "id": cid,
        "step": step,
        "title": f"card {cid}",
        "status": status,
        "depends": list(depends),
        "paths": [f"server/{cid}.go"],
        "file": f"kanban/cards/{cid}.md",
        "body": "## Notes\nBLOCKED: no toolchain\n",
        "unchecked": 0,
        "checked": 1,
    }


SAMPLE = [
    card("S1-07", "1", "done"),
    card("S1-12", "1", "done"),
    card("S1-14", "1", "done"),
    card("S2-04", "2", "in-progress", ["S1-07", "S1-12"]),
    card("S2-05", "2", "backlog", ["S2-04"]),
    card("S9-06", "9", "blocked", ["S9-01"]),
    card("S11-04", "11", "review", ["S11-03"]),
    card("P-06", "process", "backlog", ["P-04"]),
]


class Ids(unittest.TestCase):
    def test_numeric_and_sortable(self):
        for c in SAMPLE:
            tid = pi_board.task_id(c["id"], c["step"])
            self.assertTrue(tid.isdigit(), f"{c['id']} -> {tid!r} is not numeric")
            self.assertLess(int(tid), 100000)

    def test_step_and_number_embedded(self):
        self.assertEqual(pi_board.task_id("S1-14", "1"), "114")
        self.assertEqual(pi_board.task_id("S11-04", "11"), "1104")
        self.assertEqual(pi_board.task_id("S0-01", "0"), "1")
        self.assertEqual(pi_board.task_id("P-06", "process"), "9006")

    def test_no_collisions_on_the_real_board(self):
        metas = pi_board.load_cards()
        ids = [pi_board.task_id(c["id"], c["step"]) for c in metas]
        self.assertEqual(len(ids), len(set(ids)), f"duplicate task ids: {ids}")


class StatusMap(unittest.TestCase):
    def test_columns_collapse_to_the_three_the_board_has(self):
        self.assertEqual(pi_board.map_status("backlog"), "pending")
        self.assertEqual(pi_board.map_status("blocked"), "pending")
        self.assertEqual(pi_board.map_status("in-progress"), "in_progress")
        self.assertEqual(pi_board.map_status("review"), "in_progress")
        self.assertEqual(pi_board.map_status("done"), "completed")

    def test_unknown_status_is_pending_not_a_crash(self):
        self.assertEqual(pi_board.map_status("nonsense"), "pending")


class TaskShape(unittest.TestCase):
    def setUp(self):
        self.by_id = {c["id"]: c for c in SAMPLE}

    def task(self, cid):
        return pi_board.build_task(self.by_id[cid], self.by_id)

    def test_fields_the_board_reads(self):
        t = self.task("S2-04")
        for field in ("id", "subject", "description", "activeForm", "status", "blocks", "blockedBy"):
            self.assertIn(field, t)
        self.assertEqual(t["status"], "in_progress")
        self.assertIn("S2-04", t["subject"])
        self.assertEqual(t["id"], "204")

    def test_depends_become_blockedBy_ids(self):
        self.assertEqual(self.task("S2-05")["blockedBy"], ["204"])
        self.assertEqual(self.task("S2-05")["blocks"], [])

    def test_blocks_is_the_inverse_of_blockedBy(self):
        t = self.task("S2-04")
        self.assertIn("205", t["blocks"])
        self.assertIn("107", t["blockedBy"])
        self.assertIn("112", t["blockedBy"])

    def test_unknown_dependency_is_dropped_not_crashed(self):
        # S9-06 depends on S9-01, which is not in the sample: the task points at the
        # dependencies it can resolve, and board.py --check is what catches the gap.
        t = self.task("S9-06")
        self.assertEqual(t["blockedBy"], [])
        self.assertIn("S9-01", t["description"])

    def test_blocked_names_its_blocker_in_the_description(self):
        self.assertIn("BLOCKED", self.task("S9-06")["description"])

    def test_subjects_stay_short(self):
        for c in SAMPLE:
            self.assertLessEqual(len(self.task(c["id"])["subject"]), 90)


class Sync(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = pathlib.Path(self.tmp.name)

    def write_task(self, tid: str, **over) -> None:
        t = {"id": tid, "subject": "x", "description": "", "activeForm": None,
             "status": "pending", "blocks": [], "blockedBy": []}
        t.update(over)
        (self.dir / f"{tid}.json").write_text(json.dumps(t))

    def test_sync_writes_one_file_per_card(self):
        n = pi_board.sync(SAMPLE, self.dir)
        self.assertEqual(n, len(SAMPLE))
        files = sorted(p.stem for p in self.dir.glob("*.json"))
        self.assertEqual(files, sorted(pi_board.task_id(c["id"], c["step"]) for c in SAMPLE))

    def test_sync_is_idempotent(self):
        pi_board.sync(SAMPLE, self.dir)
        first = {p.name: p.read_text() for p in self.dir.glob("*.json")}
        changed = pi_board.sync(SAMPLE, self.dir)
        self.assertEqual(changed, 0, "a second sync must rewrite nothing")
        second = {p.name: p.read_text() for p in self.dir.glob("*.json")}
        self.assertEqual(first, second, "a second sync must not rewrite unchanged tasks")

    def test_sync_removes_tasks_for_cards_that_are_gone(self):
        pi_board.sync(SAMPLE, self.dir)
        self.write_task("99999", subject="stale")
        pi_board.sync(SAMPLE, self.dir)
        self.assertFalse((self.dir / "99999.json").exists())

    def test_check_is_quiet_when_in_sync(self):
        pi_board.sync(SAMPLE, self.dir)
        self.assertEqual(pi_board.check(SAMPLE, self.dir), [])

    def test_check_names_every_drift(self):
        pi_board.sync(SAMPLE, self.dir)
        # Change exactly one field on one task, so each kind of drift is one line.
        path = self.dir / "204.json"
        task = json.loads(path.read_text())
        task["status"] = "completed"
        path.write_text(json.dumps(task, indent=2, sort_keys=True) + "\n")
        (self.dir / "205.json").unlink()                  # missing task
        self.write_task("42424", subject="ghost")         # extra task
        drift = pi_board.check(SAMPLE, self.dir)
        self.assertEqual(len(drift), 3, drift)
        joined = " ".join(drift)
        self.assertIn("204", joined)
        self.assertIn("205", joined)
        self.assertIn("42424", joined)


class RealBoard(unittest.TestCase):
    """The mirror against the 111 cards that are actually in the repo."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = pathlib.Path(self.tmp.name)
        self.cards = pi_board.load_cards()

    def test_every_card_mirrors(self):
        self.assertGreaterEqual(len(self.cards), 100)
        pi_board.sync(self.cards, self.dir)
        self.assertEqual(len(list(self.dir.glob("*.json"))), len(self.cards))

    def test_counts_match_the_columns(self):
        metas = self.cards
        pi_board.sync(metas, self.dir)
        want = {"pending": 0, "in_progress": 0, "completed": 0}
        for c in metas:
            want[pi_board.map_status(c["status"])] += 1
        got = {"pending": 0, "in_progress": 0, "completed": 0}
        for p in self.dir.glob("*.json"):
            got[json.loads(p.read_text())["status"]] += 1
        self.assertEqual(got, want)

    def test_blocked_cards_carry_their_blocker(self):
        blocked = [c for c in self.cards if c["status"] == "blocked"]
        self.assertGreaterEqual(len(blocked), 1)
        pi_board.sync(self.cards, self.dir)
        for c in blocked:
            t = json.loads((self.dir / f"{pi_board.task_id(c['id'], c['step'])}.json").read_text())
            self.assertIn("Blocked", t["description"])

    def test_json_is_what_pi_kanban_parses(self):
        pi_board.sync(self.cards, self.dir)
        for p in self.dir.glob("*.json"):
            t = json.loads(p.read_text())
            self.assertEqual(p.stem, str(t["id"]))
            self.assertIn(t["status"], ("pending", "in_progress", "completed"))
            self.assertIsInstance(t["blocks"], list)
            self.assertIsInstance(t["blockedBy"], list)
            self.assertTrue(t["subject"].strip())


class SharedList(unittest.TestCase):
    def test_session_registration_file(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = pathlib.Path(tmp.name)
        tasks_dir = root / "tasks" / "nvrway"
        maps_dir = root / "agent-activity" / "_task-maps"
        pi_board.register_sessions(tasks_dir, maps_dir, "nvrway", {"sess-1": "/root/projects/nvrWay"})
        data = json.loads((maps_dir / "nvrway.json").read_text())
        self.assertIn("sess-1", data)
        self.assertEqual(data["sess-1"]["project"], "/root/projects/nvrWay")
        self.assertIn("updatedAt", data["sess-1"])
        self.assertTrue(tasks_dir.exists(), "the board only uses a shared list whose dir exists")

    def test_registration_keeps_other_sessions(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = pathlib.Path(tmp.name)
        tasks_dir = root / "tasks" / "nvrway"
        maps_dir = root / "agent-activity" / "_task-maps"
        maps_dir.mkdir(parents=True)
        (maps_dir / "nvrway.json").write_text(json.dumps({"old": {"project": "/x"}}))
        pi_board.register_sessions(tasks_dir, maps_dir, "nvrway", {"new": "/root/projects/nvrWay"})
        data = json.loads((maps_dir / "nvrway.json").read_text())
        self.assertEqual(set(data), {"old", "new"})


if __name__ == "__main__":
    unittest.main(verbosity=2)
