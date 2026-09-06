#!/usr/bin/env bash
set -euo pipefail

if command -v cr >/dev/null 2>&1; then
  cli=cr
elif command -v coderabbit >/dev/null 2>&1; then
  cli=coderabbit
else
  printf '%s\n' 'CodeRabbit CLI missing. Install: CI=1 curl -fsSL https://cli.coderabbit.ai/install.sh | sh' >&2
  exit 127
fi

if ! git rev-parse --show-toplevel >/dev/null 2>&1; then
  printf '%s\n' 'Run this command inside a Git repository.' >&2
  exit 2
fi

exec "$cli" review --plain --uncommitted --include-untracked "$@"
