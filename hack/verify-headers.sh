#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# verify-headers.sh fails when a Go source file is missing the SPDX header.
# The goheader linter enforces the same rule for linted files; this script also
# covers files that lint skips (build-tagged files, testdata helpers) and runs
# in `make verify` without needing the lint toolchain.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

expected_header="${EXPECTED_HEADER:-SPDX-License-Identifier: AGPL-3.0-only}"
status=0

while IFS= read -r -d '' file; do
  if ! head -n 3 "$file" | grep -qF "$expected_header"; then
    echo "missing '$expected_header' in the first 3 lines: ${file#"$root"/}" >&2
    status=1
  fi
done < <(find . \( -name '*.go' -o -name '*.sh' \) -type f \
  -not -path './bin/*' \
  -not -path './vendor/*' \
  -not -path '*/testdata/*' \
  -print0)

if [ "$status" -ne 0 ]; then
  echo "verify-headers: add the header from hack/boilerplate.go.txt to the files above" >&2
fi
exit "$status"
