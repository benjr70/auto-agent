# What breaks when the Daemon acts as the operator's own GitHub account

Research for wayfinder ticket #93 (map #90). Read on 2026-10-11 against
`origin/main` at `8ef7d6c`. Read-only: no GitHub write API was called to test a
refusal, so every "GitHub refuses" below is what the docs say, not a reproduced
error.

## Answer

Nothing is fatal by default. GitHub refuses exactly one thing the workflow
names (the operator approving an Agent PR, because they are its author), and
that refusal only blocks merging when the Target Project's default branch
requires approving reviews *and* admins cannot bypass. Everything else is a
harness assumption that degrades quietly: two "is this the human's or the
Daemon's?" checks keyed on login stop discriminating, the human loses GitHub's
notifications for what the Daemon does, and the token's reach widens to
everything the operator can touch.

The live proof that the shape runs: this repo's own Daemon acts as the owner.
`gh api user` on the Host returns `benjr70`, the owner of `benjr70/auto-agent`;
PRs #81 to #89 are all `author=benjr70 mergedBy=benjr70`, with
`reviewDecision` empty and the automated review's inline comments recorded as
`benjr70:COMMENTED` reviews on PRs #82 to #87. `main` has no branch protection
and no rulesets.

## What `doctor` should report

Detection: `gh api user` login equals the repo owner (`slug%%/*`), or, on an
org-owned repo, the operator declares the token is their own. The first test is
what `infra/cutover/soak-check.sh:94` already does.

| # | Breakage | Kind | Severity | Precondition `doctor` can read |
|---|----------|------|----------|--------------------------------|
| 1 | No Agent PR can be approved, so none can merge | GitHub refusal | **Fatal** | default branch requires >= 1 approving review (or code-owner review) AND admin bypass is off (classic `enforce_admins`, or a ruleset with no bypass for the operator) |
| 2 | Same refusal, but the operator merges past it as admin | GitHub refusal | Warning | required reviews on, admin bypass available |
| 3 | The human's approval is not a second-party review; "approve and merge" is just "merge" | GitHub refusal + ADR 0005 premise | Warning (always) | none |
| 4 | An issue the human assigned to themselves is picked by the Daemon | Harness assumption | Warning (always) | none |
| 5 | A PR the human opened by hand on `feat/issue-<n>` or `research/<slug>` is treated as an Agent PR; a docs-only one is admin-squash-merged | Harness assumption | Warning (always) | none |
| 6 | A Ruling reply is only read when the whole comment is a valid Ruling; free text earns no nudge | Harness assumption (already conceded) | Warning (always) | none |
| 7 | GitHub does not notify the operator about the Daemon's PRs, Ruling requests, parks and hand-offs | GitHub behaviour (inferred) | Warning (always) | none |
| 8 | The classic PAT reaches every repo and project the operator can, and shares their 5,000 req/h | GitHub behaviour | Warning (always) | none |
| 9 | Commits, comments and merges are attributed to the operator | Harness behaviour | Info | none |
| 10 | `soak-check` fails `identity` outright when the login owns the repo | Harness assumption | Fatal for that script only | login == owner |

Not broken (so `doctor` should stay quiet about them): posting the automated
review, telling bot threads from human threads, replying in and resolving
threads, the admin-squash merge lanes, approving a Dependabot major bump, and
review requests (the harness never makes one).

## GitHub's side

### Approving your own pull request: refused

> "Pull request authors cannot approve their own pull requests."

Source: [Approving a pull request with required reviews](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews).

The same page gives the escape that makes it non-fatal by default:

> "Repository owners and administrators can merge a pull request even if it hasn't received an approving review"

