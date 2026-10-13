# Agent hooks

The checks every coding agent runs here, written so that no harness owns them.
Each script takes its input as arguments and answers with an exit code; none of
them knows about Claude Code's JSON hook protocol or omp's extension API.

| Script | Arguments | Exit codes |
| --- | --- | --- |
| `guard-kubectl.sh` | one shell command | `0` may run, `2` denied (reason on stderr) |
| `format-go.sh` | file paths | `0` always; formatting is best-effort, `make verify` is the gate |
| `verify-fast.sh` | none | `0` clean or passing, `2` failed (tail on stderr), `3` could not run |

`guard-kubectl.sh` implements the repository's one-cluster rule: kubectl is
only allowed with `--context kind-turnfleet`, and the context is never changed.
`verify-fast.sh` runs `make verify-fast` and stays silent on a pristine tree.

Wiring lives with each harness and is deliberately thin:

| Harness | Wiring |
| --- | --- |
| Claude Code | `.claude/hooks/*.sh` translate Claude's stdin JSON into these arguments, and its verdict into `permissionDecision`/exit 2. `.claude/settings.json` holds the permission lists, which are Claude-specific by nature. |
| omp | `.omp/hooks/pre/guard-bash.ts` and `.omp/hooks/post/format-go.ts` call the same scripts from `tool_call` and `tool_result`. |

omp has a `session_stop` event that could host the finish gate, but its handlers
are cancelled after a fixed 30-second budget (only
`extensionHandlers.toolCallTimeoutMs` is configurable) while `make verify-fast`
rebuilds the pinned linter and runs it — about 46s here. Wiring it there would
either stall every turn or be cancelled mid-build, so the finish gate stays
Claude Code's, and omp sessions rely on `AGENTS.md` telling the agent to run
`make ci`.

Shared content that is not executable lives in `.agents/`: the skills
(`.agents/skills/<name>/SKILL.md`) and the `api-reviewer` subagent definition
(`.agents/agents/api-reviewer.md`). `.claude/` and `.omp/` reach them through
symlinks, because each harness only discovers its own directory; edit the file
under `.agents/`, never the alias. `hack/verify-harness-links.sh` fails the
build when an alias is missing, points somewhere else, or was replaced by a
copy.
