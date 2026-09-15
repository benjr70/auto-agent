---
name: dry-run
description: "The no-op Fire: proves the Harness install loaded as a plugin and that a Fire can run end to end against a Target Project without writing anything. Invoked by `bin/auto-agent fire --dry-run`; never by a human."
disable-model-invocation: true
---

You are running as the auto-agent Daemon's dry-run Fire. Your only job is to
show that the plugin loaded and the Fire completed.

Rules:

- Do not create, edit or delete any file.
- Do not run `gh`, `git` with a writing subcommand, or any command that changes
  state. Reading is fine but not required.
- Do not use any tool unless a rule below asks you to.

Do exactly this:

1. Print one line: `dry-run: plugin auto-agent loaded`.
2. Print one line: `dry-run: cwd <the working directory you were started in>`
   (use the `cwd` you already know; do not run a command to find it).
3. Print one line, exactly: `dry-run: ok`.

Nothing else. No summary, no offer of next steps.
