# What a webhook trigger costs when the Daemon has no inbound network

Research for [#91](https://github.com/benjr70/auto-agent/issues/91), part of map
[#90](https://github.com/benjr70/auto-agent/issues/90). Researched 2026-10-11.

## Answer

Keep polling as the source of truth. Make it cheap with conditional requests
(ETags), which lets the interval drop from 300 s to about 60 s at no rate-limit
cost. Treat any webhook path as an optional nudge that only shortens the sleep,
never as the thing that decides whether work exists.

The reasons, each sourced below:

- Every way of receiving a webhook without inbound network needs a third party
  in the middle, and both first-party-documented ones (`gh webhook forward`,
  smee.io) are documented as not for production.
- GitHub does not retry a failed delivery. A Daemon that was asleep, restarting
  or offline loses the event unless it runs a reconciling sweep, which is
  polling again.
- The Work Probe wakes on state (is there a pick candidate, is a PR
  reconcilable), not on events. Mapping it to events needs seven webhook event
  types and still needs the state read afterwards.
- Polling needs nothing from the operator beyond the token the Daemon already
  has. Every webhook option adds admin rights, an account, an app, or a
  listener.

## What the Work Probe reads today

`lib/work-probe.sh` runs four `gh` list calls per scan (open PRs, `AFK:paused`
issues, open `AFK` issues, open `wayfinder:map` issues) plus the lock read, and
`pr_triage_enrich` adds per-PR comment and file reads. `lib/daemon.sh` runs a
scan every `AUTO_AGENT_WORK_PROBE_INTERVAL` seconds (default 300) while
sleeping. So a human's Ruling or a new `AFK` label waits up to five minutes
plus the Fire start-up. That latency is the only thing a webhook would buy.

The wake rules, and the GitHub change behind each:

| Wake rule | Underlying change | Webhook event (action) |
|---|---|---|
| New pick candidate | `AFK` label added, state label removed, issue opened or reopened with `AFK` | `issues` (`labeled`, `unlabeled`, `opened`, `reopened`, `closed`) |
| `AFK:paused` issue | state label added | `issues` (`labeled`) |
| Open-PR set shrink | PR merged or closed | `pull_request` (`closed`) |
| Reconcile: revise | review submitted, review comment, thread resolved | `pull_request_review`, `pull_request_review_comment`, `pull_request_review_thread` |
| Reconcile: ruling | human replies on the PR | `issue_comment` (`created`); PR comments arrive as this event |
| Reconcile: conflict | base branch moved, mergeability changed | `push` to the base branch; there is no mergeability event |
| Reconcile: incomplete, docs-merge, Dependabot | PR opened, pushed, labelled, CI finished | `pull_request` (`opened`, `synchronize`, `labeled`, `ready_for_review`), `check_suite` (`completed`) |
| Lock held | `AFK:in-progress` label | `issues` (`labeled`, `unlabeled`) |

Action lists are from the
[webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)
reference. Two things in it matter here:

- Labelling does not fire the `label` event. It "is reported through the
  `labeled` or `unlabeled` actions" on `issues` and `pull_request`.
- Repository webhooks receive only `completed` for `check_suite`, and only
  `created` and `completed` for `check_run`.

So a full replacement subscribes to `issues`, `issue_comment`, `pull_request`,
`pull_request_review`, `pull_request_review_comment`,
`pull_request_review_thread`, `push`, and `check_suite`. Mergeability has no
event at all: GitHub computes it asynchronously and `pr-triage.sh` already
"rides out" that with a re-read. Blocker closure and Project membership, which
the probe comments say it "cannot cheaply check", are not made cheaper by
events either.

The simpler reading is that the Daemon does not need the payloads. Any
delivery for the repo means "scan now". That is the nudge design.

## Options

### 1. Plain polling with conditional requests

What it is: the same list calls, sent with `If-None-Match` and the saved ETag.

- GitHub: "Most endpoints return an `etag` header", and "Making a conditional
  request does not count against your primary rate limit if a `304` response
  is returned" when the request is authorised
  ([REST best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)).
- The same page says "You should subscribe to webhook events instead of
  polling the API for data", and where polling is needed to honour
  `x-poll-interval` when present.
- Primary limit is 5,000 requests per hour for a user token; a GitHub App
  installation token gets at least 5,000
  ([rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)).
  Unconditional, a five-call scan every 60 s is 300 requests per hour, 6% of
  the budget, before per-PR enrichment.

Operator needs: nothing new. No account, app, port or admin right.

Reliability: as good as outbound HTTPS to api.github.com. Nothing can be
missed, because each scan reads current state. Latency is the interval.

Checked live against this repo on 2026-10-11:

- `GET /repos/{owner}/{repo}/pulls` and `/issues` return a strong `ETag`;
  replaying it in `If-None-Match` returned `304 Not Modified`.
- Over two `200`s and three `304`s, `X-RateLimit-Used` went 16, 17, 17, 17,
  18. That is one increment not explained by the `200`s. The token is shared
  with the live Daemon on this box, so the stray increment is probably its
  probe, but that was not isolated. See "Not verified".

Cost for the Go rewrite: the saved ETag per URL is a small piece of state, and
`gh` does not manage it, so the binary calls the REST API directly for these
reads.

### 2. Conditional polling of the repository events feed

What it is: `GET /repos/{owner}/{repo}/events` as a single change detector,
then a full scan only when it changes.

From the [events API reference](https://docs.github.com/en/rest/activity/events):

- "Events are optimized for polling with the "ETag" header", and an
  `X-Poll-Interval` header "specifies how often (in seconds) you are allowed
  to poll. In times of high server load, the time may increase. Please obey
  the header."
- "This API is not built to serve real-time use cases. Depending on the time
  of day, event latency can be anywhere from 30s to 6h."
- The timeline holds at most 300 events from the past 30 days.
- The event types are a fixed list: `IssuesEvent`, `IssueCommentEvent`,
  `PullRequestEvent`, `PullRequestReviewEvent`,
  `PullRequestReviewCommentEvent`, `PushEvent`, `CreateEvent`, `DeleteEvent`
  and a few others. There is no check-suite or commit-status event, so CI
  finishing is invisible to it.

Checked live: the feed returned `X-Poll-Interval: 60` and a weak ETag, a
replay returned `304`, and label and assign actions from 90 seconds earlier
were present as `IssuesEvent`.

Operator needs: nothing new.

Reliability: not suitable as the only signal. The documented latency ceiling
of six hours is worse than the current five minutes, and it cannot see CI. It
is usable only as one more cheap "something changed" hint beside option 1,
and option 1's ETags on the real list endpoints already give that without the
latency caveat.

### 3. `gh webhook forward`

What it is: a `gh` extension that creates a repository webhook and receives
its deliveries over an outbound websocket.

From GitHub's
[forwarding guide](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/using-the-github-cli-to-forward-webhooks-for-testing):

- "Webhook forwarding is only designed for use during testing and
  development. It is not supported for use in production environments for
  handling live webhooks."
- "Only one person can use webhook forwarding at a time for each repository
  and organization." A second user gets "Hook already exists".
- Works only with repository and organization webhooks.
- Installed with `gh extension install cli/gh-webhook`; it is not in core
  `gh`.

From the extension source
([cli/gh-webhook](https://github.com/cli/gh-webhook), `webhook/create_webhook.go`
and `webhook/forward.go`):

- It `POST`s `repos/{repo}/hooks` with `"name": "cli"` and reads a `ws_url`
  from the response, then dials that websocket with the `gh` token. The hook
  is created inactive and activated once connected.
- A `403` is reported as "you do not have access to this feature".
- On an abnormal websocket close it retries three times, five seconds apart,
  then exits with "unable to connect to webhooks server, forwarding stopped".

Operator needs:

- Admin on the Target Project for the Daemon's token, since "You must be a
  repository owner or have admin access in the repository to create webhooks"
  ([creating webhooks](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks)).
  The Machine user is a collaborator today, not an admin.
- No inbound port and no extra account.
- The extension installed, or the Go binary reimplementing the `cli` hook and
  websocket protocol, which has no published contract.

Reliability: a supervised process that gives up after about 15 seconds of
disconnection, a one-forwarder-per-repo limit that collides with a developer
using the same feature, and an explicit "not supported" for production. The
Go binary could speak the protocol itself and reconnect forever, but it would
be building on an undocumented interface that GitHub says not to rely on.

### 4. A relay: smee.io or a self-hosted equivalent

What it is: the webhook points at a public relay URL; a client holds an
outbound Server-Sent Events connection to the relay and replays payloads to
localhost.

From the [smee.io README](https://github.com/probot/smee.io):

- "Smee.io is intended for use in development, not for production."
- "Webhook payloads are never stored on the server, or in any database; the
  Smee.io server is simply a pass-through." It sends a payload "to any
  actively connected clients".
- "channels are not authenticated, so if someone has your channel ID they can
  see the payloads being sent."
- It can be self-hosted: `docker run -p 3000:3000 ghcr.io/probot/smee.io`.

GitHub's own GitHub App guide repeats that Smee is for development only
([using webhooks with GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/using-webhooks-with-github-apps)).

Operator needs:

- A webhook on the repo (admin, as above) or a GitHub App (option 5).
- A channel URL on the public smee.io, or a host with a public address to run
  their own relay, which contradicts the premise for anyone without one.
- A webhook secret, because a channel URL is the only thing guarding the
  payloads. With a nudge-only design the payload is discarded, so a leaked
  channel exposes repo activity but cannot inject work.

Reliability: best effort. A payload that arrives while the client is
disconnected is not stored by the relay; the README implies it is dropped,
though it does not say so in those words. GitHub still records the delivery as
successful because the relay answered, so GitHub's redelivery API will not
show it as failed.

The client is an npm package ([probot/smee-client](https://github.com/probot/smee-client)).
A Go binary would implement the SSE client itself, which is small.

### 5. A GitHub App

What it is: an App registration with one webhook URL that receives events for
every repo it is installed on.

- "GitHub Apps have built-in, centralized webhooks"
  ([deciding when to build a GitHub App](https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/deciding-when-to-build-a-github-app)).
- The App webhook is optional and is a URL: "the address of a web server that
  will receive the webhook event payloads". Events available depend on the
  permissions granted
  ([using webhooks with GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/using-webhooks-with-github-apps)).
- Permissions for the events in the table above are read-level Issues, Pull
  requests, Checks and Metadata
  ([webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)).

An App does not remove the inbound requirement. Its webhook still needs a
reachable URL, so on a Host with no public address it needs option 4 in front
of it. What an App changes is identity and rate limit, which is a separate
question from the trigger and was covered by the earlier
`research/github-identity-for-daemon` ticket.

Operator needs: register an App, generate and store a private key, install it
on the repo, and still provide a relay or public endpoint.

Reliability: same delivery rules as any webhook, below.

## How reliable webhook delivery is, whatever the transport

- "GitHub does not automatically redeliver failed deliveries." A delivery
  fails if the server "is down or takes longer than 10 seconds to respond"
  ([handling failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)).
- Recovery is the receiver's job: list deliveries and `POST` to the
  `/attempts` endpoint. GitHub's example runs this on a schedule. It needs
  write access to repository webhooks, or the classic `repo` scope
  ([automatically redelivering](https://docs.github.com/en/webhooks/using-webhooks/automatically-redelivering-failed-deliveries-for-a-repository-webhook)).
- "You can redeliver webhook deliveries that occurred in the past 3 days."
  Only repo admins, or the App owner, can do it
  ([redelivering webhooks](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/redelivering-webhooks)).
- A redelivery carries the same `X-GitHub-Delivery` as the original, so the
  receiver must deduplicate
  ([best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)).
- Payloads over 25 MB are not delivered
  ([webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)).

So a webhook-only Daemon has to poll anyway: either the deliveries API to find
what it missed, or the state itself. Polling the state is simpler and is what
the Work Probe already does.

## Comparison

| | Operator must provide | Third party in path | Missed-event behaviour | Latency |
|---|---|---|---|---|
| Polling with ETags | nothing new | none | cannot miss; reads state | the interval (60 s is affordable) |
| Events feed | nothing new | none | cannot miss, but no CI events | documented 30 s to 6 h |
| `gh webhook forward` | repo admin for the token; extension | GitHub's forwarder (unsupported for production) | lost while disconnected; process exits after 3 retries | seconds |
| Relay (smee) | repo admin or an App; a channel or a public relay host | smee.io or self-hosted | lost while disconnected, and GitHub records success | seconds |
| GitHub App | App registration, private key, install, plus a relay | a relay, as above | same as any webhook | seconds |

## Recommendation

1. Build the Go Daemon on polling with conditional requests, at a default of
   about 60 s, directly against the REST API. This needs no operator setup,
   runs anywhere with outbound HTTPS, and cuts worst-case wake latency from
   five minutes to one.
2. Shape the wake path so a nudge is possible later: one internal "scan now"
   signal that the timer fires and anything else may also fire. A nudge source
   never carries meaning beyond "scan now", so its loss or duplication is
   harmless.
3. Do not ship a webhook transport in the first version. If sub-minute latency
   later matters, add a relay client as an opt-in nudge behind that signal. It
   stays optional because it needs repo admin and a relay the operator trusts.
4. Do not build on `gh webhook forward`. It is documented as unsupported for
   live use, allows one forwarder per repo, and its protocol is unpublished.

## Not verified

- Whether a `304` is free in practice. The docs say so. The live check showed
  one unexplained `X-RateLimit-Used` increment across three `304`s, on a token
  shared with a running Daemon. A check on an idle token would settle it.
- Whether the list-endpoint ETag changes for every change the Work Probe cares
  about, for example a new review comment or a CI result on an open PR. The
  PR and issue lists carry `updated_at`, which suggests comments change it,
  but CI results may not. The per-PR enrichment reads may need their own
  conditional requests.
- What smee does with a payload when no client is connected. Dropping is
  inferred from "never stored" and "actively connected clients", not stated.
- No webhook was created on any repo for this ticket. The `gh webhook forward`
  findings are from the docs and a reading of the extension source, not a run.
- Secondary rate limits for a 60 s polling loop. The documented limit is 900
  points per minute for REST, far above this use, but it was not exercised.
- How long GitHub keeps delivery records beyond the three-day redelivery
  window.
