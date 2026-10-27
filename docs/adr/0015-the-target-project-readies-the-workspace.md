---
status: accepted
---

# The Target Project readies the Workspace; the Daemon keeps what it names

ADR 0013 made the Host the operator's job and retired the Host extension, the
Target Project's script that Setup ran once in the one checkout the bash
Daemon worked in. The Go Daemon works in its own Workspace instead, and
resets it with `git clean -ffdx` before each Fire, so nothing installed in it
survives. On Smart Smoker that left every Verification round unable to boot:
its Environment provider needs `scripts/stack-runner`'s dependencies, and its
Electron Surface needs the workspace install and a built shell bundle, about
2.2 GB that no Host-wide install can put inside a clone the Daemon owns.

We decided the Harness config may declare, under `workspace`, the paths the
reset keeps (`keep`, gitignore-style patterns) and a command Go runs in the
Workspace after the reset (`prepare`), with no credential, whenever the
default branch's head or the command has changed since it last succeeded. A
failed `prepare` fails the Fire; the back-off after failed Fires paces the
retries. The Daemon still installs nothing on the Host: the command is the
Target Project's, and it writes only inside the clone.

## Considered options

- **The provider installs its own prerequisites on `up`**: no Daemon change,
  but with nothing kept every round pays several minutes of installs, and
  every implement Fire reinstalls the workspace. Rejected.
- **Run the old Host extension after every reset, keeping nothing**: correct
  and slow for the same reason. Rejected.
- **Stop removing ignored files (`git clean -ffd`)**: keeps build outputs and
  anything a Step hid under an ignored name, from one branch into the next.
  Rejected for the explicit list.

## Consequences

- A kept path is a place a Step can leave something a later Fire runs. A
  Target Project that declares no `keep` has the old guarantee.
- `keep` is read from the clone's config as the last Fire left it, so a
  change to it takes effect one Fire late.
- The bash loader resolves `workspace` too, and the shared schema accepts it,
  so a Target Project that declares it can still roll back to the bash.
