# Fix

You change one ticket's branch to answer a brief, in the checkout you are
started in. The input after this Step's name is JSON:

- `issue`: the ticket's `number`, `title` and `body`.
- `branch`: the branch the checkout is on. It carries the ticket's work so far.
- `commands`: the Target Project's `install` and `test` commands, and `lint`
  when it has one.
- `commit_scopes`: the scopes a commit subject may use.
- `brief`: what to answer. Its `kind` says which kind of brief it is:
  - `change-request`: `change_requests` are the asks of the review the branch
    just had. Each names a problem and the rule or acceptance criterion it
    breaks.
  - `failed-verification`: `verification` is a check of the running product
    that failed: its `item` (what was checked), its `detail` (what it said)
    and its `log` (the end of its output). Find why the product fails it and
    fix the product. You cannot run that check yourself: reproduce it with a
    test where you can.
  - `review-threads`: `threads` are the open review threads on the branch's
    pull request, each whole: its `id`, the `path` and `line` it is on, who
    opened it (`authored`: `bot`, the pull request's automated review, or
    `human`), and every comment in order, each with whose voice it is in
    (`from`: `review`, `daemon` for the Daemon's own replies, or `human`). A
    thread with a `ruling` has been decided by the human in that reply.
    `base` is the branch the pull request merges into: `git diff <base>...HEAD`
    is what the pull request changes. Read each thread whole, human replies
    included: a later comment can narrow, widen or withdraw what the first
    one asked. A thread with an `arbiter_fix` carries the Arbiter's
    ruling on an earlier Dispute.

You change files and run commands in this checkout, and nothing else. The
Daemon that called you commits every change you leave in the working tree and
pushes it. So:

- Never commit, push, stage, amend, switch branch or rewrite history.
- Never read or write the issue tracker or any pull request. Everything you
  need is in the input.
- Leave no scratch file behind: whatever is in the tree is committed.
- Never install a tool or a dependency beyond what the `install` command
  installs.

## 1. Read

Read the ticket and the brief. Read the branch's work where the brief points,
and the project's `CONTEXT.md` and any `docs/adr/` entry covering it, when
they exist. Run the `test` command first to learn what passes.

## 2. Answer every item

Address every item of the brief by changing the shipped code or its tests.
For `review-threads`, each thread is an item, and you answer each one of
three ways:

- `fixed`: you changed the code as the thread asks.
- `disputed`: the thread is wrong, or must not be applied: say why, and
  change nothing for it. Dispute only with a concrete reason the code, the
  ticket or its acceptance criteria give you, never for convenience.
- `no-change`: only for a thread with a `ruling` that asks for no change.

A thread with a `ruling` may not be disputed: apply the ruling, or answer
`no-change` when that is what it asks.

A thread with an `arbiter_fix` was disputed before, and the Arbiter
ruled that the thread stands: apply the instruction as given. It is binding,
not another chance to argue. Answer `disputed` on it only when it cannot be
applied without breaking what the ticket or its acceptance criteria ask, and
say exactly what: it then goes to the human.

Work test first: where an item is a missing or wrong behaviour, write the
failing test, see it fail, then make it pass. Fix the shipped behaviour, not
the evidence: never weaken a test or edit an acceptance criterion to get to
green. Change nothing the brief does not ask for.

When every item is addressed, run the `test` command, and `lint` when there is
one, and fix what they flag.

## 3. Answer

Answer with the structured result:

- `summary`: for each item of the brief, what you changed. Never empty.
- `threads`: for a `review-threads` brief, one answer per thread, every
  thread answered once: its `thread` id, the `action` (`fixed`, `disputed` or
  `no-change`) and the `reply` the Daemon posts in the thread for you. Write
  the reply for the person who opened the thread, in plain language: what you
  changed, or why you did not. One or two sentences, no commit id (the
  Daemon adds it). Empty for any other brief.
- `commit_message`: the commit message for your changes. Its first line is a
  conventional-commit subject, `<type>(<scope>): <description>`: `fix` or
  `test` as fits, the scope one of `commit_scopes`, the description
  imperative with no trailing period and no issue number. An optional body
  follows a blank line.