and [About protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
gives the switch that removes the escape:

> "By default, the restrictions of a branch protection rule do not apply to people with admin permissions to the repository"
>
> "You can enable this setting to apply the restrictions to admins and roles with the "bypass branch protections" permission, too."

Rulesets are stricter by default: bypass is granted, not assumed
("When you create a ruleset, you can allow certain users to bypass the rules in
the ruleset", [About rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets)),
and they layer with classic rules ("all applicable rules are enforced"). The
ruleset's pull request rule counts "approving reviews from people with write
permissions in the repository" or a code owner
([Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)).

So on a solo, user-owned repo where the Daemon is the operator, a required
review can never be satisfied for an Agent PR: the only account with write
access is the author. That is breakage 1 (bypass off) or 2 (bypass on).

Required code-owner review is the same wall: "any pull request that affects
code with a code owner must be approved by that code owner", and the code owner
is the author.

"Require approval of the most recent reviewable push" ("must be approved by
someone other than the person who pushed it") adds nothing for Agent PRs, which
are already unapprovable. It can bite the Dependabot lane: the operator can
approve Dependabot's PR, but once the Daemon pushes a fix commit to that branch
under the same login, the operator is the last pusher. The admin-squash recipe
passes it wherever admin bypass is on.

The REST docs do not document the refusal: `POST .../pulls/{n}/reviews` lists
only "422: Validation failed, or the endpoint has been spammed"
([REST: pull request reviews](https://docs.github.com/en/rest/pulls/reviews)).
The exact error body, and whether `REQUEST_CHANGES` on your own PR is refused
the same way, are not in the docs and were not tested.

### Commenting on, replying in and resolving threads: allowed

> "You can resolve a conversation in a pull request if you opened the pull request or if you have write access to the repository where the pull request was opened."

Source: [Commenting on a pull request](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/commenting-on-a-pull-request).
The operator qualifies on both counts, so does a Machine user. A `COMMENT`
review on your own PR is allowed; this repo's PRs #82 to #87 show it.

"Require conversation resolution" ("Requires all comments on the pull request
to be resolved before it can be merged") is identity-neutral: it holds an Agent
PR until its threads are resolved under either identity.

### Review requests: irrelevant here

The docs say a request needs write access and can go to "a person or team with
read access"; the REST endpoint documents only "422: Unprocessable Entity if
user is not a collaborator"
([Requesting a pull request review](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/proposing-changes-to-your-work-with-pull-requests/requesting-a-pull-request-review),
[REST: review requests](https://docs.github.com/en/rest/pulls/review-requests)).
Neither page says the PR author cannot be requested. The harness never requests
a reviewer (no `--reviewer`, `--add-reviewer` or `requested_reviewers` anywhere
in `lib/`, `plugin/` or `bin/`), so nothing breaks today. The consequence for
the redesign: a review request cannot be the "your turn" signal in own-account
mode, since the only candidate is the author.

### Notifications: the operator's own actions

The docs never say "you are not notified of your own activity" in one sentence.
What they do say:

- email has an opt-in for "Your own updates, such as when you open, comment on,
  or close an issue or pull request"
  ([Configuring notifications](https://docs.github.com/en/account-and-profile/managing-subscriptions-and-notifications-on-github/setting-up-notifications/configuring-notifications)),
  which only makes sense if they are off otherwise;
- the notification `reason` values describe why *you* are subscribed
  (`author`: "You created the thread.", `comment`: "You commented on the
  thread.", `review_requested`, `mention`,
  [REST: notifications](https://docs.github.com/en/rest/activity/notifications)).

Inference: when the Daemon is the operator, every PR opened, Ruling request
posted, issue parked and hand-off comment written is the operator's own update,
so nothing reaches the web inbox and nothing is emailed unless "Your own
updates" is ticked. With a Machine user the same events arrive as someone
else's activity on a watched repo. The web-inbox half of this is inferred, not
quoted and not tested.

### Token reach and rate limit

The classic PAT ADR 0005 mandates carries `repo` ("Grants full access to public
and private repositories ..."), `workflow` and `project` ("Grants read/write
access to user and organization projects")
([Scopes for OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps)).
A classic PAT is not scoped to a repo: on the operator's account it reaches
every repo and board they can, where a Machine user reaches only the repos it
was invited to. All of a user's tokens draw on one budget: "All of these
requests count towards your personal rate limit of 5,000 requests per hour"
([Rate limits for the REST API](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)),
so the Daemon and the operator's own `gh` use compete.

## The harness's side

Every place that reads an identity, and what it does when both are one login.

### Breaks or degrades

**The pick's "never race a human" rule** (`lib/pickup-triage.sh:83-85`, the jq
at `:230-231`; `plugin/skills/afk-pickup/SKILL.md:541-544`). An issue is
eligible when it has no assignee other than the Daemon's login:
`select((($asgn - [$login]) | length) == 0)`. A human claim *is* an assignment
(`--add-assignee @me`, `plugin/skills/wayfinder/SKILL.md:313`;
`afk-resolve/SKILL.md:84-85` does the same as the Daemon). Under one login the
operator's claim reads as the Daemon's own, and an `AFK` ticket the human is
working gets picked. Silent, and the costliest of the set: two workers on one
ticket. (Breakage 4.)

**The Agent PR test** (`lib/pr-triage.sh:59-70`, `author_ok` at `:224-226`,
fed `DAEMON_GH_LOGIN` by `lib/work-probe.sh:119`, `lib/pickup-triage.sh:168`
and `bin/auto-agent:255,259`). A PR is the Daemon's when its branch has a
harness shape and its author is the agent's login. The author half no longer
excludes the human, so only the branch shape (`feat/issue-<n>`,
`research/<slug>`) stands between a hand-made PR and the reconcile loop. For a
`research/` PR that changes only research docs, the outcome is reason
`docs-merge`: `lib/docs-only-gate.sh` hands back the admin-squash recipe and
the PR lands with no human look. (Breakage 5.)

**Ruling replies** (`lib/ruling.sh:88-92`, `:336-347`, `:371-390`;
`lib/pr-triage.sh:510-513`). "The human answers under their own account" is the
rule: comments under the agent's login are never a reply. The shared-account
concession (commit `af40e29`, PR #88) keeps an unmarked comment under that
login as a candidate only when its whole body is a valid Ruling. What is still
lost: a free-text or malformed reply is taken for the loop's own text, so it is
not read and earns no nudge. The human gets silence instead of "that was not a
Ruling". (Breakage 6.)

**`soak-check` identity** (`infra/cutover/soak-check.sh:90-97`). The only place
that tests login against owner, and it fails:
`"${login} owns ${slug}: the Daemon must act as a machine user, never the owner's account"`.
Its `afk-ticket` check at `:133-138` (the merged PR was "opened by the machine
user") passes trivially under one login and proves nothing. (Breakage 10.)

**ADR 0005's premise and the docs that repeat it.** "the human's approval then
counts as a real second-party review" cannot hold: there is no approval.
`README.md:144` ("approve and merge") and `:276` ("Approve and merge, or hand
it back") describe a step GitHub refuses. `CONTEXT.md:147-150`, `README.md:100`
("Never your own") and `afk-pickup/SKILL.md:120` ("never act as a human
account") state the requirement the redesign relaxes. (Breakage 3.)

**Attribution** (`lib/setup.sh:473-481`, `:587`). The checkout commits as
`<login>@users.noreply.github.com`; under the operator's login every Daemon
commit, comment and merge is theirs in history and on their profile.
(Breakage 9.)

### Already identity-neutral

**Setup and the per-Fire login check** (`lib/setup.sh:337-363`,
`lib/pickup-triage.sh:276-282`). Both compare the token's login to
`DAEMON_GH_LOGIN` and nothing else; `_setup_gh_verify` also wants
`permissions.admin`, which an owner has. Neither looks at the repo owner, so an
operator who names themselves as `--gh-login` passes Setup and every Fire
today. `wrong-login` only catches a token that disagrees with the Host env.
This is where the `doctor` warning belongs.

**The automated review** (`lib/review-poster.sh:198`, `:306`, `:383`). It posts
individual inline comments (`pulls/{n}/comments`) and a top-level done-marker
comment. It never submits a review with `APPROVE` or `REQUEST_CHANGES`, so the
self-approval refusal never reaches it.

**Thread authorship** (`lib/thread-reconciler.sh:28-41`, `:88-91`, `:192`;
`plugin/skills/pr-reconcile/SKILL.md:209-229`; `plugin/agents/arbiter.md`).
"Authorship is by marker, never by login", because even a Machine user answers
its own review under one login. A thread is the bot's iff its first comment's
first line is `<!-- pr-review-bot -->`; a reply is the loop's iff its first
line is one of the four `auto-agent:*` markers. The human's unmarked comments
stay human under any login, so "a human-authored thread is never dismissed"
survives.

**Resolving threads** (`lib/thread-reconciler.sh:227-230`,
`resolveReviewThread`). Allowed for the PR opener and for anyone with write
access.

**The merge lanes** (`lib/harness-config.sh:105-118`). `gh pr merge --squash
--admin --match-head-commit` relies on admin bypass, which an owner has
wherever a Machine user admin would.

**Dependabot** (`lib/pr-triage.sh:216-226`, `:640-647`, `:738-740`;
`lib/deps-gate.sh:191-207`, `:256-258`). The author test is the Dependabot app,
not the agent. A `HITL` major bump waits for `reviewDecision == APPROVED`; the
operator is not that PR's author, so they can approve it.

## Could not verify

- The exact API error for self-approval, and whether `REQUEST_CHANGES` on your
  own PR is refused too. The docs state only the approval rule.
- That the PR author cannot be requested as a reviewer. Not in the docs read.
- That own actions produce no web-inbox notification. Inferred from the opt-in
  email setting and the `reason` list.
- Rows 1 and 2 against a live protected branch. This repo's `main` is
  unprotected, so the fatal case is derived from the docs, not observed.
- Org-owned repos, where a second human could approve, were not examined
  beyond what the docs quoted above say.
