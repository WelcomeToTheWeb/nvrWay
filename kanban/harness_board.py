#!/usr/bin/env python3
"""Mirror nvrWay's cards into the DeepSeek Harness workspace board (KANBAN.json).

    python3 kanban/harness_board.py          # mirror
    python3 kanban/harness_board.py --check  # report drift, exit 1 if any

The repo board (`kanban/cards/*.md`) is the source of truth and stays that way:
every entry this writes ends with a pointer back to its card, and nothing here
invents a sentence the card does not already contain. The harness board is a
hand-off view of the cards, not a second place decisions live.

Two rules make it safe to run next to the harness's own board tools, which write
the same file:

* **Only empty fields are filled.** A `rationale`, `summary` or `rejected` a
  person wrote is never overwritten by an extracted one, so a card closed by
  hand keeps its hand-written text through any number of runs.
* **Harness-only cards survive.** A follow-up card with no repo counterpart is
  left exactly where it is.

`--check` does not compare text — the extraction is lossy by design and a
hand-written field is legitimately different. It checks the invariants that
actually matter: every card has exactly one entry, the status matches, and a
Done card carries its evidence.
"""

from __future__ import annotations

import json
import pathlib
import re
import sys
import time
import uuid

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import board  # noqa: E402

ROOT = board.ROOT
BOARD_FILE = ROOT / "KANBAN.json"
CARDS = ROOT / "kanban" / "cards"

# The harness has no blocked column, so a Blocked card becomes a todo card that
# says so twice over: a tag for grouping, and the reason as the first line of
# its description.
STATUS_MAP = {"done": "done", "backlog": "todo", "blocked": "todo", "in-progress": "in_progress"}

FENCE = "```"
REPO_ID = re.compile(r"^(S\d+-\d+|P-\d+)\b")
BOLD_LEAD = re.compile(r"(?m)^(?:[-*] )?\*\*[^*\n]{4,}\*\*")

# A deviation is a decision that ruled something out. These markers only ever
# select text the card itself wrote; they never invent one. STRONG marks an
# explicit rejection. DECIDE marks this corpus's house style for a ruled-out
# alternative — "**Two gates, not one.**", "403, not 401" — and is only trusted
# on a paragraph that opens a line with a bold lead, so ordinary prose cannot
# trigger it.
STRONG_MARKERS = (
    "rejected", "deviation", "beyond the card", "not here", "instead of",
    "rather than", "not this card", "would have led", "lost to",
    "out of scope", "deferred",
)
DECIDE_MARKERS = (", not ", ", never ", "not one", "only behaviour", "deliberately",
                  "refused", "no path out")

# Written only when the card's own Notes were scanned and contain no rejection
# language at all. It states a verified absence, so the field is honest rather
# than blank, and it names where to correct it. Do not extend it to a card
# without running the scan — that is how it becomes a lie.
NO_REJECTION = (
    "No rejected alternative is recorded on this card. Its Notes were read on "
    "{date} and name no declined option and no deviation from the plan — the plan "
    "named the shape and the implementation followed it. If one was weighed and "
    "never written down, kanban/cards/{cid}.md is where to add it."
)
READ_DATE = "2026-11-05"


# ---------------------------------------------------------------- parsing ----

def scalar(value: str) -> str:
    """Strip YAML quoting so a title containing a colon keeps its own words.

    `title: "Key layout: nvr/<slug>/…"` is quoted because of the colon; the
    quotes are not part of the title. A single-quoted value escapes an apostrophe
    by doubling it, so that is undone too.
    """
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        inner = value[1:-1]
        if value[0] == "'":
            inner = inner.replace("''", "'")
        return inner
    return value


def load_cards(cards_dir: pathlib.Path, root: pathlib.Path) -> list[dict]:
    """Parse the cards with board.py's own parser.

    `board.parse` resolves `meta["file"]` against `board.ROOT`, so pointing the
    mirror at another tree means pointing the parser there too — done here and
    restored, rather than copying the parser and letting the two drift apart.
    """
    saved = board.ROOT
    board.ROOT = root
    try:
        paths = sorted(cards_dir.glob("S*.md")) + sorted(cards_dir.glob("P-*.md"))
        return [board.parse(p) for p in paths]
    finally:
        board.ROOT = saved


def section(body: str, name: str) -> str:
    m = re.search(r"^## " + re.escape(name) + r"\s*$\n(.*?)(?=^## |\Z)", body, re.M | re.S)
    return m.group(1).strip() if m else ""


