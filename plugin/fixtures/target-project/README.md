# Fixture Target Project

The Target Project the harness tests itself against: a stdlib web service, the
single-process reference Environment provider, and a complete `.auto-agent/`
directory. Every Slice that needs "something to run against" uses this
directory; nothing here is Smart Smoker code.

- `app/server.py`: one process, one port, `/`, `/api/health`, `/api/items`.
- `verify/provider`: the Environment provider (`up`, `down`, `smoke`, `status`)
  behind the contract in `../../providers/CONTRACT.md`, and the harness's
  single-process reference provider. It sources `provider-lib.sh` by relative
  path; a real Target Project copies that lib in beside its own provider.
- `.auto-agent/harness.json`: a label-only pick, a non-default research prefix
  (`docs/findings/`, so a resolve dry run proves the prefix is read, not
  assumed), a `browser` and an `api` Surface, a hermetic tier with smoke on, a
  deployed tier declared but disabled.
- `.auto-agent/*.md`: the three prose siblings (verifier runbook, bot-PR
  checklist, deployed checks).

Check the config from the repo root:

    bin/auto-agent check-config plugin/fixtures/target-project

Drive the provider by hand (any PR number):

    plugin/fixtures/target-project/verify/provider up --pr 42
    plugin/fixtures/target-project/verify/provider down --pr 42

Or run the whole contract over it:

    bin/auto-agent provider-check --pr 42 plugin/fixtures/target-project
