# Verifier runbook

This Target Project is the harness itself. It has no UI Surface: nothing to
boot, tour or screenshot. Verification is the test suites plus one dry-run
Fire against the fixture Target Project.

- `smoke` (run by the provider) is `bash run-tests.sh`: every `lib/*.test.sh`
  and `dashboard/*.test.py` must pass.
- A PR that changes a skill must keep `lib/runbook-check.sh` green; it runs
  inside the suites.
- A PR that changes `lib/fire.sh`, `lib/daemon.sh`, `lib/pickup-triage.sh` or
  `lib/pr-triage.sh` is also checked by one dry-run Fire on the fixture:
  `AUTO_AGENT_FIRE_MODEL=haiku AUTO_AGENT_STATE_DIR=<scratch> AUTO_AGENT_HOST_ENV=/nonexistent bin/auto-agent fire --dry-run plugin/fixtures/target-project`
  must end on `afk-pickup: would-pick #N` or `no eligible issue`.
- Never run anything against `~/auto-agent-install` or the live Host env; the
  install is the Harness running this very Fire.