def notes_tail(body: str) -> str:
    """Everything from `## Notes` to the end of the card.

    Several cards continue the record under their own heading after Notes —
    "## What closed it", "## Probe, 2026-11-02 …" — and a deviation written
    there is still a deviation.
    """
    m = re.search(r"^## Notes\s*$\n", body, re.M)
    return body[m.end():].strip() if m else ""


def paragraphs(text: str) -> list[str]:
    """Blank-line separated, with fenced code blocks kept whole."""
    out: list[str] = []
    cur: list[str] = []
    fence = False
    for line in text.split("\n"):
        if line.strip().startswith(FENCE):
            fence = not fence
            cur.append(line)
            continue
        if not line.strip() and not fence:
            if cur:
                out.append("\n".join(cur).strip())
                cur = []
        else:
            cur.append(line)
    if cur:
        out.append("\n".join(cur).strip())
    return [p for p in out if p]


def cap(paras: list[str], limit: int) -> str:
    out: list[str] = []
    total = 0
    for p in paras:
        if out and total + len(p) + 2 > limit:
            break
        out.append(p)
        total += len(p) + 2
    return "\n\n".join(out)


def one_line(text: str, limit: int = 200) -> str:
    flat = re.sub(r"\s+", " ", text.replace("`", "")).strip()
    return flat[:limit] + ("…" if len(flat) > limit else "")


def as_list(value) -> list[str]:
    if not value:
        return []
    return value if isinstance(value, list) else [value]


def area(paths: list[str]) -> str:
    for p in paths:
        if p.startswith("web/"):
            return "web"
        if p.startswith("server/"):
            return "server"
        if p.startswith("docs/"):
            return "docs"
        if p.startswith("deploy/") or p.startswith("scripts/") or "docker" in p:
            return "infra"
    return "general"


def is_deviation(paragraph: str) -> bool:
    """True when the card's own text names something it ruled out.

    The house style here is a bold lead — "**Two gates, not one.**" — and those
    leads are usually **list items**, so the paragraph starts with "- " rather
    than "**". BOLD_LEAD therefore matches a bold opener at the start of any
    line, optionally after a bullet.
    """
    low = paragraph.lower()
    if any(m in low for m in STRONG_MARKERS):
        return True
    return bool(BOLD_LEAD.search(paragraph)) and any(m in low for m in DECIDE_MARKERS)


# ---------------------------------------------------------------- building ----

def build(card: dict) -> dict:
    cid = card["id"]
    status = STATUS_MAP.get(card.get("status", "backlog"), "todo")
    deliverable = section(card["body"], "Deliverable") or section(card["body"], "What to build")
    criteria = section(card["body"], "Exit criteria")
    verification = one_line(section(card["body"], "Verification")
                            or section(card["body"], "Verification command"))
    notes = notes_tail(card["body"])
    paths, depends = as_list(card.get("paths")), as_list(card.get("depends"))

    description: list[str] = []
    if card.get("status") == "blocked":
        head = paragraphs(notes)[:1]
        reason = head[0] if head else "the blocker is named on the card"
        # The card's Notes already open with "BLOCKED on Step 8:". Adding the
        # marker on top of that read "BLOCKED — BLOCKED on Step 8".
        if not re.match(r"^BLOCKED\b", reason, re.I):
            reason = "BLOCKED — " + reason
        description.append(one_line(reason, 400))
    if deliverable:
        description.append(deliverable)
    if criteria:
        description.append("Exit criteria:\n" + criteria)
    if verification:
        description.append("Verification: " + verification)
    description.append("Source of truth: kanban/cards/%s.md — full evidence, "
                       "deviations and verification output." % cid)

    rationale = "Step %s of NVRWAY_V1_PLAN.md. %s" % (card.get("step", "?"),
                                                     deliverable or card.get("title", ""))
    if depends:
        rationale += " Depends on: %s." % ", ".join(depends)
    if paths:
        rationale += " Owns: %s." % ", ".join(paths)

    summary = rejected = ""
    if status == "done":
        prose = [p for p in paragraphs(notes) if not p.startswith(FENCE)]
        opening = prose[:1]
        # The length floor is what keeps a label paragraph ("**Decisions, with
        # the reason.**") out of the summary: it carries no content, and every
        # one of the eight in the corpus is under 120 characters.
        decisions = [p for p in prose if p not in opening and len(p) > 120][:3]
        evidence = [p for p in paragraphs(notes) if p.startswith(FENCE)][:1]
        summary = cap(opening + decisions + evidence, 1500)
        if summary:
            summary += "\n\nFull evidence: kanban/cards/%s.md" % cid
        drops = [p for p in prose if is_deviation(p)][:3]
        rejected = cap(drops, 1100) if drops else NO_REJECTION.format(date=READ_DATE, cid=cid)

    tags = ["nvrway", "step-%s" % card.get("step", "?"), area(paths)]
    if card.get("status") == "blocked":
        tags.append("blocked")

    return {
        "title": "%s — %s" % (cid, scalar(str(card.get("title", "")))),
        "status": status,
        "description": "\n\n".join(description),
        "rationale": rationale,
        "summary": summary,
        "rejected": rejected,
        "tags": tags,
    }


