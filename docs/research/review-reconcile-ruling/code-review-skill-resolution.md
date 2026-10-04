# Why the correctness review runs a Standards and Spec reviewer instead of a bug hunter

- Ticket: benjr70/auto-agent#66 (wayfinder:research)
- Map: benjr70/auto-agent#65
- Date: 2026-10-04
- Checked against: auto-agent `origin/main` at `114e03d`; Claude Code 2.1.289 on the Host (`claude --version`)
- Sources (all primary):
  - Skills: https://code.claude.com/docs/en/skills
  - Commands reference: https://code.claude.com/docs/en/commands
  - Code Review, "Review a diff locally": https://code.claude.com/docs/en/code-review#review-a-diff-locally
  - Subagents: https://code.claude.com/docs/en/sub-agents
  - Plugin components: https://code.claude.com/docs/en/plugins/components
  - Plugin manifest reference: https://code.claude.com/docs/en/plugins-reference
  - CLI reference: https://code.claude.com/docs/en/cli-reference
  - The Host itself, read-only: the skill directories, the running Fire's command line and environment, and the Fire's subagent transcripts under `~/.claude/projects/-home-claude-agent-1-Smart-Smoker-V2/`
  - The review summaries on Smart-Smoker-V2 PR 715 and PR 717

Vocabulary follows `CONTEXT.md` (Fire, Host, Target Project, Agent PR).

This note reports facts and options. It does not choose between the options.

## Short answer

The correctness axis calls a skill by the bare name `code-review`. On the Host that name resolves to a personal (user-level) skill, `~/.claude/skills/code-review`, installed from `mattpocock/skills` on 2026-08-29. That skill is a Standards + Spec review. Claude Code's documented rule is that a personal, project or enterprise skill with the same name as a bundled skill replaces the bundled one, so the bundled bug-hunting `/code-review` is not reachable by that name in any session run by this OS user. The plugin does nothing to pin which `code-review` it means: the word "built-in" in the prompt is prose, not a selector.

Every recorded correctness-axis call on this Host, from PR 660 (2026-09-07) to PR 718 (2026-10-04), resolved to the personal skill. The bundled review has not run for pr-review on this Host in that window.

## 1. Which `code-review` is resolved, and how pr-review names it

### How pr-review names it

`plugin/skills/pr-review/SKILL.md` on `origin/main` names it three times, always as a bare, un-namespaced name:

Line 5 (frontmatter description):

```
  correctness pass (built-in /code-review at medium effort) plus a spec pass
```

Lines 153 to 161 (the correctness-axis subagent prompt):

```
> You are the CORRECTNESS-AXIS reviewer for PR #\<PR_NUM> (branch \<BRANCH>,
> repo \<REPO>). Check out the branch, then invoke the built-in `/code-review`
> skill at **medium** effort against the branch diff vs the default branch
> (`origin/<BASE>`). Do NOT pass `--comment` and do NOT pass `--fix`. When it
> completes, restate every finding it produced — nothing added, nothing dropped
> — in the JSON contract below, with `"axis":"correctness"` and `category` one
> of `bug|logic-error|data-loss|race|error-handling|security`. Cite only
> path+line pairs that appear in the diff (right-hand side). Then the sentinel
> block and the terminal line `correctness-review: <k> finding(s)`.
```

Line 163:

```
Treat `/code-review`'s native output as opaque; the wrapper's only job beyond
```

The wrapper is spawned as `subagent_type: general-purpose` (same file, section 2). `lib/runbook-check.sh:126` pins the literal `/code-review` in that file as the `correctness-axis` runbook check, so the bare name is also asserted by a test.

The copy the live Daemon loads, `~/auto-agent-install/plugin/skills/pr-review/SKILL.md`, is byte-identical to `origin/main` (`cmp` exit 0).

### What exists on the Host under that name

