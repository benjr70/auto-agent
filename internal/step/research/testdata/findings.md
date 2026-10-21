# Claude Code's headless auth modes

Ticket: [#13](https://github.com/acme/widgets/issues/13) (part of wayfinder map
[#9](https://github.com/acme/widgets/issues/9) — Reusable auto-agent).
Researched on 2026-10-12.

Sources: https://docs.anthropic.com/en/docs/claude-code/iam

## TL;DR

- A subscription login or an API key; the Daemon uses the login.

## Auth modes

Claude Code authenticates with a Claude.ai subscription login or an Anthropic
API key ([IAM](https://docs.anthropic.com/en/docs/claude-code/iam)).