# ------------------------------------------------------------------ merge ----

def merge(cards: list[dict], existing: list[dict]) -> tuple[list[dict], dict]:
    """Fold the built entries into the board that is already there."""
    by_repo: dict[str, dict] = {}
    for c in existing:
        m = REPO_ID.match(c.get("title", ""))
        if m:
            by_repo[m.group(1)] = c

    session = next((c["sourceSessionId"] for c in existing if c.get("sourceSessionId")), None)
    now = int(time.time() * 1000)
    out: list[dict] = []
    added = filled = 0

    for card in cards:
        built = build(card)
        cur = by_repo.get(card["id"])
        if cur is None:
            entry = {"id": "card-" + str(uuid.uuid4()), "createdAt": now, "updatedAt": now}
            if session:
                entry["sourceSessionId"] = session
            entry.update(built)
            out.append(entry)
            added += 1
            continue
        # A hand-written field is never overwritten by an extracted one.
        for field in ("description", "rationale", "summary", "rejected"):
            if not cur.get(field) and built.get(field):
                cur[field] = built[field]
                if field == "description":
                    filled += 1
        cur["status"] = built["status"]
        cur["updatedAt"] = now
        out.append(cur)

    kept = 0
    for c in existing:  # harness-only cards have no repo counterpart
        if not REPO_ID.match(c.get("title", "")):
            out.append(c)
            kept += 1

    return out, {"cards": len(cards), "added": added, "filled": filled, "kept": kept}


def read_board(path: pathlib.Path) -> dict:
    if path.exists():
        return json.loads(path.read_text(encoding="utf-8"))
    return {"version": 1, "cards": []}


def sync(root: pathlib.Path = ROOT, board_file: pathlib.Path = None) -> dict:
    board_file = board_file or (root / "KANBAN.json")
    data = read_board(board_file)
    cards = load_cards(root / "kanban" / "cards", root)
    entries, counts = merge(cards, data.get("cards", []))
    data["version"] = data.get("version", 1)
    data["cards"] = entries
    board_file.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    return counts


# ------------------------------------------------------------------ check ----

def check(root: pathlib.Path = ROOT, board_file: pathlib.Path = None) -> list[str]:
    board_file = board_file or (root / "KANBAN.json")
    cards = load_cards(root / "kanban" / "cards", root)
    entries = read_board(board_file).get("cards", [])

    seen: dict[str, int] = {}
    for e in entries:
        m = REPO_ID.match(e.get("title", ""))
        if m:
            seen[m.group(1)] = seen.get(m.group(1), 0) + 1

    problems: list[str] = []
    for cid, n in seen.items():
        if n > 1:
            problems.append("%s: mirrored %d times" % (cid, n))

    by_repo = {}
    for e in entries:
        m = REPO_ID.match(e.get("title", ""))
        if m and m.group(1) not in by_repo:
            by_repo[m.group(1)] = e

    for card in cards:
        cid = card["id"]
        want = STATUS_MAP.get(card.get("status", "backlog"), "todo")
        e = by_repo.get(cid)
        if e is None:
            problems.append("%s: no harness entry" % cid)
            continue
        if e.get("status") != want:
            problems.append("%s: harness status %r, card says %r" % (cid, e.get("status"), want))
        for field in ("description", "rationale", "tags"):
            if not e.get(field):
                problems.append("%s: empty %s" % (cid, field))
        if want == "done":
            for field in ("summary", "rejected"):
                if not e.get(field):
                    problems.append("%s: Done card has no %s" % (cid, field))
    return problems


def main() -> int:
    check_mode = "--check" in sys.argv[1:]
    if check_mode:
        problems = check()
        for p in problems:
            print("drift: " + p)
        print("harness board: %d cards, %d drift(s)" % (len(load_cards(CARDS, ROOT)), len(problems)))
        return 1 if problems else 0

    counts = sync()
    print("harness board: %(cards)d cards mirrored -> KANBAN.json "
          "(%(added)d new, %(filled)d descriptions filled, %(kept)d harness-only kept)" % counts)
    return 0


if __name__ == "__main__":
    sys.exit(main())