| Location | `code-review` present? |
| :- | :- |
| Personal: `~/.claude/skills/code-review` | Yes. Symlink to `~/.agents/skills/code-review`. `~/.agents/.skill-lock.json` records source `mattpocock/skills`, path `skills/engineering/code-review/SKILL.md`, installed `2026-08-29T13:35:14Z`. Its description: "Review the changes since a fixed point ... along two axes: Standards ... and Spec ..." |
| Project: `Smart-Smoker-V2/.claude/skills/` | No. It holds `review-pr` (a different name, `disable-model-invocation: true`) and an older repo-owned `pr-review`, but no `code-review`. Same on the default branch on GitHub. |
| Project commands: `Smart-Smoker-V2/.claude/commands/` | Directory does not exist. |
| Personal commands: `~/.claude/commands/` | Directory does not exist. |
| Plugin: `plugin/skills/` in this repo and in `~/auto-agent-install/plugin/skills/` | No. 17 skills, none named `code-review`. |
| Enterprise / managed: `/etc/claude-code` | Directory does not exist. |
| Bundled | Yes, ships with Claude Code (see section 2). |

So the PR summaries' phrase "the project's two-axis override" is inaccurate on one point: the override is personal (user-level, per OS user), not in the Target Project.

### What the Fire actually called

The running Fire's command line (`ps`):

```
claude --print --permission-mode bypassPermissions --plugin-dir /home/claude-agent-1/auto-agent-install/plugin --settings /home/claude-agent-1/auto-agent-install/plugin/settings/baseline.json --mcp-config ... --output-format stream-json --verbose /auto-agent:afk-pickup
```

Its cwd is `/home/claude-agent-1/Smart-Smoker-V2` and its environment has `HOME=/home/claude-agent-1`. It passes no `--bare`, `--setting-sources`, `--safe-mode` or `--disable-slash-commands` (`lib/fire.sh:507` is the only `--plugin-dir` site). `baseline.json` carries `env` and `permissions` only: no `skillOverrides`, no `disableBundledSkills`. Personal skills therefore load in every Fire.

The correctness-axis subagent transcripts record the call and the directory the skill loaded from. For PR 717 (`fe4c8568-.../subagents/agent-a93b281953af9ea70.jsonl`):

```
"name":"Skill","input":{"skill":"code-review","args":"medium origin/master...HEAD"}
Base directory for this skill: /home/claude-agent-1/.claude/skills/code-review
```

All twelve recorded calls resolve to the same directory:

| Transcript date | PR | Resolved base directory |
| :- | :- | :- |
| 2026-09-07 | 660, 661 | `~/.claude/skills/code-review` |
| 2026-09-08 | 662, 664, 665, 666 (twice), 667 | `~/.claude/skills/code-review` |
| 2026-10-03 | 715, 716 | `~/.claude/skills/code-review` |
| 2026-10-04 | 717, 718 | `~/.claude/skills/code-review` |

Consequences visible in those calls:

- The args `medium origin/master...HEAD` are written for the bundled skill's grammar (`/code-review [low|medium|high|...] [pr#|branch|path]`). The personal skill takes a "fixed point" and has no effort argument, which is what the PR 715 summary means by "no effort level applied".
- The personal skill has no bug-category vocabulary, so the wrapper mapped its Standards and Spec findings onto the nearest of `bug|logic-error|data-loss|race|error-handling|security`. PR 715's summary says the values "are nearest-fit"; PR 717's says the 20 findings "are spec-conformance and maintainability reads from the code (tests not run), not verified defects".
- pr-review already runs its own spec axis, so with this resolution the spec question is asked twice and the correctness question is not asked at all.

Review summaries: https://github.com/benjr70/Smart-Smoker-V2/pull/715#issuecomment-5973901866 and https://github.com/benjr70/Smart-Smoker-V2/pull/717#issuecomment-5981865458.

## 2. Claude Code's documented precedence

### The bundled skill

