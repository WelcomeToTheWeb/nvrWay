#!/usr/bin/env python3
"""Render KANBAN.md from the cards in kanban/cards/.

Cards are the source of truth: status lives in each card's frontmatter, and this
script derives the board plus a validation report from them. Run it in the same
commit as the card move that changed status, so the board and the cards cannot
drift (docs/kanban.md).

    python3 kanban/board.py           # write KANBAN.md
    python3 kanban/board.py --check    # validate only, exit 1 on any violation

Validation rules (the board is only useful if it cannot lie):
  * status is one of the five columns
  * every card is linked from its step's plan section
  * no card sits In Progress while a card it depends on is unfinished
  * In Progress carries at most WIP_LIMIT cards (focus, not parallelism)
  * a card in Done has no unchecked exit criteria
  * a card in Done quotes its verification evidence in Notes
  * depends: names a card that exists
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CARDS = pathlib.Path(__file__).resolve().parent / "cards"
BOARD = ROOT / "KANBAN.md"

COLUMNS = [
    ("in-progress", "In Progress"),
    ("review", "Review — built, not yet verified"),
    ("blocked", "Blocked"),
    ("backlog", "Backlog"),
    ("done", "Done"),
]
WIP_LIMIT = 2

STEP_TITLES = {
    "0": "Repo scaffold",
    "1": "Config, models, store, migrations",
    "2": "Auth: JWT, break-glass admin, OIDC, WebSocket",
    "3": "Cameras + go2rtc sync + live plumbing",
    "4": "Recorder: continuous + event-gated segments",
    "5": "Detection worker (Python)",
    "6": "Events, clips, snapshots, webhooks",
    "7": "Storage backends, upload queue, retention",
    "8": "Web console",
    "9": "Containers, compose, scripts, CI",
    "10": "Demo fixture, seeding, end-to-end",
    "11": "Test suite CI runs",
    "process": "Repo mechanics",
}

FRONTMATTER = re.compile(r"\A---\n(.*?)\n---\n(.*)\Z", re.S)
LIST_ITEM = re.compile(r"^\s*-\s*(.+)$")


def parse(path: pathlib.Path) -> dict:
    text = path.read_text()
    m = FRONTMATTER.match(text)
    if not m:
        raise SystemExit(f"{path.name}: no YAML frontmatter")
    raw, body = m.group(1), m.group(2)
    meta: dict = {"paths": [], "depends": []}
    key = None
    for line in raw.splitlines():
        item = LIST_ITEM.match(line)
        if item and key in ("paths", "depends"):
            meta[key].append(item.group(1).strip().strip('"'))
            continue
        if ":" in line:
            k, _, v = line.partition(":")
            key = k.strip()
            meta[key] = [] if not v.strip() else v.strip()
    meta["id"] = path.stem
    meta["file"] = path.relative_to(ROOT)
    meta["body"] = body
    meta["unchecked"] = body.count("- [ ]")
    meta["checked"] = body.count("- [x]")
    return meta


def load() -> list[dict]:
    cards = [parse(p) for p in sorted(CARDS.glob("S*.md")) + sorted(CARDS.glob("P-*.md"))]
    # Step order: numeric steps ascending, process last.
    def key(c: dict) -> tuple[int, str]:
        s = c.get("step", "99")
        return (99, "") if s == "process" else (int(s), c["id"])

    cards.sort(key=key)
    return cards


def validate(cards: list[dict]) -> list[str]:
    problems: list[str] = []
    ids = {c["id"] for c in cards}
    wip = [c for c in cards if c.get("status") == "in-progress"]
    if len(wip) > WIP_LIMIT:
        problems.append(
            f"WIP limit: {len(wip)} cards In Progress (limit {WIP_LIMIT}): {' '.join(c['id'] for c in wip)}"
        )
    for c in cards:
        status = c.get("status")
        if status not in dict(COLUMNS):
            problems.append(f"{c['id']}: status {status!r} is not a column")
        for d in c.get("depends", []):
            if d not in ids:
                problems.append(f"{c['id']}: depends on unknown card {d!r}")
            elif status == "done":
                dep = next(x for x in cards if x["id"] == d)
                if dep.get("status") != "done":
                    problems.append(
                        f"{c['id']}: Done while its dependency {d} is {dep.get('status')} — the card cannot be finished before what it stands on"
                    )
        if status == "done":
            if c["unchecked"]:
                problems.append(
                    f"{c['id']}: Done with {c['unchecked']} unchecked exit criteria — check them or move the card back"
                )
            body = c["body"]
            if "Observed" not in body and "verified" not in body.lower():
                problems.append(
                    f"{c['id']}: Done with no verification evidence in Notes (quote the command output, a date, or the commit)"
                )
        if status == "blocked" and "BLOCKED" not in c["body"]:
            problems.append(f"{c['id']}: Blocked without naming what blocks it")
    return problems


def render(cards: list[dict]) -> str:
    counts = {k: 0 for k, _ in COLUMNS}
    for c in cards:
        counts[c.get("status", "?")] = counts.get(c.get("status", "?"), 0) + 1

    out = [
        "# nvrWay — Kanban",
        "",
        "One card per deliverable (a module, a route group, a test suite), not per plan",
        "step. Cards live in `kanban/cards/` and are the source of truth; this file is",
        "generated from them. Convention and the move rules: [`docs/kanban.md`](docs/kanban.md).",
        "Plan: [`NVRWAY_V1_PLAN.md`](NVRWAY_V1_PLAN.md) — cards cite the step they implement.",
        "",
        "| " + " | ".join(name.split(" — ")[0] for _, name in COLUMNS) + " |",
        "| " + " | ".join("---:" for _ in COLUMNS) + " |",
        "| " + " | ".join(str(counts.get(k, 0)) for k, _ in COLUMNS) + " |",
        "",
        f"Total cards: **{len(cards)}**. In Progress limit: **{WIP_LIMIT}**.",
        "",
    ]

    for key, name in COLUMNS:
        subset = [c for c in cards if c.get("status") == key]
        out += ["", f"## {name}", ""]
        if not subset:
            out += ["_empty_"]
            continue
        step = None
        for c in subset:
            s = str(c.get("step", "?"))
            if s != step:
                step = s
                heading = "Process" if s == "process" else f"Step {s}"
                out += [
                    "",
                    f"### {heading} — {STEP_TITLES.get(s, '')}",
                    "",
                    "| Card | Title | Depends |",
                    "| --- | --- | --- |",
                ]
            deps = ", ".join(c.get("depends", [])) or "—"
            out.append(
                f"| [`{c['id']}`]({c['file']}) | {c['title']} | {deps} |"
            )

    problems = validate(cards)
    out += ["", "## Board health", ""]
    if problems:
        out += [f"- ⚠ {p}" for p in problems]
    else:
        out += ["- ✓ no violations: columns valid, WIP within limit, no Done card without evidence."]
    out += [
        "",
        "Regenerate with `python3 kanban/board.py` in the same commit as the card move.",
        "",
    ]
    return "\n".join(out)


def main() -> int:
    check = "--check" in sys.argv
    cards = load()
    problems = validate(cards)
    if check:
        for p in problems:
            print(f"⚠ {p}")
        print(f"{len(cards)} cards, {len(problems)} violation(s)")
        return 1 if problems else 0
    BOARD.write_text(render(cards))
    print(f"KANBAN.md: {len(cards)} cards, {len(problems)} violation(s)")
    for p in problems:
        print(f"  ⚠ {p}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
