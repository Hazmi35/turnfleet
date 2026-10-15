# Domain Docs

How the engineering skills should use this repo's domain documentation when exploring the codebase.

This is a **single-context** repo: one vocabulary and one ADR series for the whole module. There is no `GLOSSARY-MAP.md` and no per-package `GLOSSARY.md`; do not create one. If you want a per-context glossary, that is a signal: the concept belongs in the shared vocabulary instead.

## Before exploring, read these

- **`AGENTS.md` → `## Glossary`**: the canonical vocabulary. The repo's own convention points at this same section ("Naming lives in the Glossary section above; if you need a new term, add it there in the same change"). The glossary therefore lives in the instruction file rather than in a separate `GLOSSARY.md`. Read it before naming anything in an issue title, refactor proposal, hypothesis or test.
- **`docs/adr/`**: read ADRs that touch the area you're about to work in. `docs/adr/README.md` is the index; it lists the ADRs with a one-line decision each. The most important ones to know early: 0001 (addressing modes), 0003/0004 (port ranges and listener exposure), 0006 (drain semantics), 0009 (partitioned listener ports).

`docs/api.md` is generated from the API markers. Read it, never hand-edit it (same rule as `AGENTS.md` → `Never`).

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill creates them lazily when terms or decisions actually get resolved.

## File structure

```
/
├── AGENTS.md        ← `## Glossary`: the vocabulary for the whole repo
├── docs/
│   ├── adr/         ← one ADR per non-obvious decision, numbered, never renumbered
│   │   ├── README.md
│   │   ├── 0001-addressing-modes.md
│   │   └── …
│   ├── architecture.md
│   ├── capacity-model.md
│   ├── limitations.md
│   └── versions.md
└── internal/        ← one context, one vocabulary: relay, probe, metrics, controller
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in the glossary. The glossary is deliberately narrow: **Relay** (never "instance", "replica" or "server"), **Pool**, **Allocation**, **Relay address**, **Relay node**, **Draining**. Each alternative is avoided for a reason, and that drift is what the section exists to prevent.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0004 (listener exposure: per-relay node ports, not a shared Service), but worth reopening because…_

Some of these are enforced in code, not just prose. The webhook rejects a change that invalidates live allocations (relay port range, addressing mode, realm). A Service or load balancer in the relay data path is "the bug this project removes". Treat a conflict there as a design conversation, not a patch.
