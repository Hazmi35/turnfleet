#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# verify-no-claude-md.sh fails when a CLAUDE.md or CLAUDE.local.md exists
# anywhere in the tree. Claude Code reads AGENTS.md natively, but a CLAUDE.md
# in the working directory or any parent shadows it, which would silently split
# the instructions across two files.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

found="$(find . -name 'CLAUDE.md' -o -name 'CLAUDE.local.md' | grep -v '^./bin/' || true)"
if [ -n "$found" ]; then
  echo "verify-no-claude-md: AGENTS.md is the single agent instruction file." >&2
  echo "A CLAUDE.md or CLAUDE.local.md would shadow it. Remove:" >&2
  echo "$found" >&2
  exit 1
fi
exit 0
