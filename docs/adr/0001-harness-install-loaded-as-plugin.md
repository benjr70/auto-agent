---
status: accepted
---

# Harness install loaded as a Claude Code plugin, never vendored into the Target Project

Smart-Smoker-V2 ran its daemon from files committed in its own checkout, so
"merge to master is the deploy". Once the harness lives in this repo that
coupling has to be replaced, and vendoring it (subtree or submodule) into every
Target Project would tie each project's history to harness churn and turn one
upgrade into N pull requests. We decided that each Host carries one **Harness
install**: a pinned checkout of this repo beside the Target Project checkout.
The Daemon, its `lib/*.sh` scripts and the Dashboard run straight from that
install, and the wrapper passes `--plugin-dir <install>/plugin` explicitly on
every Fire so skills, agents, hooks and harness-owned MCP servers load without
writing a single file into the Target Project. The Target Project commits only
its Harness config.

## Considered options

- **Vendored copy** (subtree or submodule): keeps "merge to master is the
  deploy" but couples every Target Project to harness churn. Rejected.
- **Template repo** forked per project: no shared upgrades at all. Rejected.
- **User-scope marketplace install** instead of `--plugin-dir`: a second,
  separately versioned pin next to the install that already carries the
  scripts. Deferred until the plugin is published for interactive use.

## Consequences

- The Harness install refreshes only between Fires, never during one, so a
  Fire cannot straddle two harness versions. A Host declares a ref: a tag or
  SHA moves only through an explicit Setup upgrade step that restarts the
  Daemon; `main` may float per Fire for dogfood Hosts such as Smart Smoker.
- Settings a plugin cannot carry (`env`, `permissions`, the deny list) ship as
  a harness baseline passed with `--settings` on every Fire; the Target
  Project's own `.claude/settings.json` still applies on top.
- An MCP server is harness-owned only if a harness skill calls it by name
  (github, context7). Everything project-specific stays in the Target
  Project's `.mcp.json`.
- Upstream skills the harness depends on (`research`, `grilling`,
  `domain-modeling`) are vendored into the plugin at a pinned upstream commit,
  so a fresh Host needs nothing beyond the install.
- Plugin skills are namespaced: `/afk-pickup` becomes `/auto-agent:afk-pickup`
  in the wrapper and in every skill that chains to another.
