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

Or walk one verification round's seams over it (no PR, no Claude): which
Surfaces a diff touches, the environment up, a tour captured at the declared
viewport, and the tour injected into a body.

    printf 'app/server.py\n' | bin/auto-agent surfaces tour plugin/fixtures/target-project
    bin/auto-agent surfaces viewport web plugin/fixtures/target-project
    bin/auto-agent verify-boot up --pr 0 plugin/fixtures/target-project
    #   ... drive the browser Surface at FIXTURE_WEB_URL, writing
    #   `bin/auto-agent evidence name web <n> <slug>` files into
    #   `bin/auto-agent evidence dir --pr 0 --round 1`
    bin/auto-agent evidence shots "$DIR" | bin/auto-agent evidence inject body.md
    bin/auto-agent verify-boot down --pr 0 plugin/fixtures/target-project

Paste the bot-PR checklist into a body the way the deps-land lane does (the
fixture declares no `dependabot` block, so `deps-lane lane` reads off; the
inject takes the checklist path directly):

    bin/auto-agent deps-lane lane plugin/fixtures/target-project
    printf 'Bumps x.\n' | bin/auto-agent deps-lane inject-checklist \
        plugin/fixtures/target-project/.auto-agent/bot-pr-checklist.md

Read the deployed tier's lane over it (off: the fixture ships `enabled: false`),
or, with it switched on in a copy, the live block of a running fixture service
standing in for a deployed environment:

    bin/auto-agent deployed lane plugin/fixtures/target-project
    FIXTURE_DEPLOYED_URL=http://127.0.0.1:<port> bin/auto-agent deployed status <copy>
