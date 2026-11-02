#!/usr/bin/env python3
"""Mirror this repo's Kanban cards into the pi-kanban board's task store.

Two boards, one truth. The cards in `kanban/cards/` are the source of truth
(docs/kanban.md); the pi-kanban dev tool keeps its own task list
(`<agent dir>/kanban/tasks/<list>/<id>.json`, one JSON file per task) and shows
"no active tasks" when nothing wrote it. This script derives that store from the
cards so the tool's board shows this project's real state. It never writes into
`kanban/cards/` — delete the mirror and the cards are untouched.

    python3 kanban/pi_board.py                      # mirror into the live board
    python3 kanban/pi_board.py --check              # report drift, exit 1 on any
    python3 kanban/pi_board.py --dry-run            # show what it would write
    python3 kanban/pi_board.py --list nvrway --session <id>   # shared list + session
    python3 kanban/pi_board.py --tasks-dir <dir>    # any store (tests use a temp dir)

Mapping, and the two constraints that force it (pi-kanban's task list has only
`pending | in_progress | completed`, and sorts by `parseInt(id, 10)`):

    backlog, blocked      -> pending        (the Blocked reason is quoted in the description)
    in-progress, review   -> in_progress
    done                  -> completed
    card id S2-04         -> task id 204    (step * 100 + number; process cards 9000+)
    card depends:         -> blockedBy, and its inverse in blocks:
"""

from __future__ import annotations

import argparse
import datetime
import importlib.util
import json
import os
import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent
DEFAULT_LIST = "nvrway"

# pi-kanban's task list has three states; the board has five columns.
STATUS_MAP = {
    "backlog": "pending",
    "blocked": "pending",
    "in-progress": "in_progress",
    "review": "in_progress",
    "done": "completed",
}

PROCESS_BASE = 9000
SUBJECT_MAX = 88
CARD_NUMBER = re.compile(r"(\d+)\s*$")
BLOCKED_LINE = re.compile(r"^\s*.*BLOCKED.*$", re.M)


def load_cards() -> list[dict]:
    """The cards, through board.py's own parser — one reader, no second schema."""
    spec = importlib.util.spec_from_file_location("nvr_board", HERE / "board.py")
    board = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(board)
    return board.load()


def task_id(card_id: str, step: str) -> str:
    """Numeric so the board's `parseInt(id, 10)` sort keeps cards in step order."""
    m = CARD_NUMBER.search(card_id)
    number = int(m.group(1)) if m else 0
    step_s = str(step)
    base = PROCESS_BASE if step_s == "process" else int(step_s) * 100
    return str(base + number)


def map_status(status: str) -> str:
    return STATUS_MAP.get(status, "pending")


def one_line(text: str, limit: int) -> str:
    flat = " ".join(str(text).split())
    return flat if len(flat) <= limit else flat[: limit - 1].rstrip() + "…"


def description_for(card: dict) -> str:
    lines = [
        f"Card `{card['id']}` · plan step {card['step']} · board column **{card.get('status')}**",
        f"File: `{card['file']}`",
    ]
    depends = card.get("depends") or []
    if depends:
        lines.append(f"Depends: {', '.join(depends)}")
    paths = card.get("paths") or []
    if paths:
        lines.append("Owns: " + ", ".join(f"`{p}`" for p in paths[:6]))
    if card.get("status") == "blocked":
        m = BLOCKED_LINE.search(card.get("body", ""))
        reason = m.group(0).strip().lstrip("#*- ").strip() if m else ""
        lines.append(f"Blocked — {reason}" if reason else "Blocked — see the card")
    lines.append(f"Card `updated: {card.get('updated', '—')}`")
    return "\n".join(lines)


def build_task(card: dict, by_id: dict[str, dict]) -> dict:
    tid = task_id(card["id"], card["step"])
    blocked_by = [task_id(d, by_id[d]["step"]) for d in card.get("depends", []) if d in by_id]
    blocks = sorted(
        task_id(other["id"], other["step"])
        for other in by_id.values()
        if card["id"] in (other.get("depends") or [])
    )
    status = map_status(card.get("status"))
    subject = one_line(f"{card['id']} · {card.get('title', '')}", SUBJECT_MAX)
    active = one_line(f"{card['id']} · {card.get('title', '')}", 60) if status == "in_progress" else None
    return {
        "id": tid,
        "subject": subject,
        "description": description_for(card),
        "activeForm": active,
        "status": status,
        "blocks": blocks,
        "blockedBy": blocked_by,
    }


def tasks_for(cards: list[dict]) -> dict[str, dict]:
    by_id = {c["id"]: c for c in cards}
    return {task_id(c["id"], c["step"]): build_task(c, by_id) for c in cards}