`/code-review` is a bundled skill, not a fixed-logic built-in command. Skills doc, "Bundled skills": "Claude Code includes a set of bundled skills, such as `/doctor`, `/code-review`, `/batch`, `/debug`, `/loop`, and `/claude-api`. Bundled skills are prompt-based."

Commands reference row: `/code-review [low|medium|high|xhigh|max|ultra] [--fix] [--comment] [--max-findings n|all|default] [pr#|branch|path]`: "Review the current diff, or a PR number, branch, or path you pass, for correctness bugs. Depending on your model and effort level, the review also covers cleanup opportunities. ... Alias: `/review`".

Code Review doc, "Tune effort and arguments": "At `low` and `medium`, the review reports only the findings it's most confident in, so you see fewer false positives; `high` through `max` broaden coverage."

Code Review doc, "Let Claude start the review": "Claude can start `/code-review` on its own." (Since v2.1.246 without a feature flag.) In a `-p` run the review runs in the foreground and "Claude Code waits for the review and includes the findings in the response".

### Same-name resolution

Skills doc, "Resolve skills that share a name" (https://code.claude.com/docs/en/skills), quoted in full for the rows that matter:

| Same name in | Which one runs |
| :- | :- |
| Two of enterprise, personal, and project | "Enterprise over personal, and personal over project. With `deploy` in both `~/.claude/skills/` and the project's `.claude/skills/`, `/deploy` runs the personal one" |
| Any of those locations and a bundled skill | "Your skill replaces the bundled command, but not its aliases. A project `code-review` skill replaces `/code-review`, and the bundled alias `/review` never runs your skill" |
| Any of those locations and a built-in command | "In a local terminal session, your skill replaces the built-in command, but not its aliases." |
| A skill and a file in `.claude/commands/` | "The skill" |
| A plugin skill and a skill at any of the locations above | "Both load, because plugin skills are namespaced as `/plugin-name:skill-name`" |

Read as an order for a bare name: enterprise, then personal, then project, then bundled or built-in. Plugin skills sit outside that order because their canonical name carries the plugin prefix.

The documented example is exactly this case (a non-bundled `code-review` replacing `/code-review`), with a project skill where the Host has a personal one. The "Any of those locations" wording covers personal.

### Plugin skills and the bare name

Skills doc, "How a skill gets its command name": "In a plugin skill, the frontmatter `name` replaces the directory name in the last segment of the command, so `my-plugin/skills/review/SKILL.md` with `name: fancy` becomes `/my-plugin:fancy`. The bare `/fancy` also invokes the skill unless another command already uses that name."

Plugin manifest reference, `name`: "Claude Code namespaces every component under it, so an agent `reviewer` in plugin `deploy-tools` appears as `deploy-tools:reviewer`."

So a plugin skill's namespaced name is always its own. Its bare name is a fallback that loses to any other command already holding that name.

### Related controls

- `skillOverrides` (Skills doc, "Override skill visibility from settings") sets a skill to `on`, `name-only`, `user-invocable-only` or `off` by name. "Plugin skills are not affected by `skillOverrides`." The doc does not say which of two same-named skills an entry applies to once one has replaced the other.
- `disableBundledSkills` turns bundled skills off.
- CLI flags that change which skills load at all (CLI reference): `--bare` ("skip auto-discovery of hooks, skills, custom commands, subagents, installed plugins, MCP servers, auto memory, and CLAUDE.md"), `--setting-sources` ("Comma-separated list of setting sources to load (`user`, `project`, `local`)"), `--safe-mode`, `--disable-slash-commands`. The CLI reference does not state whether `--setting-sources` without `user` also stops personal skills from loading; that is not documented on the pages read.

### Subagents and skills

Subagents doc, `skills` frontmatter field: "Skills to preload into the subagent's context at startup. The full skill content is injected, not only the description. Subagents can still invoke unlisted project, user, and plugin skills through the Skill tool."

So the `general-purpose` wrapper pr-review spawns sees the same skill set as the Fire, personal skills included. That matches the transcripts.

## 3. Mechanisms for a plugin skill to pin the review it means

### A. Plugin-namespaced invocation (`plugin:skill`)

What the docs say: a plugin skill is always reachable as `/plugin-name:skill-name`, and "Both load" when a personal, project or enterprise skill shares the last segment. Nothing at those levels can replace the namespaced name. This repo already relies on it: the Fire is started with `/auto-agent:afk-pickup`, and afk-pickup delegates to `/auto-agent:pr-review`.

Caveats:

- It only pins skills that live in a plugin. The bundled `/code-review` has no namespaced form in the docs, so this mechanism cannot be used to reach it. It applies only together with option B.
- The docs do not rule on two enabled plugins with the same plugin name. The namespace is the manifest `name` (`auto-agent`).
- The bare last segment is not pinned: "The bare `/fancy` also invokes the skill unless another command already uses that name."

### B. Ship the review as a skill in the plugin

What the docs say: any `plugin/skills/<name>/SKILL.md` loads as `/auto-agent:<name>` wherever the plugin is loaded, `--plugin-dir` included. Frontmatter can set `effort` ("Overrides the session effort level"), `context: fork` with `agent` (run in a subagent of a named type), `allowed-tools`, `disable-model-invocation` and `user-invocable`. The repo already vendors upstream skills this way with a pin file (`plugin/vendored-skills.json`, `lib/vendored-skills.sh`).

Caveats:

- The bundled `/code-review` prompt is not published as a file in the docs read here, so a plugin-shipped review is the plugin's own prompt (or a vendored third-party one), not a copy of the bundled one. Its behaviour, effort handling and finding format are whatever the plugin writes.
- Naming it `code-review` inside the plugin gives `/auto-agent:code-review`, which is pinned, but the bare `/code-review` would still go to the personal skill on this Host. The call site has to use the namespaced name for the pin to hold.
- A skill with `disable-model-invocation: true` cannot be preloaded into a subagent (Subagents doc, "Preload skills into subagents") and Claude cannot start it itself, so a review the wrapper must invoke cannot carry that flag.
- `context: fork` gives the subagent only the skill content as its prompt: "The subagent doesn't see your conversation history, so the skill's instructions have to stand on their own."

### C. Invoke the bundled review explicitly

What the docs say:

- By its alias. "Your skill replaces the bundled command, but not its aliases ... the bundled alias `/review` never runs your skill." The permissions table in the Skills doc lists the rule `Skill(review)` as blocking "The bundled `/code-review`, through its `/review` alias", which shows the Skill tool accepts the alias as a name for the bundled skill. The commands reference gives `/review` the same effort levels and flags as `/code-review`.
- By removing the shadow. With no enterprise, personal or project skill named `code-review`, the bare name reaches the bundled skill. On this Host that means the personal `~/.claude/skills/code-review` not being present for the Daemon's OS user, or the Fire not loading personal skills (`--bare` is documented to skip skill auto-discovery; it also skips plugins, hooks, MCP servers and CLAUDE.md, and "Skills in a directory you pass with `--add-dir` still load").

Caveats:

- The docs give no namespaced or otherwise unshadowable name for a bundled skill. The alias is the only documented route past a same-named skill.
- The alias is itself a bare name. A personal, project or enterprise skill or command named `review` would take it by the same rule. None exists on this Host today (the Target Project has `review-pr`, a different name).
- Alias behaviour has changed across versions: "Before v2.1.223, `/review` was a separate command that ran a single-pass, read-only review of a GitHub pull request by number." The Host runs 2.1.289 and `baseline.json` sets `DISABLE_AUTOUPDATER=1`.
- The bundled skill depends on the Claude Code version, not on the plugin: its prompt, its finding format and what `medium` means can change with a CLI upgrade. pr-review already treats its output as opaque for that reason. The commands reference adds "Depending on your model and effort level, the review also covers cleanup opportunities", so the bundled review is not guaranteed to be bugs-only.
- `disableBundledSkills`, or a `skillOverrides` entry of `off` or `user-invocable-only` for `code-review`, makes the bundled skill unavailable to the model regardless of the name used. Neither is set on this Host.
- Not tested here: that a Skill-tool call with `skill: "review"` inside a `--print` Fire on 2.1.289 loads the bundled review while the personal `code-review` is present. The docs say it should; no run confirmed it.

### D. Embed the review prompt in a subagent

What the docs say: a plugin can ship agents in `plugin/agents/`; each is addressed by its scoped name (`auto-agent:<agent>`), and the Agent tool takes that as `subagent_type`. An agent's body is its system prompt, so the review instructions travel with the plugin and involve no skill-name lookup at all. Frontmatter supports `tools`, `disallowedTools`, `model`, `effort` and `skills` (preload). The plugin already ships `reviewer.md`, `implementer.md`, `verifier.md` and `manual-verifier.md` this way. A lighter form of the same idea needs no new agent: write the review instructions directly into the prompt pr-review hands to its `general-purpose` wrapper, as the spec axis already does.

Caveats:

- Agent names have their own precedence, and plugins are last: managed, then `--agents` flag, then project `.claude/agents/`, then `~/.claude/agents/`, then "Plugin's `agents/` directory ... 5 (lowest)". The scoped name `auto-agent:<agent>` is what avoids a same-named project or personal agent; a bare `reviewer` would lose to the Target Project's own `.claude/agents/reviewer.md`, which exists in Smart-Smoker-V2.
- "For security reasons, plugin subagents don't support the `hooks`, `mcpServers`, or `permissionMode` frontmatter fields."
- A subagent can still call skills: "Subagents can still invoke unlisted project, user, and plugin skills through the Skill tool ... To prevent a subagent from invoking skills entirely, omit `Skill` from the `tools` list or add it to `disallowedTools`." An embedded-prompt reviewer that keeps the Skill tool can still be drawn to the personal `code-review` by its description ("Use when the user wants to review a branch, a PR ...").
- A `skills:` preload entry is a skill name and resolves by the same rules as any other name; preloading `code-review` would preload the personal one on this Host. The docs do not show a namespaced plugin skill in a `skills:` list, so whether `auto-agent:<skill>` is accepted there is not documented.
- As with B, the prompt is the plugin's own. It does not reproduce the bundled review.

### Side by side

| Mechanism | Pins against a same-named personal/project skill? | Reaches the bundled review? | Who owns the review prompt |
| :- | :- | :- | :- |
| Bare `/code-review` (today) | No | Only where nothing shadows it | Whoever wins the name |
| A. `auto-agent:<skill>` | Yes | No (needs B) | The plugin |
| B. Skill shipped in the plugin, called namespaced | Yes | No | The plugin |
| C1. Bundled alias `/review` | Against `code-review`, yes; against a skill named `review`, no | Yes | Claude Code version |
| C2. Remove the shadow (no personal skill, or a Fire that does not load personal skills) | Only on Hosts kept that way | Yes | Claude Code version |
| D. Prompt embedded in a plugin agent or in the wrapper prompt | Yes, if addressed by scoped name and the Skill tool is withheld or unused | No | The plugin |

## Not verified

- No test Fire was run. The alias route (C1) and the effect of `--setting-sources` on personal skill loading are from the docs only.
- The bundled `/code-review` prompt was not inspected; its description here is the docs' description.
- The Host's transcripts hold no `code-review` Skill call earlier than 2026-09-07, so nothing here says what resolved before the personal skill was installed on 2026-08-29.
- Other Hosts were not examined. On a Host whose Daemon user has no personal or project `code-review`, the documented rules say the bare name reaches the bundled skill.
