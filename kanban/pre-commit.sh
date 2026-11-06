#!/bin/sh
# Git pre-commit hook: keep KANBAN.md derived and the board honest.
#
# Install (it cannot be committed inside .git, so install it once per clone):
#   cp kanban/pre-commit.sh .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit
#
# What it enforces:
#   * the board validates (columns, WIP limit, Done cards carry evidence)
#   * KANBAN.md is regenerated from the cards, so it cannot lag them
#
# Bypass deliberately with: git commit --no-verify   (say why in the message)
set -e
cd "$(git rev-parse --show-toplevel)"
python3 kanban/board.py --check
python3 kanban/board.py
if ! git diff --quiet -- KANBAN.md; then
	git add KANBAN.md
	echo "pre-commit: KANBAN.md regenerated and staged"
fi

# The pi-kanban mirror (P-06): the dev tool's task store is derived from these
# cards, so a commit that moves a cards moves the tool's board in the same breath.
# Quietly skipped when this machine has no pi-kanban board installed.
PK_AGENT="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
if [ -d "$PK_AGENT/kanban" ]; then
	python3 kanban/pi_board.py --list nvrway --quiet \
		|| echo "pre-commit: pi-kanban mirror did not run — python3 kanban/pi_board.py"
fi

# The DeepSeek Harness mirror (P-09): the workspace board (KANBAN.json) is
# derived from the same cards. Skipped when the tree has no KANBAN.json, so a
# plain clone never grows one — the file is gitignored and belongs to whoever
# runs an agent in this workspace.
if [ -f KANBAN.json ]; then
	python3 kanban/harness_board.py >/dev/null \
		|| echo "pre-commit: harness board mirror did not run — python3 kanban/harness_board.py"
fi
