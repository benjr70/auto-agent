# What Claude Code headless mode offers for schema-checked step results

Research for [#92](https://github.com/benjr70/auto-agent/issues/92), part of map [#90](https://github.com/benjr70/auto-agent/issues/90).
Researched 2026-10-11 against the official docs and the locally installed `claude` 2.1.296 (`claude --help` only; no prompt was run).

## Answer in brief

- There is **no official Go Agent SDK**. The Agent SDK is Python and TypeScript only, and both are wrappers that spawn the same `claude` binary. The docs tell every other language to run the CLI as a subprocess. A Go caller uses `claude -p`.
- `claude -p --output-format json|stream-json --json-schema '<schema>'` gives a **client-validated, schema-conforming object** in the result message's `structured_output` field, after the agent has finished its tool use. Claude Code re-prompts on mismatch and ends with an error result when it gives up.
- The **result message** is the contract: one JSON object (`json`) or the last line of the stream (`stream-json`) carrying `subtype`, `is_error`, `session_id`, `structured_output`, `permission_denials`, `terminal_reason`, cost and usage.
- The **exit code is coarse**: 0 on success, non-zero on failure, 143 on SIGTERM. No table of exit codes is published. The reason for a failure is in the result message, not the exit code.
- Go must still enforce: "success with no `structured_output`" is a failure; its own re-validation of the object; a wall-clock timeout; the out-of-budget vs dead-credential vs real-failure classification; and a check of `system/init` that the plugin and skill it supplied actually loaded.

## Is there a Go SDK?

No. The Agent SDK overview describes the SDK as "programmable in Python and TypeScript", lists only the TypeScript and Python packages and changelogs, and says:

> To drive the same agent loop from a language other than Python or TypeScript, run the CLI as a subprocess with the `-p` flag and `--output-format json`.

([Agent SDK overview](https://code.claude.com/docs/en/agent-sdk/overview))

The SDK is itself "a library that runs the Claude Code binary" (same page); the Python SDK "launches the Claude Code CLI as a subprocess" and the TypeScript SDK ships a bundled native CLI binary ([SDK troubleshooting](https://code.claude.com/docs/en/agent-sdk/troubleshooting)). So a Go program that spawns `claude -p` is on the same footing as the official SDKs: same binary, same message stream. What the SDKs add is typed message objects, in-process callbacks (`canUseTool`, in-process MCP tools, hooks as functions) and `interrupt()`. None of that is needed for "one bounded step, typed result".

Anthropic does publish a Go *client* SDK for the Claude API, but that is the raw Messages API where the caller writes the tool loop; it is not Claude Code and brings none of its tools, skills, plugins or sessions (the overview's comparison table draws this line between "Agent SDK" and "Client SDK").

One further constraint from the overview: Anthropic does not allow third-party products to offer claude.ai login or rate limits, "including agents built on the Claude Agent SDK". auto-agent today runs the user's own `claude` under the user's own login, which is the CLI's ordinary use; that note is about products offered to others.

## Output formats

`--output-format` takes `text` (default), `json` or `stream-json`, and only works with `--print` ([headless: get structured output](https://code.claude.com/docs/en/headless#get-structured-output), confirmed in `claude --help` 2.1.296).

| Format | What stdout carries | Use for a Go step |
| - | - | - |
| `text` | The final text only | No |
| `json` | One JSON object: the result message ("structured JSON with result, session ID, and metadata") | Simplest; no rate-limit or init events |
| `stream-json` | Newline-delimited JSON events; "the last line of the stream is a `result` message" | Needed to see `system/init` and `rate_limit_event` |

The docs always pair `stream-json` with `--verbose` in their examples, and `lib/fire.sh` passes both today. `--include-partial-messages` adds token-level `stream_event` lines and is not needed for a step result.

With `json`/`stream-json` and a non-terminal stdout, the "Waiting for background work to finish" notice goes to stderr only, "so the JSON your script reads never carries it" ([headless: background tasks at exit](https://code.claude.com/docs/en/headless#background-tasks-at-exit)).

## Structured output: `--json-schema`

The flag exists, in print mode only:

> `--json-schema`: Get validated JSON output matching a JSON Schema after the agent completes its workflow (print mode only). ... Claude Code exits with an error on an invalid schema and accepts the `format` keyword as an annotation without client-side validation

([CLI reference](https://code.claude.com/docs/en/cli-reference#cli-flags); present in `claude --help` 2.1.296 as "JSON Schema for structured output validation".)

What the docs promise:

- **Where the value lands.** "The response includes metadata about the request (session ID, usage, etc.) with the structured output in the `structured_output` field" ([headless](https://code.claude.com/docs/en/headless#get-structured-output)). `result` still holds the free text.
- **It is the agent's final answer, not a single-turn constraint.** "The agent can use any tools it needs to complete the task, and you still get validated JSON matching your schema at the end" ([structured outputs](https://code.claude.com/docs/en/agent-sdk/structured-outputs)).
- **Validation and retry are done by Claude Code.** "the SDK validates the output against it, re-prompting on mismatch. If validation does not succeed within the retry limit, the result is an error instead of structured data" (same page). The error result has `subtype: "error_max_structured_output_retries"`. The retry limit is not stated.
- **Schema dialect.** "The SDK validates schemas with JSON Schema draft-07, so schemas that declare a newer version are rejected." Supported: basic types, `enum`, `const`, `required`, nested objects, `$ref`. `format` is accepted but not enforced.
- **A bad schema fails fast.** `Error: --json-schema is not a valid JSON Schema` plus the validator's diagnostic; before v2.1.205 it was silently ignored and unstructured text came back ([headless](https://code.claude.com/docs/en/headless#get-structured-output)). Pin a minimum CLI version of 2.1.205.

What the docs warn the caller must handle:

- **Success without a value.** "A result can also end with subtype `success` but no `structured_output` value ... Treat that case as a failure as well." One documented cause is an unsatisfiable schema ([structured outputs: error handling](https://code.claude.com/docs/en/agent-sdk/structured-outputs#error-handling), [troubleshooting](https://code.claude.com/docs/en/agent-sdk/troubleshooting#structured_output-is-none-but-the-result-says-success)).
- **Retraction.** A model fallback "can retract an already-completed output mid-stream, and if no retry replaces it the run ends with the same error"; the `errors` list tells the two causes apart.
- **Semantics are not checked.** The schema says the object has a `status` enum; it does not say the PR the object names exists. Go checks facts against GitHub and git.

## The result message

Documented as `SDKResultMessage` in the [TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript); the CLI emits the same object.

Success arm (`subtype: "success"`), fields a Go caller would decode:

| Field | Type | Note |
| - | - | - |
| `type` | `"result"` | |
| `subtype` | `"success"` | |
| `is_error` | boolean | Typed `boolean`, not `false`, on the success arm, so check it as well as `subtype` |
| `api_error_status` | number, null or absent | "the HTTP status code of the API error that terminated the conversation" |
| `result` | string | Final text |
| `structured_output` | any, optional | Present only with a schema and a validated answer |
| `session_id` | string | For `--resume` |
| `num_turns`, `duration_ms`, `duration_api_ms` | number | |
| `total_cost_usd`, `usage`, `modelUsage` | | Client-side estimates; on a resumed session the total covers the whole conversation |
| `stop_reason` | string or null | |
| `permission_denials` | array of `{tool_name, tool_use_id, tool_input}` | "the authoritative record" of denied tool calls |
| `terminal_reason` | string, optional | See below |

Error arm: `subtype` is one of `error_max_turns`, `error_during_execution`, `error_max_budget_usd`, `error_max_structured_output_retries`. It has no `result` and no `structured_output`; it has `errors: string[]` and, for a refused start, `startup_failure_reason` ("That result carries zeroed totals, and its `errors` array carries the same text as stderr").

`terminal_reason` says why the loop ended: `completed`, `max_turns`, `tool_deferred`, `aborted_streaming`, `aborted_tools`, `hook_stopped`, `stop_hook_prevented`, `background_requested`, `blocking_limit`, `rapid_refill_breaker`, `prompt_too_long`, `image_error`, `model_error`, `api_error`, `malformed_tool_use_exhausted`, `budget_exhausted`, `structured_output_retry_exhausted`, `tool_deferred_unavailable`, `turn_setup_failed`. It is optional, so treat it as a hint and an open set.

## Other stream-json events worth decoding

The stream is a union of some 35 message types (`SDKMessage` in the TypeScript reference). A step runner needs four and should ignore every `type`/`subtype` it does not know.

**`system` / `init`**: first event unless plugin-install or SessionStart/Setup hook events precede it ([headless: read session metadata](https://code.claude.com/docs/en/headless#read-session-metadata)). Carries `session_id`, `model`, `cwd`, `claude_code_version`, `permissionMode`, `tools`, `skills`, `slash_commands`, `agents`, `mcp_servers[{name,status}]`, `plugins[{name,path}]`, optional `plugin_errors[{plugin,type,message,path?}]`, optional `mcp_server_errors`, optional `capabilities`. The docs recommend failing CI on a non-empty `plugin_errors` and feature-detecting on `capabilities` rather than version strings.

**`rate_limit_event`**: documented shape ([TypeScript reference: SDKRateLimitEvent](https://code.claude.com/docs/en/agent-sdk/typescript)):

```typescript
type SDKRateLimitEvent = {
  type: "rate_limit_event";
  rate_limit_info: {
    status: "allowed" | "allowed_warning" | "rejected";
    resetsAt?: number;
    utilization?: number;
    errorCode?: "credits_required";
    canUserPurchaseCredits?: boolean;
    hasChargeableSavedPaymentMethod?: boolean;
  };
  uuid: UUID;
  session_id: string;
};
```

Only `status`, `resetsAt` and `utilization` (plus the credits fields) are documented. `lib/rate-limits-tap.sh` also reads `rateLimitType`, `isUsingOverage`, `overageStatus` and `unifiedWindows` from the same object. Those are **not in the published type**, so the Go decoder must treat them as optional and keep working from the three documented fields. The docs describe the event as "emitted when the session encounters a rate limit" and do not say how often it is emitted or that one is guaranteed per run.

**`system` / `api_retry`**: emitted before each retry of a retryable API failure, with `attempt`, `max_retries`, `retry_delay_ms`, `error_status` and `error`, one of `authentication_failed`, `oauth_org_not_allowed`, `account_on_hold`, `billing_error`, `rate_limit`, `overloaded`, `invalid_request`, `model_not_found`, `server_error`, `max_output_tokens`, `cloud_credential_error`, `unknown` ([headless: handle API retries](https://code.claude.com/docs/en/headless#handle-api-retries)). The same category set appears as the optional `error` field on `assistant` messages. This is the documented, typed signal for "credential died" (`authentication_failed`) versus "out of quota" (`rate_limit`, a 429) versus "server busy" (`overloaded`, a 529).

**`system` / `permission_denied`**: a denied tool call as it happens; best-effort, with `permission_denials` on the result as the record.

## Exit codes

What is documented:

- "Claude Code exits with code 0 on success and a non-zero code when the run fails ... If you pass an invalid flag, Claude Code reports the error to stderr before the run starts. When a failure happens inside the run, such as missing authentication, Claude Code prints the failure as the result on stdout." ([headless: basic usage](https://code.claude.com/docs/en/headless#basic-usage))
- SIGTERM: "Claude Code exits with code 143", leaves the turn unfinished "and records no result for it". SIGINT ends the turn instead. On SIGTERM it kills the process tree of a running Bash command and runs `SessionEnd` hooks ([headless: stop a run with SIGTERM](https://code.claude.com/docs/en/headless#stop-a-run-with-sigterm)).
- `--max-turns`: "Exits with an error when the limit is reached" ([CLI reference](https://code.claude.com/docs/en/cli-reference#cli-flags)).
- Piped stdin over 10MB: "exits with a clear error and a non-zero status".

No published table maps specific non-zero codes to causes. The SDKs themselves only distinguish "exited non-zero with an error result" from "exited non-zero with none" ([SDK troubleshooting: CLI process exit](https://code.claude.com/docs/en/agent-sdk/troubleshooting#cli-process-exit)). A Go caller should do the same: three cases, zero / non-zero with a result message / non-zero or signal with none, and read the cause from the result.

## Sessions and resume

- `--resume <session-id>` continues a specific session; the ID comes from `session_id` on the result or the init event. `--continue` takes the most recent session in the directory; with `-p` that includes other `-p` sessions, so a daemon should always resume by ID ([headless: continue conversations](https://code.claude.com/docs/en/headless#continue-conversations), [CLI reference](https://code.claude.com/docs/en/cli-reference#cli-flags)).
- "You can run the two commands from different directories: Claude Code finds the session by its ID in any project on this machine." Before v2.1.223 the ID search covered only the current project directory and its worktrees. `--resume` also accepts the absolute path of a session's `.jsonl` transcript.
- `--session-id <uuid>` lets the caller choose the ID up front. `--fork-session` resumes into a new ID. `--no-session-persistence` (print mode only) writes nothing, so nothing can be resumed.
- After a SIGTERM, a resumed session "leaves the interrupted turn as it is, and your next prompt drives the conversation"; `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` makes it continue the interrupted turn instead.
- The system prompt is recorded on the first request and reused on resume until compaction, even if later launches pass different `--append-system-prompt` text; `--system-prompt-snapshot off` disables that ([CLI reference: system prompt flags in resumed conversations](https://code.claude.com/docs/en/cli-reference#system-prompt-flags-in-resumed-conversations)). A step runner that changes per-step instructions across a resumed session must put them in the prompt, or turn the snapshot off.
- `total_cost_usd` on a resumed run "reports the conversation's whole total, earlier runs' spend included", while `--max-budget-usd` does not count restored totals.

Sessions are local files on one machine. Resume is a convenience for carrying context between steps, not durable state: the Go program's own record of the step is what survives.

## Permission and tool restriction

All confirmed in `claude --help` 2.1.296 unless marked.

| Flag | Effect | Source |
| - | - | - |
| `--permission-mode <mode>` | `default`/`manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions` | [CLI reference](https://code.claude.com/docs/en/cli-reference#cli-flags) |
| `--allowedTools` | Tools that run without prompting, in permission rule syntax (`Bash(git diff *)`). Does not restrict availability | [headless: auto-approve tools](https://code.claude.com/docs/en/headless#auto-approve-tools) |
| `--disallowedTools` | Deny rules. A bare name removes the tool from context; a scoped rule denies matching calls | CLI reference |
| `--tools` | Restricts which built-in tools exist (`""` for none). Does not affect MCP tools; use `--disallowedTools "mcp__*"` for those | CLI reference |
| `--permission-prompts none` | Nobody answers prompts: anything that would prompt is denied and Claude is told not to retry. Needs v2.1.259 | [headless](https://code.claude.com/docs/en/headless#turn-off-permission-prompts-in-unattended-runs) |
| `--permission-prompt-tool` | An MCP tool answers prompts (documented; not listed in local `--help`) | CLI reference |
| `--max-turns` | Caps agentic turns; error exit at the limit (documented; not listed in local `--help`) | CLI reference |
| `--max-budget-usd` | Stops at an estimated spend; "spend can pass the cap" | CLI reference |
| `--restricted` | Removes command-running tools and WebFetch unless named in `--tools`, confines file tools to working directories, ignores user/project/local settings, refuses `bypassPermissions`. Needs v2.1.248 | CLI reference |
| `--strict-mcp-config` | Only MCP servers from `--mcp-config` | CLI reference |

The CLI reference notes that "`claude --help` does not list every flag, so a flag's absence from `--help` does not mean it is unavailable", which covers the two rows marked above.

Points that matter for an unattended step:

- **Always pass a permission mode.** A `-p` run where nothing sets one "takes the built-in starting permission mode, which can be `auto`" ([headless](https://code.claude.com/docs/en/headless#auto-approve-tools), [permission modes](https://code.claude.com/docs/en/permission-modes#which-mode-a-session-starts-in)).
- **`dontAsk` plus an allow-list is the locked-down shape**: "Claude Code denies every call that would otherwise prompt"; reads in the working directory, the read-only command set and anything `--allowedTools` or `permissions.allow` covers still run.
- **`bypassPermissions`** (what `lib/fire.sh` uses today) skips prompts including writes to protected paths, but "the actions no mode auto-approves still prompt", and in a `-p` run those few calls are denied. On Linux and macOS Claude Code "refuses to start in this mode when running as root or under `sudo`". The docs' own row for this says a container, VM or sandbox is required ([permission modes](https://code.claude.com/docs/en/permission-modes#skip-all-checks-with-bypasspermissions-mode)).
- **A denial does not fail the run.** The call is refused, Claude keeps working, exit can still be 0. Go must read `permission_denials` and decide whether a step that was denied something counts as done.
- **`-p` shows no workspace trust dialog.** Without `--bare` it "runs the hooks in a project's `.claude/settings.json` and connects the servers in its `.mcp.json`, even in a folder you've never trusted". The Target checkout's own settings therefore run inside every step unless excluded with `--setting-sources`, `--strict-mcp-config`, `--restricted` or `--bare`.

## Loading skills and agents from a caller-supplied directory

- **`--plugin-dir <path>`** loads a plugin "from a directory or `.zip` archive ... for this session only", repeatable; a folder of plugins loads each child from v2.1.265 ([CLI reference](https://code.claude.com/docs/en/cli-reference#cli-flags)). This is the mechanism ADR 0001 already chose.
- **Plugin layout**: `skills/<name>/SKILL.md`, `agents/*.md`, `commands/*.md`, `hooks/hooks.json`, `.mcp.json`, and an optional `.claude-plugin/plugin.json` ("components auto-discovered without it") ([SDK plugins](https://code.claude.com/docs/en/agent-sdk/plugins#plugin-structure-reference)).
- **Invoking**: plugin skills are namespaced; "send `/plugin-name:skill-name` as the prompt". In `-p` mode, "user-invoked skills and custom commands work. Include `/skill-name` in the prompt string and Claude Code expands it before running" ([headless](https://code.claude.com/docs/en/headless#create-a-commit)).
- **Verifying**: a missing or broken plugin path does not fail the run. "If a plugin path doesn't exist, the SDK skips that plugin and the session continues, so check the `plugins` list in the init message." Failures appear in `plugin_errors` on `system/init`, with the resolved `path` from v2.1.283. This is the check `lib/fire.sh` already makes (`plugin.loaded`, `plugin.skillListed`) and the Go caller must keep.
- **`--agents <json-or-file>`** defines subagents inline, or from a JSON file path with `--print` (v2.1.281); validated at startup, exiting on an invalid value (v2.1.242). `--agent <name>` runs the session as a named agent.
- **`--settings <file-or-json>`** layers settings over the file-based ones for the session ("values you set here override the same keys ... keys you omit keep their file-based values"); `--setting-sources user,project,local` picks which files load at all.
- **`--add-dir`** grants file access and does *not* load most `.claude/` configuration from the directory; skills in its `.claude/skills/` are the exception. It is not a substitute for `--plugin-dir`.
- **`--bare`** skips auto-discovery of hooks, skills, commands, subagents, installed plugins, MCP servers, auto memory and CLAUDE.md, while still honouring `--plugin-dir`, `--agents`, `--settings`, `--mcp-config` and the system-prompt flags. The docs call it "the recommended mode for scripted and SDK calls" and say it "will become the default for `-p` in a future release". **But bare mode "never reads OAuth credentials or the system keychain"**: it needs `ANTHROPIC_API_KEY` or an `apiKeyHelper`, so it is unusable with the subscription login auto-agent runs on today. In bare mode Claude also "doesn't get the list of available skills", so a skill must be invoked by name.

That last announcement is a planning risk: if `--bare` becomes the default for `-p`, a subscription-login daemon will need whatever opt-out ships with that change. Nothing in the docs names one yet.

## What a Go caller can rely on, and what it must enforce

Rely on (documented):

1. One `result` message, last on stdout, in `json` and `stream-json`, with `subtype`, `is_error`, `session_id` and cost fields.
2. With `--json-schema`: a value in `structured_output` has been validated against the schema by Claude Code (draft-07, `format` unenforced), after retries.
3. An invalid schema or invalid `--agents` value fails at startup (v2.1.205 / v2.1.242).
4. Exit 0 means the run did not fail; exit 143 means SIGTERM and no result.
5. `system/init` lists what loaded: `plugins`, `plugin_errors`, `skills`, `agents`, `mcp_servers`, `model`, `permissionMode`.
6. `--resume <id>` continues a session by ID from any directory on the same machine (v2.1.223).
7. Tool availability and approval are set per invocation by flags, and denials are reported in `permission_denials`.

Enforce itself:

1. **Absent `structured_output` on `success` is a failure.** So is `is_error: true` whatever the subtype.
2. **Re-validate in Go.** Unmarshal into the step's struct with unknown fields rejected and required fields checked; do not trust the CLI's validator for `format`, cross-field rules, or anything semantic.
3. **Verify claims against the world**: the branch, commit, PR or label the result names.
4. **Classify failures from the result and stream**, not the exit code: `subtype`, `errors`, `api_error_status`, `terminal_reason`, the last `rate_limit_event`, and `api_retry.error`. The out-of-budget / dead-credential / real-failure split that `lib/exhaustion-classifier.sh` makes has no single documented field; `rate_limit_info.status == "rejected"` with `resetsAt` and `error == "authentication_failed"` are the typed inputs, and the human-readable limit strings ("You've hit your session limit · resets 3:45pm", [error reference](https://code.claude.com/docs/en/errors)) remain a fallback.
5. **Bound the step in time.** No wall-clock flag is documented. Use `--max-turns` and `--max-budget-usd` as soft bounds and a Go context deadline as the hard one; send SIGINT first if the turn should be recorded, then SIGTERM (143, no result). Note a `-p` run can stay open waiting on background work for up to 10 minutes of idle by default (`CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS`).
6. **Confirm the load.** Fail the step if the plugin is missing from `init.plugins`, `plugin_errors` is non-empty, or the skill is not in `init.skills`.
7. **Decide what a permission denial means** for the step; the CLI will not fail the run for it.
8. **Tolerate schema drift in the stream.** Ignore unknown event types and unknown fields, treat optional fields as optional, and pin and record `claude_code_version`. Several behaviours above changed inside the 2.1.x line.
9. **Isolate the configuration.** Pass the permission mode, `--plugin-dir`, `--settings` and (if wanted) `--setting-sources` / `--strict-mcp-config` explicitly on every call, since `-p` otherwise loads whatever the Target checkout and `~/.claude` hold, without a trust prompt.
10. **Close stdin** (`< /dev/null`, as `lib/fire.sh` does) or feed the prompt through it deliberately; `-p` reads stdin.

## Suggested invocation shape

A sketch assembled from the documented flags above; not run.

```
claude -p "/auto-agent:<step-skill> <args>" \
  --output-format stream-json --verbose \
  --json-schema "$(cat step-result.schema.json)" \
  --permission-mode <mode> --permission-prompts none \
  --plugin-dir <harness-plugin> --settings <baseline.json> \
  [--resume <session-id>] [--max-turns N] [--model M] < /dev/null
```

`stream-json` rather than `json` because the rate-limit and init events are only in the stream.

## Not verified

- **Nothing was executed.** No `claude -p` prompt was run (the Host shares its usage budget with a live Daemon), so every behaviour here is as documented, not as observed. In particular the interaction of `--json-schema` with a `/plugin:skill` prompt, and with a forked skill, is not documented either way and needs a live probe.
- **`--json-schema` with `stream-json`.** The docs' examples pair `--json-schema` with `--output-format json`. That `structured_output` also appears on the stream's final `result` line follows from the shared `SDKResultMessage` type, but no doc example shows that combination.
- **The structured-output retry limit** is not published.
- **Specific non-zero exit codes** other than 143 are not published; the claim that `--max-turns` exits non-zero rests on "exits with an error".
- **How the structured output is produced internally** (a synthetic tool versus constrained decoding) is not stated in the docs. The docs promise validation and re-prompting, not grammar-constrained generation.
- **`rate_limit_event` cadence**, and the fields `rateLimitType`, `unifiedWindows`, `isUsingOverage` and `overageStatus` that this repo's tap reads, are outside the published type.
- **Whether `--verbose` is strictly required** with `--output-format stream-json` in `-p` mode: the docs always show the pair but the text read does not state the requirement.
- **The error reference was read only in part** (the page is very large): its index of messages and the usage-limit strings, not the bodies of the authentication and "process exited with code N" sections. The permission-modes page was read by targeted passages, not end to end.
- **The absence of a Go Agent SDK** rests on the official overview naming only Python and TypeScript and directing other languages to the CLI. Community Go wrappers may exist; none was looked for, and none is official.

## Sources

- Run Claude Code programmatically (headless): https://code.claude.com/docs/en/headless
- CLI reference: https://code.claude.com/docs/en/cli-reference
- Agent SDK overview: https://code.claude.com/docs/en/agent-sdk/overview
- Agent SDK structured outputs: https://code.claude.com/docs/en/agent-sdk/structured-outputs
- Agent SDK TypeScript reference (message types): https://code.claude.com/docs/en/agent-sdk/typescript
- Agent SDK troubleshooting: https://code.claude.com/docs/en/agent-sdk/troubleshooting
- Agent SDK plugins: https://code.claude.com/docs/en/agent-sdk/plugins
- Permission modes: https://code.claude.com/docs/en/permission-modes
- Error reference: https://code.claude.com/docs/en/errors
- `claude --help`, Claude Code 2.1.296, on the Host
- This repo: `lib/fire.sh`, `lib/rate-limits-tap.sh`, `lib/exhaustion-classifier.sh`, `docs/adr/0001-harness-install-loaded-as-plugin.md`