def dump(task: dict) -> str:
    # Sorted keys, no timestamps: an unchanged card yields a byte-identical file,
    # so a re-run of the mirror rewrites nothing.
    return json.dumps(task, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def read_store(dir: pathlib.Path) -> dict[str, tuple[dict, str]]:
    out: dict[str, tuple[dict, str]] = {}
    if not dir.exists():
        return out
    for p in sorted(dir.glob("*.json")):
        raw = p.read_text()
        try:
            out[p.stem] = (json.loads(raw), raw)
        except json.JSONDecodeError:
            out[p.stem] = (None, raw)
    return out


def sync(cards: list[dict], dir: pathlib.Path, *, dry: bool = False) -> int:
    want = tasks_for(cards)
    have = read_store(dir)
    if not dry:
        dir.mkdir(parents=True, exist_ok=True)
    written = 0
    for tid, task in want.items():
        body = dump(task)
        current = have.get(tid)
        if current is None or current[1] != body:
            written += 1
            if not dry:
                (dir / f"{tid}.json").write_text(body)
    for tid in sorted(set(have) - set(want)):
        written += 1
        if not dry:
            (dir / f"{tid}.json").unlink()
    return written


def check(cards: list[dict], dir: pathlib.Path) -> list[str]:
    want = tasks_for(cards)
    have = read_store(dir)
    drift: list[str] = []
    for tid, task in sorted(want.items()):
        got = have.get(tid)
        if got is None:
            drift.append(f"{tid}: card has no task file — run the mirror")
            continue
        data, _ = got
        if data is None:
            drift.append(f"{tid}: task file is not valid JSON")
            continue
        for field in ("subject", "status", "blockedBy", "blocks"):
            if data.get(field) != task[field]:
                drift.append(f"{tid}: {field} is {data.get(field)!r}, the card says {task[field]!r}")
    for tid in sorted(set(have) - set(want)):
        drift.append(f"{tid}: task with no card — the mirror owns this store, delete it")
    return drift


def agent_dir() -> pathlib.Path:
    env = os.environ.get("PI_CODING_AGENT_DIR")
    return pathlib.Path(env).expanduser() if env else pathlib.Path.home() / ".pi" / "agent"


def store_paths(list_name: str) -> tuple[pathlib.Path, pathlib.Path]:
    base = agent_dir() / "kanban"
    return base / "tasks" / list_name, base / "agent-activity" / "_task-maps"


def register_sessions(tasks_dir: pathlib.Path, maps_dir: pathlib.Path, list_name: str, sessions: dict[str, str]) -> None:
    """Tell the board which sessions read this shared task list.

    The board reads <agent dir>/kanban/agent-activity/_task-maps/<list>.json as
    {sessionId: {project, updatedAt}}, and points those sessions at
    kanban/tasks/<list>/. Its getCustomTaskDir() only honours a list whose
    directory exists, so this creates it. Existing entries are kept.
    """
    tasks_dir.mkdir(parents=True, exist_ok=True)
    maps_dir.mkdir(parents=True, exist_ok=True)
    path = maps_dir / f"{list_name}.json"
    data = {}
    if path.exists():
        try:
            data = json.loads(path.read_text())
        except json.JSONDecodeError:
            data = {}
    now = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")
    for sid, project in sessions.items():
        entry = dict(data.get(sid) or {})
        entry["project"] = project
        entry["updatedAt"] = now
        data[sid] = entry
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Mirror kanban/cards into the pi-kanban task store")
    ap.add_argument("--list", default=DEFAULT_LIST, help=f"shared task list name (default {DEFAULT_LIST})")
    ap.add_argument("--session", action="append", default=[], help="session id to point at this list (repeatable)")
    ap.add_argument("--tasks-dir", default=None, help="write the store here instead of the live agent dir")
    ap.add_argument("--check", action="store_true", help="report drift, exit 1 on any")
    ap.add_argument("--dry-run", action="store_true", help="count what would change, write nothing")
    ap.add_argument("--quiet", action="store_true", help="print one summary line")
    args = ap.parse_args(argv)

    cards = load_cards()
    if args.tasks_dir:
        tasks_dir = pathlib.Path(args.tasks_dir).expanduser().resolve()
        maps_dir = tasks_dir.parent.parent / "agent-activity" / "_task-maps"
    else:
        tasks_dir, maps_dir = store_paths(args.list)

    if args.check:
        drift = check(cards, tasks_dir)
        for d in drift:
            print(f"⚠ {d}")
        print(f"{len(cards)} cards, {len(drift)} drift(s) in {tasks_dir}")
        return 1 if drift else 0

    sessions = {s: str(ROOT) for s in args.session}
    live = os.environ.get("PI_KANBAN_SESSION_ID")
    if live:
        sessions[live] = str(ROOT)

    changed = sync(cards, tasks_dir, dry=args.dry_run)
    if not args.dry_run:
        register_sessions(tasks_dir, maps_dir, args.list, sessions)

    by_status: dict[str, int] = {}
    for t in tasks_for(cards).values():
        by_status[t["status"]] = by_status.get(t["status"], 0) + 1
    summary = (
        f"{'would write' if args.dry_run else 'mirror'}: {len(cards)} cards -> {tasks_dir} "
        f"({by_status.get('pending', 0)} pending, {by_status.get('in_progress', 0)} in_progress, "
        f"{by_status.get('completed', 0)} completed), {changed} file(s) changed"
    )
    if not args.quiet:
        print(summary)
        print(f"sessions registered in {maps_dir / (args.list + '.json')}: {', '.join(sorted(sessions)) or '—'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
