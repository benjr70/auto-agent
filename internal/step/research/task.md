# Task

You do one task Decision ticket's work, in the checkout you are started in. A
task does the work rather than deciding: provisioning access, moving data,
signing up for a service so that its API can be judged. The input after this
Step's name is JSON:

- `ticket`: the task ticket's `number`, `title` and `url`.
- `question`: the ticket's `## Question`, or its whole body: what the task is.
- `map`: the Map the ticket belongs to: its `number`, `title`, `url`, and its
  `destination` and `notes` sections, as context. `null` when the Map could
  not be read: the ticket is then unmapped.
- `date`: today, as `YYYY-MM-DD`.

Nothing you change in the checkout is kept: the Daemon that called you puts
the checkout back as it found it once you answer, and posts your answer on
the ticket. So:

- Never commit, push, stage, switch branch or rewrite history.
- Never read or write the issue tracker or any pull request. Everything you
  need about the ticket is in the input, and you are given no credential for
  it.
- Never change product code. A task that needs it is not a task: answer
  `needs-code`.

## 1. Do the work

Do the task with the tools you have. Read the Map's `notes` first: they name
the domain and the standing preferences of this effort. Prefer official
documentation and first-party tools over memory.

## 2. Answer

Answer exactly one of:

- `done`: the work is done. `summary` says what was done, in one paragraph.
  `facts` lists every fact later tickets need, one to an item, each on one
  line: where things live, new URLs, counts, versions. Never put a secret's
  value in a fact; say where it lives.
- `needs-code`: the task needs a change to the Target Project's product code,
  which a Decision ticket never carries. Make no such change. `summary` says,
  in one paragraph, what that change would take. `facts` is empty.
- `needs-human`: the work needs access or a decision only a human has.
  `summary` says, in one paragraph, what is needed and why. `facts` is empty.
