# PROTOTYPE: the contract for one agent Step, driven from Go

Throwaway. Answers [issue #96](https://github.com/benjr70/auto-agent/issues/96)
for the map [auto-agent as a Go software factory](https://github.com/benjr70/auto-agent/issues/90).
Not production code: no tests, one file, one Step.

**Question:** when Go owns the flow and calls Claude Code for one bounded
Step, what does the call look like, what can Go rely on, and what must it
enforce itself?

**Step prototyped:** Review: correctness, run for real against
[PR 88](https://github.com/benjr70/auto-agent/pull/88) (`5ac80839...af40e295`).

## Run it

```
go build -o /tmp/stepproto . 
/tmp/stepproto -checkout <worktree at head> -base <sha> -head <sha> -model haiku
/tmp/stepproto -claude $PWD/stubs/<case>.sh -checkout <dir>     # failure cases, no model call
```

## What is here

| File | What it shows |
|---|---|
| `main.go` | `ReviewInput` and `ReviewResult` (the typed contract), `RunStep` (the one place that calls Claude Code), `StepRun` (what the journal would record), and a caller standing in for the state machine. |
| `plugin/skills/review-correctness/SKILL.md` | The Step's prompt: 39 lines against the 174 of today's `correctness-review`. No harness context block, no `gh`, no checkout, no output grammar. |
| `stubs/*.sh` | Six fake `claude` binaries, one per failure case. This is how the real test suite would exercise the runner without a model. |
| `live-run.txt` | Output of the live run. |

## The call

```
claude -p '/factory:review-correctness {"base":…,"head":…,"issue_title":…,"issue_body":…}'
  --output-format stream-json --verbose
  --json-schema '<schema>'
  --plugin-dir <embedded plugin, unpacked>
  --permission-mode dontAsk
  --tools Read,Grep,Glob,Bash
  --allowedTools 'Read,Grep,Glob,Bash(git diff:*),Bash(git log:*),Bash(git show:*)'
  --strict-mcp-config
  --setting-sources ''
  --model <from the Model policy>
```

Working directory: a checkout at `head`. Environment: the caller's, minus
every GitHub token, with `GH_CONFIG_DIR` pointed at an empty directory.

## Verified live (Claude Code 2.1.296)

- **`--json-schema` works with a plugin skill prompt and with `stream-json`.**
  The final `result` line carries `structured_output`. This was the open
  question from the headless research.
- **Structured output is a synthetic tool.** `StructuredOutput` appears in the
  init message's tool list.
- **The init message tells Go what the Step can reach:** session id, model,
  permission mode, tools, slash commands, MCP servers. The runner fails the
  Step when its skill is not in the list.
- **`--resume <session id>` works with `--json-schema`** and keeps the same
  session id. A cut-off Step can be resumed and still return a typed result.
- **The rate-limit event arrives in the stream** with `status`, `resetsAt`,
  `rateLimitType`, `unifiedWindows` (utilization per window), `isUsingOverage`
  and `overageStatus`. The last four are undocumented but present.
- **The result carries token usage and cost per call**, so per-Step usage
  needs no transcript scraping.
- **A denied tool call does not fail the run.** It is listed in
  `permission_denials` on the result; the Step carried on and returned
  success.
- **No GitHub write from inside the Step.** Under the Step's environment
  `gh auth status` reports not logged in and `git push` fails with "could not
  read Username".

## Three things that went wrong, and what they teach

1. **`--tools` takes bare tool names only.** Passing `Bash(git diff:*)` there
   silently removed Bash. The first live run reviewed the PR without ever
   seeing the diff and still returned a plausible finding and `OK`. Nothing
   failed. Go must read the tool list from the init message and compare it to
   what the Step needs.
2. **The account's claude.ai connectors load into every headless run.** The
   first runs had Gmail (including `send_message`), Calendar and Docs tools
   available to a review Step. `--strict-mcp-config` removes them. A Step that
   needs an MCP server (Verify, for the browser) gets it through an explicit
   `--mcp-config`.
3. **Killing `claude` on timeout is not enough.** A child process kept stdout
   open and the reader blocked for ever. The runner kills the process group.

## Failure cases (stubs, no model call)

| Case | Outcome | Decided by |
|---|---|---|
| Success, no `structured_output` | `MALFORMED` | runner |
| Schema-valid, but a defect with no failure scenario | `MALFORMED` | Go check after decode |
| Rejected rate-limit event plus limit text | `EXHAUSTED` | runner, from the stream |
| Skill missing from the init message | `FAILED` | runner |
| Dies with no result line (exit 139) | `FAILED` | runner |
| Never answers | `TIMED_OUT` | runner, process group killed |

## Not tested

- A schema the model cannot satisfy: how many times the client retries, and
  what the result looks like when it gives up.
- A Step that edits the checkout (Implement, Fix). It needs Edit, Write and a
  wide Bash allowance; stripping credentials is then the main guard.
- Verify, with a browser MCP server passed through `--mcp-config`.
- Whether `--setting-sources ''` also drops the Target Project's `CLAUDE.md`.
- The production model. Every live run used Haiku to protect the Daemon's
  usage budget; the finding quality here says nothing about Opus.
- A real mid-Step usage cut-off. `EXHAUSTED` was only exercised by a stub.

## Observed, for the human to decide

The reviewer tried to run `bash lib/ruling.test.sh` and was denied, because
the Step's Bash allowance covers only `git diff`, `git log` and `git show`.
Whether a read-only Step may run the Target Project's tests is a design
choice, not a mechanical one.
