# Agent Note: harness-board-mirror

Status: implemented

## Problem

The DeepSeek Harness workspace board (`KANBAN.json`) is what the agent sees of its own plan — the `board_*` tools write it and the web board page renders it. It held six cards while `kanban/cards/*.md` held 116, so the agent's board showed only the current session and nothing of Steps 0–7. Asked to fill it out, the user chose to mirror the whole repo board onto the harness board, reversing the earlier arrangement in which the harness board carried only session-level hand-off and the two boards deliberately did not duplicate.

A hand-mirrored copy is not maintainable: 116 cards move as work proceeds, and a board that lags the cards is worse than no board, because it is read as fact. The mirror also had to coexist with the harness's own board tools, which write the same file with richer hand-written text, and it had to be checkable — the repo's convention is that a derived artifact is regenerated and validated, not trusted.

## Decision

`kanban/harness_board.py` mirrors `kanban/cards/*.md` into the DeepSeek Harness workspace board at the repo root (`KANBAN.json`), which the agent's `board_*` tools and the web board page read. It is stdlib-only, matching `kanban/pi_board.py`, and runs as `python3 kanban/harness_board.py` (mirror) or `--check` (report drift, exit 1). Tests: `kanban/test_harness_board.py`, 25 cases, stdlib `unittest`, all in a `TemporaryDirectory`.

Cards are parsed through `board.parse`, not a private parser: `load_cards()` sets `board.ROOT` to the target tree for the duration of the call and restores it in a `finally`, because `board.parse` resolves `meta["file"]` against that global. `test_the_mirror_uses_board_pys_own_parser` asserts the mirrored id set equals `board.load()`'s on the real repo, so the two cannot drift apart unnoticed.

Status mapping is `done→done`, `backlog→todo`, `in-progress→in_progress`, `blocked→todo`. The harness has no Blocked column, so a Blocked card keeps the fact twice: a `blocked` tag, and the reason as the description's first line. The reason is not prefixed if the card's Notes already open with `BLOCKED` — the first pass read "BLOCKED — BLOCKED on Step 8".

Two merge rules are load-bearing because the harness's own `board_*` tools write the same file. **Only empty fields are filled**: `description`, `rationale`, `summary`, `rejected` are written only when absent, so text a person wrote through `board_update` survives any number of runs. **Harness-only cards survive**: an entry whose title does not begin with a repo card id (`S\d+-\d+` or `P-\d+`) is left untouched and re-appended. A card removed from `kanban/cards/` does lose its entry.

`--check` compares invariants, never text: exactly one entry per card, status parity through the mapping, non-empty `description`/`rationale`/`tags`, and a Done card carrying non-empty `summary` and `rejected`.

`kanban/pre-commit.sh` runs the mirror after `board.py`, guarded by `[ -f KANBAN.json ]`, so a machine that never runs an agent here does not grow a gitignored board file.
## Alternatives considered

**Byte-for-byte `--check`.** Rejected: the extraction is lossy by design (summaries are capped at 1500 chars, deviations at 1100) and a hand-written field is *supposed* to differ from an extracted one, so equality comparison would report permanent drift and the check would be switched off within a week. It checks the invariants that can actually break: one entry per card, status parity, no empty fields, Done cards carrying evidence.

**A second frontmatter parser inside `harness_board.py`.** Rejected: two parsers drift, and the drift is silent. The mirror calls `board.parse` and temporarily points `board.ROOT` at the target tree so `meta["file"]` resolves correctly — the coupling is explicit and restored in a `finally`, and `test_the_mirror_uses_board_pys_own_parser` asserts the mirrored id set equals `board.load()`'s on the real repo.

**Overwrite every field on each run (a pure projection).** Rejected: the harness's own `board_*` tools write the same file, and a card closed by hand through `board_update` carries richer text than any extractor can produce. Overwriting would destroy exactly the content the board exists to hold. Only empty fields are filled.

**Mirror all 116 cards as separate harness cards** was itself the alternative to keeping the boards separate, and it is what the user chose on 2026-11-05; the earlier decision (recorded when P-06 added the pi mirror) was that the harness board carries only session-level hand-off and the two must not duplicate. That decision is superseded, not ignored.

**Commit `KANBAN.json`.** Rejected: it is a derived local artifact of whoever runs an agent in this workspace, and it is already gitignored (P-07's anchoring lesson). The hook therefore runs the mirror only when the file already exists, so a plain clone never grows one.
## Consequences

**Bought:** the agent's board and the repo board can no longer disagree silently — `--check` fails on a lagging status, a missing entry, a duplicate, or a Done card without evidence, and the pre-commit hook runs it. A fresh session reading the harness web board sees the whole project, each card pointing at its source. The three extraction bugs are pinned by named tests, so a regression re-raises a failure instead of quietly writing a false "nothing was rejected" onto a card that recorded one.

**Cost:** a third derived artifact and a third mirror to keep honest; `harness_board.py` is coupled to `board.parse`'s use of the module-level `board.ROOT`, which is patched and restored at call time — a change to `board.py`'s globals is felt here. The extraction heuristics (`STRONG_MARKERS`, `DECIDE_MARKERS`, the 120-character summary floor, the 1500/1100 caps) are tuned to this corpus's house style; a card written in a different voice will extract thin, and the honest-absence sentence will be written for it. That sentence is only ever a *verified absence* — it is emitted solely when the deviation scan matched nothing — so a future card that records a rejection in wording none of the markers match will be mislabelled. The mitigation is the named tests and the pointer each card carries back to its source.

**Not done:** the mirror does not write `sourceSessionId` for new entries unless one already exists on the board to reuse; the harness's own `board_add` remains the right way to create a card that should be attributable to a session. Nothing prunes a harness-only card automatically — a follow-up that is finished by hand must be closed by hand.

