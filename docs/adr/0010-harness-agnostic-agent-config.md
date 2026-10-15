# 10. The agent harness is harness-agnostic

* Status: accepted
* Date: 2026-10-13

## Context

`AGENTS.md` claims to be the only instruction file for every coding agent, but
everything else an agent needs was Claude Code shaped and lived only under
`.claude/`:

* hooks that parsed Claude's stdin JSON protocol and keyed off
  `${CLAUDE_PROJECT_DIR}`;
* skills that only Claude Code discovers;
* a subagent definition whose frontmatter carried Claude's `model: sonnet`
  alias;
* a permission list in Claude's syntax.

A second harness got none of it: omp reads `.agents/skills`,
`.omp/agents/*.md` and `.omp/hooks/{pre,post}/*.ts`, and skips `.claude/agents`
on purpose. The only way to give it the same guardrails would have been to copy
them.

Copying is the failure mode worth designing out: two guards that drift, two
skill texts that disagree, and a repository rule ("kubectl only against
kind-turnfleet") that is enforced in exactly one harness.

## Decision

Separate the *policy* from the *wiring*.

**Canonical content, harness-neutral.**

* `.agents/skills/<name>/SKILL.md`: the skills.
* `.agents/agents/<name>.md`: the subagent definitions, with no
  harness-specific frontmatter beyond `name`, `description` and `tools`.
* `hack/agent/*.sh`: the hook policy, holding `guard-kubectl.sh`,
  `format-go.sh` and `verify-fast.sh`. Arguments in, exit code out (`0`
  allow/pass, `2` deny/fail with the reason on stderr, `3` unknown), so no
  script knows a harness's JSON protocol. The contract is in
  `hack/agent/README.md`.

**Wiring, per harness, thin.**

* `.claude/settings.json` (Claude's permission lists and hook registration) and
  `.claude/hooks/*.sh`, which translate stdin JSON into arguments and the exit
  code into `permissionDecision`/exit 2.
* `.omp/hooks/pre/guard-bash.ts` (`tool_call`) and `.omp/hooks/post/format-go.ts`
  (`tool_result`), which call the same scripts.
* omp's `session_stop` event could host the finish gate, but omp cancels
  handlers after a fixed 30-second budget, and only
  `extensionHandlers.toolCallTimeoutMs` is configurable. `make verify-fast`
  rebuilds the pinned linter and runs it, which takes about 46 s on the
  reference machine. A gate that is cancelled mid-build, leaving a half-built
  `bin/`, is worse than the "run `make ci`" the agent already gets from
  `AGENTS.md`, so no stop hook is wired for omp.
* Decision-time aliases are symlinks, because a harness only discovers its own
  directory: `.claude/skills/<name>/SKILL.md` and
  `.claude/agents/<name>.md`/`.omp/agents/<name>.md` point into `.agents/`.
  Aliasing the `SKILL.md` *file* rather than the skill *directory* keeps every
  `skills/` entry a real directory, which is what a `readdir`-based loader with
  an `isDirectory()` filter needs to see. `hack/verify-harness-links.sh`, part
  of `make verify`, fails on a missing alias, a redirected one, a dangling one,
  a stray file next to an alias, or an alias left behind by a rename.

**Permission and approval lists stay harness-native** (Claude's
`permissions.allow`/`deny`, omp's approval settings). They encode each
harness's prompt UX rather than repository policy. What must hold for every
harness is a script under `hack/agent/`.

## Consequences

* One place to edit, one place to read: a new skill is added under `.agents/`
  and gets its two aliases, and `make verify` refuses a tree where they
  disagree.
* The finish gate was already broken for any change that touched no Go file.
  With `pipefail`, the `grep '\.go$'` in `make verify-fast`'s vet step exited 1
  when no Go file had changed, `set -e` aborted the recipe, and the target
  failed with no diagnostic. An agent that had only touched docs or YAML was
  told to fix a failure that did not exist. The pipeline now tolerates the
  no-match case, and `make verify-fast` passes in that tree.
* The `api-reviewer` definition no longer pins `sonnet`: another harness would
  have read that alias as its own model selector. Each harness now runs the
  reviewer on its session model.
* The aliases are symlinks, so the checkout has to materialize them (Linux,
  macOS, or Windows with developer mode). A copy is not an acceptable
  substitute, and the verifier says so.
* Editing an alias instead of the canonical file is now a "never" in
  `AGENTS.md`, next to "run kubectl only against kind-turnfleet", the rule the
  guard enforces for every harness.
* Verified on 2026-10-13 against both harnesses.

  Claude Code 2.1.296:

  * a `kubectl get pods -A` call is denied with the guard's reason;
  * a `Write` of unformatted Go comes back gofmt-formatted;
  * the `Stop` gate hands a failing `make verify-fast` back to the model;
  * `Skill` loads `release-check` through the `.claude/skills` alias;
  * the `Task` tool dispatches `api-reviewer` through the `.claude/agents`
    alias.

  omp 18.10.3:

  * the guard blocks the same command while the `--context kind-turnfleet`
    form reaches kubectl;
  * an edit with `--no-lsp` still comes out gofmt-formatted;
  * `api-reviewer` spawns from the symlinked `.omp/agents` file with the tools
    `read, grep, glob, bash`.

  omp normalized the shared `Read, Grep, Glob, Bash` CSV, so the one
  frontmatter satisfies both harnesses.
