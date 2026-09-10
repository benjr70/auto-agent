# Research: Claude auth modes for a headless Host and their effect on the usage gate

Ticket: [#13](https://github.com/benjr70/auto-agent/issues/13) (part of map
[#1](https://github.com/benjr70/auto-agent/issues/1)). Researched 2026-09-10
against primary sources only; local Claude Code was 2.1.267. The current
Smart-Smoker Host runs `claude --print` under a Max subscription logged in
with `/login`, and `lib/usage-sensor.sh` gates fires on
`https://api.anthropic.com/api/oauth/usage`.

Sources (all fetched 2026-09-10):

- [Authentication](https://code.claude.com/docs/en/authentication) (Claude Code docs)
- [Run Claude Code programmatically / headless](https://code.claude.com/docs/en/headless)
- [Environment variables](https://code.claude.com/docs/en/env-vars)
- [Settings reference](https://code.claude.com/docs/en/settings-reference) and [Settings files and precedence](https://code.claude.com/docs/en/settings)
- [CLI reference](https://code.claude.com/docs/en/cli-reference)
- [Manage costs effectively](https://code.claude.com/docs/en/costs)
- [Errors](https://code.claude.com/docs/en/errors) and [Troubleshooting install](https://code.claude.com/docs/en/troubleshoot-install)
- [Interactive mode](https://code.claude.com/docs/en/interactive-mode) and [Status line](https://code.claude.com/docs/en/statusline)
- [Amazon Bedrock](https://code.claude.com/docs/en/amazon-bedrock), [Google Cloud's Agent Platform (Vertex)](https://code.claude.com/docs/en/google-vertex-ai), [Microsoft Foundry](https://code.claude.com/docs/en/microsoft-foundry)
- [GitHub Actions](https://code.claude.com/docs/en/github-actions)
- [Claude Code CHANGELOG](https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md)
- Claude API docs: [Rate limits](https://platform.claude.com/docs/en/api/rate-limits), [Workspaces](https://platform.claude.com/docs/en/manage-claude/workspaces), [Usage and Cost API](https://platform.claude.com/docs/en/manage-claude/usage-cost-api), [Claude Code Analytics API](https://platform.claude.com/docs/en/manage-claude/claude-code-analytics-api), [`ant` CLI authentication](https://platform.claude.com/docs/en/cli-sdks-libraries/cli/authentication)
- Support: [Use Claude Code with your Pro or Max plan](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan), [What is the Max plan?](https://support.claude.com/en/articles/11049741-what-is-the-max-plan), [Manage usage credits for paid plans](https://support.claude.com/en/articles/12429409-extra-usage-for-paid-claude-plans), [How do usage and length limits work?](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work), [Models, usage, and limits in Claude Code](https://support.claude.com/en/articles/14552983-models-usage-and-limits-in-claude-code)
- [Anthropic Consumer Terms](https://www.anthropic.com/legal/consumer-terms)
- Read-only local: `Smart-Smoker-V2/scripts/claude-agent/lib/usage-sensor.sh`, `agent-daemon`

## TL;DR

- `claude --print` works with every auth mode; headless login for a
  subscription is documented two ways: `claude auth login` (prints the URL,
  reads the pasted code from stdin) or `claude setup-token` run anywhere with
  a browser and exported on the Host as `CLAUDE_CODE_OAUTH_TOKEN` (one-year
  token, no refresh). Copying `~/.claude/.credentials.json` between machines
  is **not documented** as supported.
- The `/api/oauth/usage` endpoint the Usage Sensor reads is **undocumented**:
  it appears in no code.claude.com or platform.claude.com page (index and
  pages searched). The only documented subscription budget signals are the
  status-line `rate_limits` object (`five_hour`/`seven_day` ×
  `used_percentage`/`resets_at`) and the "You've hit your session/weekly
  limit · resets …" errors.
- An API key has no session window; "budget" becomes the org's monthly spend
  cap / self-set spend limit (429 with no `retry-after`, or 400) plus
  per-minute rate limits with `anthropic-ratelimit-*` headers, and spend is
  observable via `--output-format json` `total_cost_usd`, `--max-budget-usd`,
  and (organizations only) the Usage & Cost Admin API. Bedrock/Vertex/Foundry
  move budget entirely to the cloud billing console.
- Several Hosts on one subscription draw one pool: limits are per account and
  "shared across Claude and Claude Code". The Consumer Terms forbid sharing
  credentials with anyone else and forbid automated access "except … via an
  Anthropic API Key or where we otherwise explicitly permit it"; the
  `setup-token` docs explicitly permit CI/scripts on Pro/Max/Team/Enterprise.
- Precedence (documented): cloud provider → `ANTHROPIC_AUTH_TOKEN` →
  `ANTHROPIC_API_KEY` → `apiKeyHelper` → `CLAUDE_CODE_OAUTH_TOKEN` →
  Anthropic profile → `/login`. In `-p` an API key is "always used when
  present"; `--bare` ignores OAuth entirely.

## 1. Auth modes × headless concerns

| Mode | Headless login procedure | Credential storage / refresh | Documented "budget" signal | Multi-Host sharing | Cost model |
|---|---|---|---|---|---|
| **Subscription OAuth via `/login`** (Pro/Max/Team/Enterprise) | Browser flow; when the callback can't reach localhost (SSH, containers) the browser shows a code to paste at `Paste code here if prompted`. Non-TUI variant: `claude auth login` reads the pasted code from stdin ([troubleshoot](https://code.claude.com/docs/en/troubleshoot-install#oauth-login-fails-in-wsl2-ssh-or-containers), CHANGELOG 2.1.126). `/login` is not available in `-p` ([headless](https://code.claude.com/docs/en/headless)). | Linux: `~/.claude/.credentials.json` mode 0600 (or under `CLAUDE_CONFIG_DIR`); macOS keychain. Managed only through `/login`/`/logout`. Auto-refreshes; login has a finite lifetime (length **not documented**); startup warning 3 days before expiry (2.1.203+). When refresh is rejected, creds are cleared and `-p` fails with `Failed to authenticate: OAuth session expired and could not be refreshed`, code `authentication_failed` ([errors](https://code.claude.com/docs/en/errors#login-expired)). Parallel sessions on one machine share the login and coordinate refresh with a lock (2.1.211). | Status-line `rate_limits.{five_hour,seven_day}.{used_percentage,resets_at}` (Pro/Max only, after first API response) ([statusline](https://code.claude.com/docs/en/statusline#rate-limit-usage)); `/usage` bars (interactive); limit errors name the reset time. `/api/oauth/usage`: **not documented**. | One account = one pool "shared across Claude and Claude Code" ([support](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan)). Multiple machines not addressed; sharing creds with another person prohibited (Consumer Terms §2). | Flat subscription; 5-hour rolling window + weekly limit (fixed weekly reset per account) + per-model (Opus/Sonnet/Fable) limits; optional usage credits at standard API rates with a monthly spend cap ([Max plan](https://support.claude.com/en/articles/11049741-what-is-the-max-plan), [usage credits](https://support.claude.com/en/articles/12429409-extra-usage-for-paid-claude-plans), [errors](https://code.claude.com/docs/en/errors#youve-hit-your-session-limit)). |
| **Long-lived OAuth token via `claude setup-token`** → `CLAUDE_CODE_OAUTH_TOKEN` | Run `claude setup-token` on any machine with a browser (same flow as `/login`); it prints a one-year token and saves nothing; export it on the Host ([authentication](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token)). | Wherever you put the env var (shell, systemd env, settings `env` block). **No refresh**: on expiry/revocation requests 401 `OAuth token has expired`; "generate a new one and restart" ([env-vars](https://code.claude.com/docs/en/env-vars), [errors](https://code.claude.com/docs/en/errors#oauth-token-revoked-or-expired)). Since 2.1.225 a transient 401 no longer swaps it for a stored login's short-lived token. `--bare` does not read it. Can only make model requests (no Remote Control / claude.ai connectors). | Same as `/login` (it "authenticates with your Claude subscription"); the `rate_limits` status-line fields are documented for Pro/Max subscribers, provider not method. Whether `/api/oauth/usage` accepts this token: **not documented**. | "Tied to the subscription of the person who ran `claude setup-token`" ([GitHub Actions](https://code.claude.com/docs/en/github-actions)); same single pool. Explicitly documented for CI/scripts on Pro/Max/Team/Enterprise. | Same subscription cost model. |
| **API key** (`ANTHROPIC_API_KEY`, Console billing) | Export the env var; no browser. In `-p` "the key is always used when present"; interactive mode asks once ([authentication](https://code.claude.com/docs/en/authentication#authentication-precedence)). Console sign-in with `claude auth login --console` also exists (mints a per-user key in the auto-created "Claude Code" workspace, or since 2.1.242 a keyless Console OAuth profile). | Static; never expires unless rotated/revoked in Console. `apiKeyHelper` (rerun every 5 min or `CLAUDE_CODE_API_KEY_HELPER_TTL_MS`, on 401/403, or on JWT expiry) for rotating creds; output sent as both `X-Api-Key` and `Authorization: Bearer` ([settings-reference](https://code.claude.com/docs/en/settings-reference#apikeyhelper)). | No session/weekly window. Per-request `anthropic-ratelimit-{requests,tokens}-{limit,remaining,reset}` headers; 429 `rate_limit_error` at RPM/ITPM/OTPM; monthly tier spend cap → 429 **without** `retry-after` until 00:00 UTC on the 1st; self-set spend limit → 400 `invalid_request_error`; Claude Code workspace limit → 429 with `retry-after` ([rate limits](https://platform.claude.com/docs/en/api/rate-limits)). Client-side: `--output-format json` `total_cost_usd`, `--max-budget-usd` (print mode, stops the run). Org-level: Usage & Cost Admin API (`1m` buckets, ~5 min lag) and Claude Code Analytics API — Admin API keys only, "unavailable for individual accounts". | Any number of Hosts; each can have its own key / workspace; per-user monthly spend limits exist only in the Claude Code workspace ([workspaces](https://platform.claude.com/docs/en/manage-claude/workspaces#claude-code-workspace)). | Pay per token at list price; prompt-cache TTL 5 min by default; `/usage-credits` and subscription features unavailable. |
| **Amazon Bedrock** (`CLAUDE_CODE_USE_BEDROCK=1`) | No browser: AWS default credential chain, `AWS_BEARER_TOKEN_BEDROCK` (Bedrock API key), or `/setup-bedrock` wizard writing the `env` block ([Bedrock](https://code.claude.com/docs/en/amazon-bedrock)). Works in `--bare`. | AWS chain resolved once and cached until 5 min before expiry (1 h if none); `awsAuthRefresh` (e.g. `aws sso login`) runs only after an STS check fails; `awsCredentialExport` for process-supplied creds. `/logout` unavailable. | None from Anthropic: "spend controls live in your cloud provider's billing console"; Anthropic analytics APIs don't cover it; per-user attribution via OpenTelemetry or a gateway ([costs](https://code.claude.com/docs/en/costs#cloud-providers)). Bedrock throttling surfaces as `cloud_credential_error`/`rate_limit` retry events. | Per AWS account/role; unlimited Hosts. | Per token on the AWS bill; AWS Budgets for caps. |
| **Google Vertex / Agent Platform** (`CLAUDE_CODE_USE_VERTEX=1`) | No browser: Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS`, WIF supported) + `ANTHROPIC_VERTEX_PROJECT_ID` ([Vertex](https://code.claude.com/docs/en/google-vertex-ai)). | ADC; `gcpAuthRefresh` runs a command (e.g. `gcloud auth application-default login`) only after a token check fails; 3-min refresh timeout; cannot receive interactive input. | None from Anthropic; GCP billing/budgets. | Per GCP project. | Per token on the GCP bill. |
| **Microsoft Foundry** (`CLAUDE_CODE_USE_FOUNDRY=1`) | No browser: `ANTHROPIC_FOUNDRY_API_KEY`, `ANTHROPIC_FOUNDRY_AUTH_TOKEN` (Entra bearer), or Azure default credential chain ([Foundry](https://code.claude.com/docs/en/microsoft-foundry)). | Static key, or a bearer token you refresh yourself, or the Azure chain. `/logout` unavailable. | None from Anthropic; Azure cost management. | Per Azure resource. | Standard API rates billed through Microsoft Marketplace. |

Not in scope but documented for completeness: a self-hosted **Claude apps
gateway** (`forceLoginMethod: "gateway"`) outranks every source above and
adds `rate_limits.spend_limit` to the status line (2.1.251+) — the only
documented *dollar* budget signal in the CLI ([statusline](https://code.claude.com/docs/en/statusline#rate-limit-usage)).

## 2. Sub-questions from the ticket

### 2.1 How can OAuth login complete on a machine without a browser?

Three documented paths; none is a device-code flow in the RFC 8628 sense.

1. **Paste-code fallback of `/login`.** "If your browser shows a login code
   instead of redirecting back after you sign in, paste it into the terminal
   at the `Paste code here if prompted` prompt. This happens when the browser
   can't reach Claude Code's local callback server, which is common in WSL2,
   SSH sessions, and containers." Press `c` to copy the URL when no browser
   opens ([authentication](https://code.claude.com/docs/en/authentication#log-in-to-claude-code)).
2. **`claude auth login`** — the non-TUI subcommand: it prints the sign-in
   URL (emitted as a single hyperlink since 2.1.202) and "reads the pasted
   code from standard input" ([troubleshoot](https://code.claude.com/docs/en/troubleshoot-install#oauth-login-fails-in-wsl2-ssh-or-containers), [CLI reference](https://code.claude.com/docs/en/cli-reference)). Flags: `--email`, `--sso`, `--console`. Pair with `claude auth status`
   (JSON, exit 0 logged in / 1 not; includes `authMethod`, `subscriptionType`,
   `orgId`) — verified locally on 2.1.267.
3. **`claude setup-token`** on a browser machine, then
   `CLAUDE_CODE_OAUTH_TOKEN` on the Host. This is the path the docs recommend
   "for CI pipelines, scripts, or other environments where interactive browser
   login isn't available" ([authentication](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token)).

**Copying `~/.claude/.credentials.json`**: the docs only say Claude Code
"manages `.credentials.json` through `/login` and `/logout`", that the file is
keyed to `CLAUDE_CONFIG_DIR`, and that on macOS it lives in the keychain
unless the keychain is locked. Transplanting it is **not documented**. The
one documented refresh-coordination fact is per machine: "Parallel sessions on
one machine share a saved login and coordinate its renewal so that only one
process refreshes the token at a time. Before v2.1.211, waking the machine
from sleep could cause two sessions to renew with the same token, which
revoked the saved login" ([troubleshoot](https://code.claude.com/docs/en/troubleshoot-install#not-logged-in-or-token-expired)). Inference (not documented): two Hosts holding a copy of the same refresh token cannot share that lock, so the pre-2.1.211 double-refresh revocation is the expected failure mode of copying the file.

### 2.2 Credential refresh behaviour and expiry

- **`/login` credential**: auto-refreshed by the CLI; the *login* itself has a
  lifetime (number **not documented**). From 2.1.203 a startup warning fires
  3 days before expiry (5 days before 2.1.217); it "never blocks a request".
  Once the refresh token is rejected, "Claude Code cleared the saved
  credentials" and every request "stops locally … before it reaches the API";
  in `-p` the result is `Failed to authenticate: OAuth session expired and
  could not be refreshed` with structured code `authentication_failed`; "Sessions
  authenticated with an API key, `CLAUDE_CODE_OAUTH_TOKEN`, or a third-party
  provider … never see this message" ([errors](https://code.claude.com/docs/en/errors#login-expired)). "Renewing early matters most for sessions that run unattended" ([authentication](https://code.claude.com/docs/en/authentication#renew-an-expiring-login)). A 401 mid-session reads `OAuth token has expired · Please run /login` and means "the automatic refresh failed mid-session". Clock skew is a documented cause of repeated expiry. The access-token TTL is **not documented** (the CHANGELOG mentions a cache miss "roughly once an hour … after an OAuth token refresh", 2.1.248, which is the only hint).
- **`CLAUDE_CODE_OAUTH_TOKEN`**: one year, no refresh; replace and restart.
  Since 2.1.225 a transient 401 no longer swaps it for a stored login's
  short-lived token, which previously "[broke] headless sessions until
  restart" (CHANGELOG 2.1.225).
- **API key**: no expiry; `apiKeyHelper` re-run every 5 min / on 401-403 /
  on JWT expiry.
- **Cloud providers**: cached until 5 min before expiry; `awsAuthRefresh` /
  `gcpAuthRefresh` hooks; both can print a URL but cannot take input.

The "expiry put the daemon to sleep for 5h" incident is explained by the
sensor's design, not by anything documented: `usage_sensor_fetch` exits 1 when
the creds file has no `claudeAiOauth.accessToken`, the daemon falls back to
`ccusage`, then to `degraded`, which never fires and sleeps a full
`WINDOW_SECS` (18000 s) (`agent-daemon` `read_budget`/`degraded_reset_iso`).
An expired login and an exhausted budget are therefore indistinguishable to
the daemon today, although Claude Code itself distinguishes them
(`authentication_failed` vs `rate_limit` in the `api_retry` event and the
`-p` result text).

### 2.3 Is an API key / Bedrock / Vertex supported for `claude --print`, and what does "budget" mean then?

Yes, all of them. The headless page's own bare-mode example says "Set
`ANTHROPIC_API_KEY` before running it, because bare mode doesn't use your
subscription login", and "Amazon Bedrock, Google Cloud's Agent Platform, and
Microsoft Foundry continue to read their own provider credentials as usual"
([headless](https://code.claude.com/docs/en/headless#start-faster-with-bare-mode)). The env-vars table adds: "In non-interactive mode (`-p`), the key is always used when present" ([env-vars](https://code.claude.com/docs/en/env-vars)).

With an API key there is no 5-hour or weekly window. The ceilings are:

| Ceiling | Trigger | Signal |
|---|---|---|
| Per-minute rate limits (RPM / ITPM / OTPM per model class, plus acceleration limits) | Burst | 429 `rate_limit_error` with `retry-after`; every response carries `anthropic-ratelimit-requests-*` and `-tokens-*` `limit/remaining/reset` headers ([rate limits](https://platform.claude.com/docs/en/api/rate-limits#response-headers)) |
| Tier monthly spend cap | Calendar month | 429 `rate_limit_error`, **no `retry-after`**, "You will regain access on <1st of month> 00:00 UTC"; SDK retries fail |
| Self-set org spend limit | Month | 400 `invalid_request_error` "You have reached your specified API usage limits" |
| Workspace spend / rate limit | Month / minute | 400 "…workspace API usage limits", or for the Claude Code workspace a 429 with `retry-after` |
| `--max-budget-usd` | Per `claude -p` run | Run stops; subagent spawn fails with `Budget limit reached` (2.1.217+) ([CLI reference](https://code.claude.com/docs/en/cli-reference)) |

Observability: `--output-format json` returns `total_cost_usd` and a
per-model breakdown (client-side estimate); the Usage & Cost Admin API
(`/v1/organizations/usage_report/messages`, `1m`/`1h`/`1d` buckets, "typically
appears within 5 minutes") and the Claude Code Analytics API
(`/v1/organizations/usage_report/claude_code`, daily) need an Admin API key,
and "The Admin API is unavailable for individual accounts" ([Usage and Cost API](https://platform.claude.com/docs/en/manage-claude/usage-cost-api), [Claude Code Analytics API](https://platform.claude.com/docs/en/manage-claude/claude-code-analytics-api)). Console-login Claude Code traffic lands in an auto-created "Claude Code" workspace that "is the only workspace that supports per-user monthly spend limits" ([workspaces](https://platform.claude.com/docs/en/manage-claude/workspaces#claude-code-workspace)).

On Bedrock/Vertex/Foundry, "Claude Code is billed per token to your cloud
account, and spend controls live in your cloud provider's billing console.
Claude Code does not send metrics from your cloud back to Anthropic", so the
analytics dashboards and Analytics API "do not cover this usage"; per-user
attribution is OpenTelemetry, a Claude apps gateway, or an LLM gateway
([costs](https://code.claude.com/docs/en/costs#cloud-providers)).

### 2.4 Do several Hosts sharing one OAuth account share limits?

- Limits are per account, not per device: "Both Pro and Max plans offer usage
  limits that are shared across Claude and Claude Code, meaning all activity
  in both tools counts against the same usage limits", and "IDE usage counts
  toward the same usage limits" ([support](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan)). A `setup-token` "is tied to the subscription of the person who ran `claude setup-token`" ([GitHub Actions](https://code.claude.com/docs/en/github-actions#set-up-for-an-organization)). Multiple machines per account are **not documented** either way; the only reasonable reading is one pool per account, so N Hosts share one 5-hour and one weekly window.
- The weekly window "resets at a fixed time each week that is assigned to
  your account"; the session window "will reset every five hours"; per-model
  Opus/Sonnet limits exist, and the session and weekly limits are "shared
  across all models, so switching models doesn't restore access" ([Max plan](https://support.claude.com/en/articles/11049741-what-is-the-max-plan), [errors](https://code.claude.com/docs/en/errors#youve-hit-your-session-limit)). "Usage counts against the session and weekly allowances at the same time. A single burst of heavy activity, such as a large workflow fanout, can exhaust the weekly allowance before the session window resets."
- Terms: "You may not share your Account login information, Anthropic API
  key, or Account credentials with anyone else … You also may not make your
  Account available to anyone else" (Consumer Terms §2); automated access is
  prohibited "[e]xcept when you are accessing our Services via an Anthropic
  API Key or where we otherwise explicitly permit it" (§3) ([Consumer Terms](https://www.anthropic.com/legal/consumer-terms)). The Claude Code docs explicitly permit subscription tokens in "CI pipelines, scripts" and GitHub Actions on Pro/Max/Team/Enterprise, which is the "otherwise explicitly permit" carve-out a Daemon relies on. One person's several Hosts is not sharing with "anyone else"; a shared team Host under one personal login is.
- Team/Enterprise seats: usage "draws from a per-seat allowance that resets
  on a rolling five-hour window and a weekly window", capped per org/group/
  member via usage credits ([costs](https://code.claude.com/docs/en/costs#claude-for-teams-and-enterprise)).

### 2.5 `CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY` precedence

Documented order, first match wins ([authentication](https://code.claude.com/docs/en/authentication#authentication-precedence)):

1. Cloud provider creds when `CLAUDE_CODE_USE_BEDROCK` / `_VERTEX` / `_FOUNDRY` is set
2. `ANTHROPIC_AUTH_TOKEN` (sent as `Authorization: Bearer`)
3. `ANTHROPIC_API_KEY` (`X-Api-Key`; in `-p` always used, interactive asks once)
4. `apiKeyHelper` output
5. `CLAUDE_CODE_OAUTH_TOKEN` ("Takes precedence over keychain-stored credentials")
6. Anthropic profile / WIF (`ANTHROPIC_PROFILE`, federation vars)
7. `/login` subscription OAuth

Corollaries that matter for a Host:

- An exported `ANTHROPIC_API_KEY` silently moves a subscription Host onto
  Console billing in `-p`; `/status` marks the unused credential.
- `--bare` "never reads OAuth credentials or the system keychain" and "does
  not read `CLAUDE_CODE_OAUTH_TOKEN`" — a subscription Host cannot use bare
  mode, and the docs say bare "will become the default for `-p` in a future
  release" ([headless](https://code.claude.com/docs/en/headless#start-faster-with-bare-mode)).
- A settings-file `env` block overrides the shell: "When the same variable is
  set in both your shell and a settings file `env` block, the settings file
  value applies" ([env-vars](https://code.claude.com/docs/en/env-vars#precedence)); `CLAUDE_CONFIG_DIR` is ignored in project/local settings and lets one Host run several accounts side by side.
- `forceLoginOrgUUID` in managed settings blocks `ANTHROPIC_API_KEY` /
  `ANTHROPIC_AUTH_TOKEN` / `apiKeyHelper` sessions at startup but not cloud
  providers or profiles.

### 2.6 Where the current Usage Sensor stands against the docs

- `USAGE_API_URL=https://api.anthropic.com/api/oauth/usage` with header
  `anthropic-beta: oauth-2025-04-20`: **not documented**. Searched the full
  code.claude.com and platform.claude.com `llms.txt` indexes and every page
  above; the only public acknowledgement is indirect — the costs page says
  `/usage` degrades "when the usage endpoint is rate limited" and shows
  last-known bars up to 60 minutes old ([costs](https://code.claude.com/docs/en/costs#when-the-usage-request-fails)). The `five_hour` / `seven_day` / `seven_day_opus` field names and the `limits[]`/`scope.model.display_name` shape the sensor parses are likewise undocumented; only the status-line projection (`used_percentage`, `resets_at` as epoch seconds) is.
- Reading `claudeAiOauth.accessToken` out of `~/.claude/.credentials.json`:
  the path and mode are documented, the JSON schema is not; it moves with
  `CLAUDE_CONFIG_DIR` and does not exist on a keychain-backed macOS Host or a
  `CLAUDE_CODE_OAUTH_TOKEN` Host.
- The daemon's premise that "the CLI refreshes that token every run" is
  consistent with the docs (auto-refresh, single-refresher lock), but the
  docs also say a failed refresh **deletes** the file's login, which is
  exactly the state the sensor cannot tell apart from "no budget".

## 3. Implications for the Identity decision (#14) and the budget gate

1. **Identity choice is a budget-model choice.** A subscription Host has a
   flat cost and a real, if undocumented, "percent of window used" signal; an
   API-key Host has an unbounded per-token cost and only hard stops (429/400
   with reset dates) plus headers per request. Bedrock/Vertex/Foundry push
   both auth and budget to the cloud account and remove every Anthropic-side
   signal. The Harness config should name the mode explicitly rather than
   sniff it, because the gate has to be a different algorithm per mode.
2. **For subscription Hosts, prefer `CLAUDE_CODE_OAUTH_TOKEN` from
   `setup-token` for Setup, keep `/login` as the fallback.** It is the
   documented headless path, needs no browser on the Host, lives in the
   systemd env with the other secrets, and cannot be silently deleted by a
   failed refresh. Costs: a hard one-year expiry the Daemon must track (store
   the mint date; warn at 30 days), no `/usage-credits`, and — undocumented —
   whether `/api/oauth/usage` accepts it must be verified during Setup. If
   the Host stays on `/login`, Setup should use `claude auth login` (stdin
   code paste) and the Daemon should surface the 3-day expiry warning.
3. **Make the gate distinguish auth failure from budget exhaustion.** Today
   both degrade to a 5-hour sleep. `claude auth status` (exit 1 when not
   logged in) is a documented zero-cost pre-fire probe, and a fire that
   returns `authentication_failed` / `account_on_hold` / `billing_error` in
   the `api_retry` event or `Failed to authenticate…` in the result should
   park the Daemon in a distinct "needs-human" state (a `HITL` signal), not
   sleep to a phantom reset.
4. **Treat `/api/oauth/usage` as an unsupported dependency with a documented
   degrade path.** Keep it as the primary sensor because nothing documented
   replaces its numbers, but (a) fall back to parsing the documented limit
   errors (`You've hit your session limit · resets 3:45pm` → sleep to that
   time; weekly → sleep to that time; per-model → switch model, as the
   `FABLE_FALLBACK_*` logic already does), and (b) consider a per-fire status-line hook: the `statusLine` command receives `rate_limits.{five_hour,seven_day}` on every render, which is the *documented* projection of the same data — worth a prototype to see whether it fires in `-p`.
5. **One account, many Hosts = one pool.** Multiple Target Projects on one
   subscription must share a single gate (a coordinator or a shared lock on
   the reset window), or run under separate accounts / API keys. Per-project
   isolation with real spend caps only exists on the API side (workspaces,
   per-user limits in the Claude Code workspace) or on cloud providers.
6. **API-key Hosts need a different gate shape**: no window to pace on, so
   pace on money — `--max-budget-usd` per fire, a daily budget derived from
   the org spend limit, and treat a 429 without `retry-after` or a 400 spend
   message as "sleep until the stated date". Rate-limit headers are visible
   only through an LLM gateway or OTel, not from `claude -p` output.
7. **`--bare` is coming for `-p`** and it ignores every OAuth source. If the
   Daemon ever adopts it for reproducible fires, subscription auth stops
   working; that is another reason the Identity decision should not assume
   subscription-only.
8. **Terms**: subscription automation is explicitly permitted by the Claude
   Code docs for the account owner's own CI/scripts; a Host shared by several
   people must use Team/Enterprise seats, Console keys, or a cloud provider.
