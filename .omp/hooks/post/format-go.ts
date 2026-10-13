// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors
//
// omp post-tool hook: after an edit or write, format the Go files it touched
// with hack/agent/format-go.sh, the same script the Claude Code adapter calls.
// Formatting is best-effort; `make verify` is the gate.

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { dirname, isAbsolute, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = import.meta.dir ?? dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, "..", "..", "..");
const FORMAT = resolve(REPO, "hack/agent/format-go.sh");

const FORMATTED_TOOL: Record<string, true> = { edit: true, write: true };

function touchedPaths(input: Record<string, unknown> | undefined, cwd: string): string[] {
  const paths = new Set<string>();
  const single = input?.path;
  if (typeof single === "string") paths.add(single);
  const many = input?.paths;
  if (Array.isArray(many)) {
    for (const entry of many) {
      if (typeof entry === "string") paths.add(entry);
    }
  }
  return [...paths]
    .filter((path) => path.endsWith(".go"))
    .map((path) => (isAbsolute(path) ? path : resolve(cwd, path)));
}

export default function formatGo(pi: ExtensionAPI): void {
  pi.on("tool_result", async (event, ctx) => {
    if (event.isError) return;
    if (!FORMATTED_TOOL[event.toolName]) return;

    const files = touchedPaths(event.input, ctx.cwd ?? REPO);
    if (files.length === 0) return;

    await pi.exec("bash", [FORMAT, ...files], { cwd: REPO });
  });
}
