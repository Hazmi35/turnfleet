// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors
//
// omp pre-tool hook: route every bash command through hack/agent/guard-kubectl.sh,
// the same policy script the Claude Code adapter calls. The verdict is the
// script's exit code (2 = deny, reason on stderr), so the policy lives in one
// place and this file only wires it into omp's tool_call event.

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = import.meta.dir ?? dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, "..", "..", "..");
const GUARD = resolve(REPO, "hack/agent/guard-kubectl.sh");

export default function guardBash(pi: ExtensionAPI): void {
  pi.on("tool_call", async (event) => {
    if (event.toolName !== "bash") return;

    const command = String(event.input?.command ?? "");
    if (command.length === 0) return;

    const result = await pi.exec("bash", [GUARD, command], { cwd: REPO });
    if (result.code === 0) return;

    const reason = (result.stderr || result.stdout || "").trim();
    return {
      block: true,
      reason: reason || "Blocked by hack/agent/guard-kubectl.sh",
    };
  });
}
