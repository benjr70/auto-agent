# PROTOTYPE: fixture Target Project (throwaway)

Answers the Verification Harness contract ticket's question: is the
`up`/`down`/`smoke`/`status` contract usable by a project that is not Smart
Smoker? Nothing here is production. No Smart Smoker code was copied.

- `app/server.py`: a stdlib web service with `/`, `/api/health`, `/api/items`.
- `verify/provider`: the Environment provider (one executable, subcommands).
- `.auto-agent/harness.json`: the stub Harness config.
- `harness-stub.sh`: the Daemon's side of the contract, as the harness would drive it.

Run: `./harness-stub.sh 42` (any PR number).
