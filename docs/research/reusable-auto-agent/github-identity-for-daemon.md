# Research: GitHub identity for the Daemon (App vs machine user vs owner PAT)

Ticket: [#12](https://github.com/benjr70/auto-agent/issues/12) (part of map
[#1](https://github.com/benjr70/auto-agent/issues/1)). Feeds the Identity
decision [#14](https://github.com/benjr70/auto-agent/issues/14).
Researched 2026-09-10 against GitHub Docs, the gh CLI manual and the live
GraphQL schema (introspected with `gh api graphql`). Operation inventory
taken read-only from the Smart-Smoker-V2 harness at
`scripts/claude-agent/lib/*.sh`, `.claude/skills/*/SKILL.md` and
`docs/agents/issue-tracker.md`.

## Sources

Primary (GitHub Docs / gh manual / live schema):

- [S1] Generating an installation access token — <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app>
- [S2] Generating a JWT for a GitHub App — <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app>
- [S3] Authenticating as a GitHub App installation — <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation>
- [S4] Triggering a workflow (Actions) — <https://docs.github.com/en/actions/writing-workflows/choosing-when-your-workflow-runs/triggering-a-workflow>
- [S5] Making authenticated API requests with a GitHub App in a workflow — <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/making-authenticated-api-requests-with-a-github-app-in-a-github-actions-workflow>
- [S6] Permissions required for GitHub Apps (REST) — <https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps?apiVersion=2022-11-28>
- [S7] Permissions required for fine-grained PATs (REST) — <https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens?apiVersion=2022-11-28>
- [S8] Managing your personal access tokens — <https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens>
- [S9] Scopes for OAuth apps (classic PAT scopes) — <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps>
- [S10] Choosing permissions for a GitHub App — <https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app>
- [S11] Using the API to manage Projects — <https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/using-the-api-to-manage-projects>
- [S12] Managing access to your projects — <https://docs.github.com/en/issues/planning-and-tracking-with-projects/managing-your-project/managing-access-to-your-projects>
- [S13] About protected branches — <https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches>
- [S14] Approving a pull request with required reviews — <https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews>
- [S15] Deciding when to build a GitHub App — <https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/deciding-when-to-build-a-github-app>
- [S16] Managing deploy keys (machine users section) — <https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys>
- [S17] Rate limits for the REST API — <https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api>
- [S18] gh pr merge — <https://cli.github.com/manual/gh_pr_merge>
- [S19] gh auth login — <https://cli.github.com/manual/gh_auth_login>
- [S20] gh environment variables — <https://cli.github.com/manual/gh_help_environment>
- [S21] gh auth token — <https://cli.github.com/manual/gh_auth_token>
- [S22] gh auth setup-git — <https://cli.github.com/manual/gh_auth_setup-git>
- [S23] Live GraphQL schema, introspected 2026-09-10 with `gh api graphql` (`Mutation.resolveReviewThread`, `Mutation.addProjectV2ItemById`, `Mutation.updateProjectV2ItemFieldValue`, `Issue.blockedBy/blocking/parent/subIssues`).

Corroborating only (GitHub-hosted community threads, not documentation):

- [C1] <https://github.com/orgs/community/discussions/64849> — App installed on a personal account gets repositories but not the user's projectsV2.
- [C2] <https://github.com/orgs/community/discussions/156512> — fine-grained PATs and Projects on personal repos.

## TL;DR

1. **Two of the four candidates cannot do the job on a user-owned repo with a
   user-owned Project at all.** A *fine-grained PAT* of a machine user is
   documented as unable to "contribute to repositories where the user is an
   outside or repository collaborator" and unable to "access Projects owned
   by a user account" [S8]. A *GitHub App* has a `Projects` permission only
   at the **organization** level; neither the App nor the fine-grained-PAT
   permission tables list any `users/{username}/projectsV2` endpoint
   [S6][S7], so the Smart-Smoker pick query (`projectItems`/`Priority` on a
   user project) and `gh project item-add/item-edit` have no documented
   route for either. Both would become viable if the Target Project lived in
   an **organization**.
2. **A classic PAT on a dedicated machine-user account does everything the
   harness does today**, with scopes `repo` + `project` + `workflow` [S9],
   provided the machine user is added as a repository collaborator (with
   **admin** for `gh pr merge --admin`) and as a **Write** collaborator on
   the user-level Project [S12]. GitHub's ToS allow one machine user per
   person [S16]. This is the same token shape the daemon uses now, just on a
   different account, so every human-vs-agent check becomes meaningful.
3. **The ticket's premise that App-token pushes do not trigger `on: push` is
   wrong.** GitHub Docs say only `GITHUB_TOKEN`-driven events skip new runs,
   and explicitly recommend "a GitHub App installation access token or a
   personal access token" to trigger them [S4].
4. **Token lifetime**: installation tokens expire after 1 hour [S1][S3] and
   must be re-minted from a ≤10-minute RS256 JWT [S2]; classic and
   fine-grained PATs may be non-expiring (classic ones are auto-removed after
   a year unused) [S8]. A long-lived Daemon on an App needs a refresh loop
   that also re-writes the git credential; a PAT needs none.
5. **Code-owner review is never satisfiable by the Daemon itself** under
   `CODEOWNERS * @benjr70`: a PR author cannot approve their own PR [S14] and
   the approver must be the code owner [S13]. The harness never merges Agent
   PRs anyway; the two machine merges bypass the rule with `--admin`, which
   requires admin on the repo and `enforce_admins` off [S13][S18].
6. `gh` consumes any of the three token kinds through `GH_TOKEN`, which
   "takes precedence over previously stored credentials" [S20]; the manual
   warns against `--with-token` for fine-grained tokens [S19]. Git pushes use
   the token as HTTP password (`x-access-token:<token>` for Apps [S3]) via
   `gh auth setup-git` [S22].

## 1. What the harness actually does on GitHub

Read-only grep of the Smart-Smoker-V2 harness (paths relative to that repo).
Every item below is a row in the matrix in §3.

| # | Operation | Call shape | Where |
|---|---|---|---|
| O1 | Who am I | `gh auth status`, `gh api user -q .login` | `lib/pickup-triage.sh:143-151` |
| O2 | Issue list / view / comment / close / labels / assignee | `gh issue list --label … --json`, `gh issue view`, `gh issue comment`, `gh issue close`, `gh issue edit --add-label/--remove-label/--add-assignee`, `gh label list/create` | `lib/pickup-triage.sh`, `agent-run`, skills, `docs/agents/issue-tracker.md` |
| O3 | Pick query | `gh api graphql` on `repository.issues(labels:["AFK"]) { blockedBy, assignees, projectItems { project { number } fieldValueByName("Priority") } }` | `lib/pickup-triage.sh:228-255` |
| O4 | Sub-issue link | `gh api -X POST repos/O/R/issues/<parent>/sub_issues -F sub_issue_id=<db id>` | `afk-resolve`, `to-spec`, `to-tickets` skills; `issue-tracker.md:41,46` |
| O5 | Issue dependency | `gh api -X POST repos/O/R/issues/<child>/dependencies/blocked_by -F issue_id=<db id>` | `issue-tracker.md:42` |
| O6 | Project item add + field edit | `gh project item-add 1 --owner benjr70 --url …`, `gh project item-edit`, `gh project view`, `gh project field-list` (GraphQL `addProjectV2ItemById`, `updateProjectV2ItemFieldValue` [S23]) | `issue-tracker.md:48` |
| O7 | PR create / edit / ready / list / view / diff / checks / comment | `gh pr create --base master`, `gh pr edit --title/--body-file`, `gh pr ready [--undo]`, `gh pr list --json …`, `gh pr view --json …`, `gh pr diff`, `gh pr checks --json`, `gh pr comment` | `lib/pr-triage.sh`, `lib/deps-gate.sh`, `lib/docs-only-gate.sh`, `pr-watch`, `pr-review`, `afk-pickup` |
| O8 | Review-comment post / reply | `gh api repos/O/R/pulls/<n>/comments -f body -F in_reply_to`, `gh api repos/O/R/issues/<n>/comments [--paginate]` | `lib/review-poster.sh:62-90`, `lib/thread-reconciler.sh:73-77` |
| O9 | Resolve review thread | `gh api graphql` `resolveReviewThread(input:{threadId})` after `reviewThreads` query | `lib/thread-reconciler.sh:39-90` |
| O10 | Admin merge | `gh pr merge <n> --squash --admin --match-head-commit <sha>` (docs-only research PRs, Dependabot PRs) | `lib/deps-gate.sh:166`, `lib/docs-only-gate.sh:112` |
| O11 | Workflow run inspection | `gh run view` | `pr-watch` skill |
| O12 | Git push / force-push, fetch | `git push --force-with-lease origin <branch>`, `git fetch origin` (HTTPS) | `lib/rebase-driver.sh:50,106`, skills |
| O13 | CI must run on the pushed branch (`on: push` / `pull_request` workflows are the babysat checks) | consequence of O12 | `lib/ci-wait.sh`, `pr-watch` |

Today all of this runs as the maintainer's own classic PAT: `gh auth status`
on the Host reports account `benjr70` via `GITHUB_TOKEN` with scopes
including `repo`, `project`, `workflow`, `admin:org` (observed 2026-09-10).

## 2. What GitHub documents for each identity

### 2.1 GitHub App installation token

- **Minting**: create an RS256 JWT with `exp` "no more than 10 minutes into
  the future" and `iat` "60 seconds in the past" [S2]; POST
  `/app/installations/{id}/access_tokens` with the JWT [S1].
- **Lifetime**: "The installation access token will expire after 1 hour."
  [S1][S3]. Tokens can be narrowed per mint with `repositories` /
  `permissions` body parameters [S1].
- **Attribution**: "API requests made by an app installation are attributed
  to the app" [S3], i.e. a distinct `<app>[bot]` actor, so author/assignee
  checks against the human become meaningful.
- **Git over HTTPS**: `git clone https://x-access-token:TOKEN@github.com/owner/repo.git`
  [S3] — the same form works for push.
- **Permissions** (repository level, from the endpoint tables [S6]):
  `Issues: write` covers issue edit/comment/labels, `sub_issues` and
  `dependencies/blocked_by`; `Pull requests: write` covers PR create and
  review comments; `Contents: write` covers merge and push; `Actions: read`
  covers run inspection; `Workflows` is needed to push files under
  `.github/workflows` [S10]. **Projects exists only as an organization
  permission** with `/orgs/{org}/projectsV2/...` endpoints [S6]; no
  user-account Projects permission is listed among the account permissions
  [S6]. Corroborated by [C1] (installation on a personal account returns
  repositories but not projectsV2).
- **Triggers CI**: "you can use a GitHub App installation access token or a
  personal access token instead of `GITHUB_TOKEN` to trigger events" [S4].
- **Rate limit**: installations not on GHEC "scale with the number of users
  and repositories" [S17].
- **Not a seat, not a user**: "GitHub Apps are not tied to a user account and
  do not consume a seat" and "use short lived tokens" [S15]. GitHub
  recommends an App for "a long-lived integration" [S15].

### 2.2 Fine-grained PAT on a machine user

- Documented gaps [S8]: cannot "contribute to repositories where the user is
  an outside or repository collaborator", cannot "contribute to public repos
  where the user is not a member", cannot "access multiple organizations at
  once", cannot "call the Checks API", cannot "access Projects owned by a
  user account".
- Endpoint tables [S7] mirror the App tables: `sub_issues` and
  `dependencies/blocked_by` need `Issues: write`; merge needs
  `Contents: write`; Projects appear only as organization endpoints.
- Lifetime: "Infinite lifetimes are allowed but may be blocked by a maximum
  lifetime policy" [S8].
- gh: the manual says not to pass fine-grained tokens through
  `--with-token`; set `GH_TOKEN` instead [S19].

### 2.3 Classic PAT on a machine user

- Scopes [S9]: `repo` "full access to public and private repositories
  including read and write access to code"; `project` "read/write access to
  user and organization projects"; `workflow` "ability to add and update
  GitHub Actions workflow files".
- Machine users are permitted: "creating a single machine user for
  automation tasks ... is permitted"; add it "as a collaborator" on personal
  repositories [S16]. The machine user's PAT then carries whatever repo role
  the collaborator has (admin needed for O10).
- User-level Projects accept invited collaborators with Read/Write/Admin
  [S12], so a machine user can be given Write on Project #1.
- Lifetime: may be set to never expire; GitHub "automatically removes
  personal access tokens that haven't been used in a year" and recommends
  an expiration [S8]. `gh auth login --with-token` is documented for
  "a personal access token (classic)" [S19].
- Rate limit 5,000 requests/hour per user [S17].

### 2.4 Owner's personal classic PAT (status quo)

Identical capabilities to §2.3 but attributed to the human, which is the
problem the ticket names: PR author, assignee and `WP_AUTHOR` checks cannot
tell the Daemon from the maintainer. Also shares the human's 5,000/hour
budget [S17].

## 3. Capability matrix

Cells: **Y** supported (with required permission/scope), **N** not
supported, **?** not documented. "org only" means documented only for
organization-owned Projects. Assumes a **user-owned** repo and a
**user-owned** Project (the Smart-Smoker shape).

| Operation | App installation token | Fine-grained PAT (machine user) | Classic PAT (machine user) | Owner's personal classic PAT |
|---|---|---|---|---|
| O1 `gh api user` | ? — installation tokens are not user tokens; `/user` is not in the App table [S6]. `gh auth status` reports the token, login must come from the App slug. | Y — user token | Y | Y |
| O2 issues list/view/comment/close/labels/assignee | Y — `Issues: write` (labels marked "additional permissions") [S6] | **N on a collaborator repo** [S8] (Y with `Issues: write` on a repo the user owns / org member [S7]) | Y — `repo` [S9] | Y — `repo` |
| O3 pick GraphQL (`blockedBy`, `assignees`, `projectItems`/`Priority`) | Partial — issues/deps Y (`Issues: read`); `projectItems` on a user Project **N**: no user Projects permission [S6][C1] | **N** — user-owned Projects unsupported [S8]; repo part also N on a collaborator repo | Y — `repo` + `project`/`read:project` [S9] | Y |
| O4 `POST …/sub_issues` | Y — `Issues: write` [S6] | Y — `Issues: write` [S7], but blocked by the collaborator-repo gap [S8] | Y — `repo` (endpoint tables list no extra scope) | Y |
| O5 `POST …/dependencies/blocked_by` | Y — `Issues: write` [S6] | Y — `Issues: write` [S7]; same gap [S8] | Y — `repo` | Y |
| O6 Project item-add / field edit (`addProjectV2ItemById`, `updateProjectV2ItemFieldValue`) | **N for user Project** (org only: `Projects: write` [S6]); Y if Project is org-owned [S11] | **N for user Project** [S8]; org only [S7] | Y — `project` scope [S9][S11] + Write on the Project [S12] | Y |
| O7 PR create/edit/ready/list/view/checks/comment | Y — `Pull requests: write` [S6] | Y on own/org repo only [S8] | Y — `repo` | Y |
| O8 review-comment post / reply | Y — `Pull requests: write` [S6] | same as O7 | Y — `repo` | Y |
| O9 `resolveReviewThread` | Y — App tokens are accepted by GraphQL [S11]; mutation "Marks a review thread as resolved" [S23]; ? no per-mutation permission documented (write access to the PR is the practical requirement) | ? — GraphQL support for fine-grained PATs is not documented in [S11]; collaborator-repo gap applies [S8] | Y — `repo` | Y |
| O10 `gh pr merge --admin` | Y for merge (`Contents: write` [S6]); **?** for `--admin` bypass — bypass is tied to "admin permissions to the repository" [S13]; an App can be a rulesets bypass actor but that is not documented for classic branch protection | N on a collaborator repo [S8] | Y — `repo`, machine user must hold **admin** on the repo and `enforce_admins` off [S13][S18] | Y — today's path |
| O11 `gh run view` | Y — `Actions: read` [S6] | Y on own/org repo | Y — `repo` | Y |
| O12 HTTPS push / force-push | Y — `Contents: write`, `x-access-token:TOKEN` [S3]; `Workflows` permission to touch `.github/workflows` [S10] | N on a collaborator repo [S8] | Y — `repo` (+ `workflow` for workflow files [S9]) | Y |
| O13 push triggers `on: push`/`pull_request` CI | **Y** — only `GITHUB_TOKEN` events are suppressed [S4] | Y [S4] | Y [S4] | Y |
| Satisfy code-owner review (`* @benjr70`) | N — author cannot approve own PR [S14]; approver must be the code owner [S13] | N | N | N (PR author) — and the maintainer reviewing their "own" PR is exactly the blindness the ticket names |
| Distinct from the human (author/assignee checks work) | Y — `<app>[bot]` [S3] | Y | Y | N |
| Token lifetime | 1 hour, re-mint from ≤10-min JWT [S1][S2] | non-expiring allowed [S8] | non-expiring allowed; removed after 1 year unused [S8] | same |
| gh consumption | `GH_TOKEN` env [S20]; `gh auth setup-git` for git [S22] | `GH_TOKEN` (manual warns against `--with-token`) [S19] | `gh auth login --with-token` or `GH_TOKEN` [S19][S20] | same |

## 4. Token lifetime and refresh for a long-lived Daemon

- **App**: every Fire (and any git push) needs a token younger than one hour
  [S1]. The Daemon would need a small minting step (JWT [S2] → installation
  token [S1]) at Fire start, exporting `GH_TOKEN` [S20] and configuring git
  to read it (`gh auth setup-git` makes gh the credential helper [S22], which
  serves the current `GH_TOKEN`). A Fire that runs longer than an hour (CI
  babysit is capped at 45 min per `ci-wait`, but a full pick → verify Fire
  is longer) must re-mint mid-Fire, or the wrapper must mint per subprocess.
  `gh auth token` prints the active token [S21] but does not refresh it.
- **PAT (classic or fine-grained)**: no refresh logic; one secret in
  `~/.config/agent-daemon/env`. Classic PATs are auto-removed after a year
  of no use [S8], which a daily Daemon never hits.
- **Rate limits**: a machine-user PAT gets its own 5,000/hour [S17] instead
  of sharing the maintainer's; an App installation's limit scales with
  users/repos [S17].

## 5. Answers to the ticket's specific questions

| Question | Answer |
|---|---|
| Can an App token list/edit Projects v2 items and fields? | Only for organization-owned Projects (`Projects` org permission, `/orgs/{org}/projectsV2/...`) [S6]. Nothing documented for user-owned Projects; [C1] reports they are not returned. |
| Can a machine-user PAT? | Classic: yes, `project` scope covers "user and organization projects" [S9], plus Project Write collaborator [S12]. Fine-grained: no for user-owned Projects [S8]. |
| Create sub-issues / dependencies? | App and fine-grained: `Issues: write` [S6][S7]. Classic: `repo`. |
| Open PRs, review comments? | App / fine-grained: `Pull requests: write` [S6][S7]. Classic: `repo`. |
| `resolveReviewThread`? | GraphQL accepts App installation tokens and classic PATs [S11]; the mutation exists in the live schema [S23]. Fine-grained PAT GraphQL support is not documented on the Projects page [S11]. |
| `gh pr merge --admin`? | "Use administrator privileges to merge a pull request that does not meet requirements" [S18]; needs repo admin and the protection not applied to admins [S13]. Straightforward for a classic-PAT machine user given admin; not documented for an App on classic branch protection. |
| Do App-token pushes trigger `on: push`? | **Yes.** Only `GITHUB_TOKEN` events are suppressed; Apps and PATs are the documented workaround [S4]. |
| Satisfy code-owner review? | No identity that authored the PR can [S14]; the code owner (`@benjr70`) must approve [S13]. Unchanged from today, and now the human's approval would be a real second-party review. |
| Token lifetime / refresh? | App: 1 h [S1]; PATs: as configured, non-expiring allowed [S8]. |
| How does `gh auth` consume each? | `GH_TOKEN`/`GITHUB_TOKEN` env for all three [S20]; `--with-token` documented for classic PATs, discouraged for fine-grained [S19]. |

## Implications for the Identity decision (#14)

1. **For a user-owned Target Project with a user-owned Project board (the
   Smart-Smoker shape), the only documented identity that covers every row
   is a classic PAT on a dedicated machine user** with scopes
   `repo, project, workflow`, added as an **admin** collaborator on the repo
   (for `--admin` merges) and a **Write** collaborator on the Project. It is
   a drop-in for today's token; the only harness change is that `gh api user`
   returns the machine login, which is what the human-vs-agent checks want.
   Cost: one extra GitHub account (ToS-permitted, one per person [S16]) and a
   non-expiring secret to rotate by hand.
2. **A GitHub App is the better long-term identity only if the Target
   Project's Project board is organization-owned** (or the harness stops
   depending on Projects v2 for its pick signal). It gives a `[bot]` actor,
   short-lived tokens, no seat, and its pushes do trigger CI, but it costs a
   JWT → installation-token minting loop every Fire plus mid-Fire refresh,
   and its `--admin` merge path is undocumented for classic branch
   protection.
3. **Fine-grained PATs are out** for any Target Project the machine user
   does not own or belong to as an org member [S8]; they only fit an
   org-hosted Target Project, where an App is the stronger choice anyway.
4. **The Harness config should treat identity as a pluggable "token source"**
   (static PAT vs App minting), because the right answer flips on whether the
   Target Project is a personal repo or an organization repo. Setup (#1's
   Setup term) needs a branch for each.
5. **Code-owner review stays a human step** under every option; the machine
   merges keep using `--admin` on a `enforce_admins`-off protection, so the
   machine identity must be a repo admin either way.
6. **Correct the ticket text / map note** that "App-token pushes do not
   trigger `on: push`": per [S4] only `GITHUB_TOKEN` pushes are suppressed.
