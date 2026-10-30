#!/usr/bin/env bash
# Aggregate lint for the harness: bash syntax, python byte-compilation, gofmt
# and go vet.
#
# Run: bash run-lint.sh
#
# This is the Harness config's `lint` command and the single entry point CI
# calls. It is one command on purpose: the Daemon's read-only Steps may run the
# Harness config's commands and nothing else, through a permission rule naming
# each command exactly, and Claude Code refuses such a rule when the command is
# a chain that also holds a glob or a command substitution. One script, no
# chain, is always honoured.

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1

status=0

bash -n bin/auto-agent lib/*.sh plugin/providers/*.sh || status=1
python3 -m compileall -q dashboard || status=1

unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt: these files are not formatted:" >&2
  echo "$unformatted" >&2
  status=1
fi

go vet ./... || status=1

exit "$status"
