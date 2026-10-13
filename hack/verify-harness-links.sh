#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# verify-harness-links.sh fails when a harness alias is not a symlink into
# .agents/, when it points somewhere else, or when a canonical skill or agent
# has no alias for a harness that only reads its own directory. Editing an alias
# instead of the canonical file (or copying it) is how the harnesses silently
# drift apart.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

status=0

fail() {
  echo "verify-harness-links: $1" >&2
  status=1
}

# alias:target-relative-to-the-alias
alias_into() {
  local alias="$1" expected="$2"
  if [ ! -L "$alias" ]; then
    if [ -e "$alias" ]; then
      fail "$alias is a copy, not a symlink to $expected"
    else
      fail "$alias is missing (expected a symlink to $expected)"
    fi
    return
  fi
  local target
  target="$(readlink "$alias")"
  if [ "$target" != "$expected" ]; then
    fail "$alias points at $target, expected $expected"
  fi
  if [ ! -e "$alias" ]; then
    fail "$alias points at $target, which does not exist"
  fi
}

# Every canonical skill is aliased for Claude Code, which only reads .claude.
for dir in .agents/skills/*/; do
  name="$(basename "$dir")"
  [ -f "$dir/SKILL.md" ] || { fail ".agents/skills/$name has no SKILL.md"; continue; }
  alias_into ".claude/skills/$name/SKILL.md" "../../../.agents/skills/$name/SKILL.md"

  # Nothing else may live next to the alias: assets belong in the canonical dir.
  while IFS= read -r stray; do
    [ "$stray" = ".claude/skills/$name/SKILL.md" ] && continue
    fail "$stray is not part of the alias"
  done < <(find ".claude/skills/$name" -mindepth 1)
done

# An alias without a canonical skill means the skill was renamed or deleted.
for dir in .claude/skills/*/; do
  [ -d "$dir" ] || continue
  name="$(basename "$dir")"
  [ -f ".agents/skills/$name/SKILL.md" ] || fail ".claude/skills/$name has no .agents/skills/$name/SKILL.md"
done

# Subagent definitions: Claude Code reads .claude/agents, omp reads .omp/agents.
for file in .agents/agents/*.md; do
  [ -e "$file" ] || continue
  name="$(basename "$file")"
  alias_into ".claude/agents/$name" "../../.agents/agents/$name"
  alias_into ".omp/agents/$name" "../../.agents/agents/$name"
done

if [ "$status" -ne 0 ]; then
  echo "verify-harness-links: aliases are symlinks into .agents/; see hack/agent/README.md" >&2
fi
exit "$status"
