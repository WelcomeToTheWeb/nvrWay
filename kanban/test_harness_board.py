#!/usr/bin/env python3
"""Tests for kanban/harness_board.py — the mirror from nvrWay's cards to the
DeepSeek Harness workspace board (KANBAN.json).

Stdlib only, matching kanban/test_pi_board.py, so it runs as:

    python3 kanban/test_harness_board.py

Every test works in a TemporaryDirectory with its own KANBAN.json, so nothing
here can damage a live board.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import harness_board  # noqa: E402

CARD = """---
id: {cid}
step: {step}
title: {title}
status: {status}
paths:
  - server/{lower}.go
depends:
  - S1-01
updated: 2026-11-05
---

# {cid} — {title}

## Deliverable

Answer the wall with the media host, and refuse an anonymous browser.

## Exit criteria

- [x] answers the host verbatim
- [x] viewer role allowed

## Verification

`go test ./api -count=1 -run Live`

## Notes

{notes}
"""


def write_card(root: pathlib.Path, cid: str, status: str, notes: str = "",
               step: str = "4", title: str = "the card") -> None:
    cards = root / "kanban" / "cards"
    cards.mkdir(parents=True, exist_ok=True)
    (cards / f"{cid}.md").write_text(
        CARD.format(cid=cid, step=step, status=status, notes=notes,
                    title=title, lower=cid.lower().replace("-", "_")),
        encoding="utf-8",
    )


def board_file(root: pathlib.Path) -> dict:
    return json.loads((root / "KANBAN.json").read_text(encoding="utf-8"))


def entry(root: pathlib.Path, cid: str) -> dict:
    for c in board_file(root)["cards"]:
        if c["title"].startswith(cid + " "):
            return c
    raise AssertionError(f"no harness entry for {cid}")


class Sync(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_one_entry_per_card_with_every_field_filled(self):
        write_card(self.root, "S4-01", "done", "**A rule.** " + "x" * 200)
        write_card(self.root, "S4-02", "backlog")
        harness_board.sync(self.root)
        board = board_file(self.root)
        self.assertEqual(board["version"], 1)
        self.assertEqual(len(board["cards"]), 2)
        for c in board["cards"]:
            for field in ("id", "title", "status", "description", "rationale", "tags"):
                self.assertTrue(c.get(field), f"{c['title']} has no {field}")

    def test_status_mapping(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        write_card(self.root, "S4-02", "backlog")
        write_card(self.root, "S4-03", "blocked", "BLOCKED on Step 8: no LivePage yet.")
        harness_board.sync(self.root)
        self.assertEqual(entry(self.root, "S4-01")["status"], "done")
        self.assertEqual(entry(self.root, "S4-02")["status"], "todo")
        blocked = entry(self.root, "S4-03")
        self.assertEqual(blocked["status"], "todo")
        self.assertIn("blocked", blocked["tags"])

    def test_blocked_reason_is_not_prefixed_twice(self):
        # The card's own Notes already open with "BLOCKED on Step 8:", and the
        # mirror adds its own marker. Both said it, and the board read
        # "BLOCKED — BLOCKED on Step 8".
        write_card(self.root, "S4-03", "blocked", "BLOCKED on Step 8: no LivePage yet.")
        harness_board.sync(self.root)
        first = entry(self.root, "S4-03")["description"].splitlines()[0]
        self.assertTrue(first.startswith("BLOCKED on Step 8"), first)
        self.assertNotIn("BLOCKED — BLOCKED", first)

    def test_title_quotes_are_stripped(self):
        # A title containing a colon has to be quoted in YAML; the quotes are
        # not part of the title.
        write_card(self.root, "S7-07", "done", "**A rule.** " + "x" * 200,
                   title='"Key layout: nvr/<slug>/<basename>.mp4"')
        harness_board.sync(self.root)
        self.assertEqual(entry(self.root, "S7-07")["title"],
                         "S7-07 — Key layout: nvr/<slug>/<basename>.mp4")

    def test_single_quoted_title_unescapes_a_doubled_apostrophe(self):
        write_card(self.root, "P-05", "done", "**A rule.** " + "x" * 200,
                   title="'Track server/models: .gitignore''s pattern ignored a package'")
        harness_board.sync(self.root)
        self.assertEqual(entry(self.root, "P-05")["title"],
                         "P-05 — Track server/models: .gitignore's pattern ignored a package")

    def test_tags_carry_the_step_and_the_area(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        tags = entry(self.root, "S4-01")["tags"]
        self.assertIn("nvrway", tags)
        self.assertIn("step-4", tags)
        self.assertIn("server", tags)


class Summary(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_a_bare_bold_heading_is_not_a_decision(self):
        # "**Decisions, with the reason.**" labels what follows. Taken as a
        # decision it became the whole summary, and the card said nothing.
        notes = "Observed 2026-11-02.\n\n**Decisions, with the reason.**\n\n" + "body " * 60
        write_card(self.root, "S2-08", "done", notes)
        harness_board.sync(self.root)
        summary = entry(self.root, "S2-08")["summary"]
        self.assertNotIn("Decisions, with the reason", summary)
        self.assertIn("body", summary)

    def test_the_verification_block_is_kept(self):
        notes = "Observed.\n\n```\n--- PASS: TestLive (0.04s)\nok  \tnvr/server/api\t0.580s\n```\n"
        write_card(self.root, "S8-04", "done", notes)
        harness_board.sync(self.root)
        self.assertIn("ok  \tnvr/server/api\t0.580s", entry(self.root, "S8-04")["summary"])

    def test_summary_points_at_the_card(self):
        write_card(self.root, "S8-04", "done", "Observed: ok.")
        harness_board.sync(self.root)
        self.assertIn("kanban/cards/S8-04.md", entry(self.root, "S8-04")["summary"])

    def test_a_backlog_card_gets_no_summary(self):
        write_card(self.root, "S8-05", "backlog")
        harness_board.sync(self.root)
        self.assertFalse(entry(self.root, "S8-05").get("summary"))


class Rejected(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_a_deviation_written_as_a_list_item_is_found(self):
        # The corpus writes its ruled-out alternatives as "- **Two gates, not
        # one.** …". Testing only paragraphs that *start* with "**" missed
        # every one of them, and the board claimed the card recorded nothing.
        notes = ("Observed: ok.\n\n"
                 "- **Two gates, not one.** RequireRole gates on a name; "
                 "RequirePermission gates on a permission row.\n"
                 "- **The rate budget caps at the burst, not at the minute.** "
                 "A tenth-sized burst is the plan's number.\n")
        write_card(self.root, "S2-08", "done", notes)
        harness_board.sync(self.root)
        rejected = entry(self.root, "S2-08")["rejected"]
        self.assertIn("Two gates, not one", rejected)

    def test_a_deviation_in_the_opening_paragraph_is_still_recorded(self):
        notes = "**Empty WORKER_TOKEN is 403, not 401.** A missing token is a disabled feature.\n"
        write_card(self.root, "S2-08", "done", notes)
        harness_board.sync(self.root)
        self.assertIn("403, not 401", entry(self.root, "S2-08")["rejected"])

    def test_a_card_that_recorded_nothing_says_so_and_points_at_itself(self):
        # This sentence is a verified absence, not filler: it is written only
        # when the scan above found nothing, and it names where to correct it.
        write_card(self.root, "S1-01", "done", "Observed 2026-11-01: go test ./... EXIT=0.\n")
        harness_board.sync(self.root)
        rejected = entry(self.root, "S1-01")["rejected"]
        self.assertTrue(rejected.startswith("No rejected alternative is recorded"))
        self.assertIn("kanban/cards/S1-01.md", rejected)

    def test_a_done_card_always_has_a_rejected_field(self):
        write_card(self.root, "S1-02", "done", "Observed: green.")
        harness_board.sync(self.root)
        self.assertTrue(entry(self.root, "S1-02")["rejected"])


class Merge(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_hand_written_fields_are_never_overwritten(self):
        write_card(self.root, "S8-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"][0]["rationale"] = "hand written why"
        board["cards"][0]["summary"] = "hand written what"
        board["cards"][0]["rejected"] = "hand written rejection"
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")

        harness_board.sync(self.root)
        after = entry(self.root, "S8-01")
        self.assertEqual(after["rationale"], "hand written why")
        self.assertEqual(after["summary"], "hand written what")
        self.assertEqual(after["rejected"], "hand written rejection")

    def test_an_empty_description_is_filled(self):
        write_card(self.root, "S8-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"][0]["description"] = ""
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        harness_board.sync(self.root)
        self.assertTrue(entry(self.root, "S8-01")["description"])

    def test_a_clean_re_run_changes_nothing(self):
        write_card(self.root, "S4-01", "done", "**A rule.** " + "x" * 200)
        first = harness_board.sync(self.root)
        self.assertEqual(first["added"], 1)
        second = harness_board.sync(self.root)
        self.assertEqual(second["added"], 0)
        self.assertEqual(second["filled"], 0)

    def test_harness_only_cards_survive(self):
        # Follow-up cards exist only on the harness board; a mirror run must
        # not delete them.
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"].append({"id": "card-follow-up", "title": "Follow-up: something",
                               "status": "todo", "description": "d", "rationale": "r",
                               "tags": ["nvrway"]})
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        result = harness_board.sync(self.root)
        titles = [c["title"] for c in board_file(self.root)["cards"]]
        self.assertIn("Follow-up: something", titles)
        self.assertEqual(result["kept"], 1)

    def test_a_card_that_disappeared_is_dropped(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        (self.root / "kanban/cards/S4-01.md").unlink()
        harness_board.sync(self.root)
        self.assertEqual(board_file(self.root)["cards"], [])


class Check(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_a_synced_board_reports_nothing(self):
        write_card(self.root, "S4-01", "done", "**A rule.** " + "x" * 200)
        harness_board.sync(self.root)
        self.assertEqual(harness_board.check(self.root), [])

    def test_a_status_that_lags_the_card_is_reported(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"][0]["status"] = "todo"
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        problems = harness_board.check(self.root)
        self.assertEqual(len(problems), 1)
        self.assertIn("S4-01", problems[0])

    def test_a_done_card_without_evidence_is_reported(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"][0]["summary"] = ""
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        self.assertTrue(any("summary" in p for p in harness_board.check(self.root)))

    def test_a_missing_card_is_reported(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        write_card(self.root, "S4-02", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"] = board["cards"][:1]
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        self.assertTrue(any("S4-02" in p for p in harness_board.check(self.root)))

    def test_a_duplicated_card_is_reported(self):
        write_card(self.root, "S4-01", "done", "Observed: ok.")
        harness_board.sync(self.root)
        board = board_file(self.root)
        board["cards"].append(dict(board["cards"][0]))
        (self.root / "KANBAN.json").write_text(json.dumps(board), encoding="utf-8")
        problems = harness_board.check(self.root)
        self.assertEqual(len(problems), 1, problems)
        self.assertIn("S4-01", problems[0])


class Loader(unittest.TestCase):
    def test_the_mirror_uses_board_pys_own_parser(self):
        # Two parsers drift. The mirror parses through board.py, so the ids it
        # mirrors are exactly the ids the repo board renders.
        import board
        repo = pathlib.Path(__file__).resolve().parent.parent
        mirrored = {c["id"] for c in harness_board.load_cards(repo / "kanban" / "cards", repo)}
        rendered = {c["id"] for c in board.load()}
        self.assertEqual(mirrored, rendered)


if __name__ == "__main__":
    unittest.main(verbosity=2)
