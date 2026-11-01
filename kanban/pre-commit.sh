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
