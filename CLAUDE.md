# CLAUDE.md

Reusable autonomous "AFK" agent: a daemon that picks up GitHub issues written in
the wayfinder / spec / slice format, implements everything labelled `AFK`, and
drives each PR to green. Extracted from the Smart-Smoker-V2 harness so it can be
pointed at any project. The charting map for the extraction lives on this
repo's issue tracker (label `wayfinder:map`).

## Agent skills

### Issue tracker

GitHub Issues on `benjr70/auto-agent` via `gh`. See `docs/agents/issue-tracker.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
