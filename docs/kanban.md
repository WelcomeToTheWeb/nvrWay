# Kanban — how the board works

`KANBAN.md` is the board. `kanban/cards/*.md` are the cards. One card per
**deliverable** — a module, a route group, a driver, a test suite — not per plan
step, so a card is small enough to finish in one sitting and precise enough to be
checked by a single command.

```
KANBAN.md                  generated board (never hand-edited)
kanban/cards/S2-04.md      one card = frontmatter + deliverable + exit criteria + verification
kanban/seed.py             the card set derived from NVRWAY_V1_PLAN.md Steps 0-11 (source of the ids)
kanban/board.py            renders KANBAN.md from the cards, and validates the board
docs/kanban.md             this file
```

## Columns

| Column | Means |
| --- | --- |
| **In Progress** | Being worked on right now. **Limit 2.** More than two is not parallel work, it is a board that lies. |
| **Review** | Code is in. Not verified — the card's verification command has not been seen exiting 0. |
| **Blocked** | Something outside the card stops it: missing hardware, a host without a toolchain, a dependency nobody controls. The card must name the blocker in Notes. |
| **Backlog** | Ready to be picked up. A card with no exit criteria is not ready; it is a heading. |
| **Done** | The verification command exited 0 and the output is quoted in Notes. |

## Moving a card

1. Pick the card **before** writing code, set `status: in-progress`, run
   `python3 kanban/board.py`.
2. Code goes in → `status: review`.
3. Run the card's **Verification** command. Quote its output in the commit body,
   check the card's exit-criteria boxes, set `status: done`, regenerate the board.
4. The card move and the `KANBAN.md` regeneration go in **the same commit** as the
   work, so the board cannot lag the code.

A card that changes shape mid-work (an approach dropped, scope grown) is edited in
place — same id, `updated:` bumped. Cards are never minted twice for one
deliverable; the id is the handle that commits, docs and memory conversations use.

## Card anatomy

```markdown
---
id: S4-03          # <step>-<nn>, stable forever
step: 4            # plan section it implements
title: Segment discovery: tail the ffmpeg CSV into segment rows on the server clock
status: backlog    # backlog | in-progress | review | blocked | done
paths:             # files this card owns — the diff a reviewer reads
  - server/media/segments.go
depends:           # cards that must be Done first
  - S4-02
  - S1-10
updated: 2026-11-01
---

## Deliverable      what exists when the card is Done, in the plan's terms
## Exit criteria    observable, checkable statements — the review checklist
## Verification     one command that proves it
## Notes            decisions, deviations, the evidence that closed the card
```

## The board's rules (`board.py --check`, exits non-zero on any violation)

- `status` is one of the five columns.
- In Progress carries at most **2** cards.
- A **Done** card has no unchecked exit criteria.
- A **Done** card names its verification evidence in Notes.
- A **Blocked** card names what blocks it.
- `depends:` refers to a card that exists.

Run it as a pre-commit habit: `python3 kanban/board.py --check`.

Install the hook once per clone (git hooks are not themselves committed):

```bash
cp kanban/pre-commit.sh .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit
```

It runs `board.py --check` and regenerates `KANBAN.md`, staging it if the cards
moved — a board that lags the code is worse than no board.

## Re-seeding

`python3 kanban/seed.py` writes any card from the plan set that does not exist
yet. It never overwrites a card: hand-edited cards are the source of truth.
`--check` reports what is missing. Adding a deliverable to the plan means adding
one `card(...)` block to `seed.py` — the plan is the upstream, the board is derived.

## Why it lives in the repo

No forge is required, no service runs, and the board is diffable: a commit that
moves a card is a reviewable record of what was claimed done and against which
command. `gh` and a remote (P-01) are a later, additive step — a GitHub Projects
board would read from these cards, not replace them.
