# Review personas

A request's ticket is reviewed by two personas: the **code review** (the
`review` role) and the **challenger** (the `challenger` role, SC-022). They
run at the same time on the same result commit, and what they find reaches
the builder as one brief per round, from one round budget. This page is the
design; the rules are in `safety-contract.md` (SC-022) and the operator view
is in `USAGE_REFERENCE.md` ("The challenger", "Advice rounds").

## The two personas

| | Code review | Challenger |
|---|---|---|
| Reads | The diff and the repository | The approved spec (with its read-back answers), the changed paths, the result commit; never the build's session, handoff, notes or the review's findings |
| Produces | Findings in prose, each with a severity | At most 5 test files |
| Checked by | The next fresh review (a model) | The factory: it runs every held file itself in a gate sandbox, a red twice |
| A later round costs | A new review job | A re-run of the frozen files, no model call |
| Can refuse | No: findings are advice | No: a red test is advice |

The two read different things on purpose, and neither sees the other's
output: their errors stay independent. They run on different models where
the session config gives them different ones (`quickstart` on the
chatgpt-codex route: `gpt-6-luna` reviews, `gpt-6.1-terra` challenges).

## One build: both personas, then one verdict

```text
build -> verify -> repository gates -> evidence -> tests_red_on_base -> size
                                                                         |
                               every gate passed and within the size caps?
                                                                         |
                       +-------------------------------+-----------------+
                       |                               |
                code review job                 challenger job
                                                  hold -> run (red twice)
                       |                               |
                       +--------------- join ----------+
                                        |
                review verdict and challenge green -> commit the tests
                                        |
                                    evaluate
```

- Both jobs start once every gate before the review has passed and the
  change is within the size caps, the rule the challenger had after the
  review. The review no longer has to pass first: the challenger's start
  does not wait for it.
- The order is chosen per run, on the host, from Docker's memory: together
  only where it holds both sandboxes of every parallel job at their limits
  (`max_parallel_jobs` × 2 × `sandbox_memory`), else the challenge phase runs
  after the review (a Docker VM of 4 GiB on a default laptop). A memory that
  cannot be read means after. The choice is a run input
  (`ChallengeWithReview`), not a key; the run's challenge record names the
  order as `challenge.with_review`, and `factoryd doctor`'s memory row counts
  sandboxes by the same rule. Overlap only saves time: the round loop is what
  makes the result correct.
- The join waits for both. The review keeps its own limit and the challenger
  keeps its own launch deadline, so one stuck job never holds the other's
  result past its limit.
- The green tests are committed only when the review gave a verdict, as
  before. A review with no verdict leaves the run in `resume_review`; the
  challenge record is kept on the run, the ticket is not marked challenged,
  and the resumed build challenges afresh.
- A challenger with no verdict (a stated `challenge.cause`) does not change
  the review's outcome.
- The change sits behind its own `GetVersion` marker in `RunWorkflow`; a
  history recorded before it replays the serial order.
- A run's own worker runs at most two Activities at once
  (`workflow.RunWorkerOptions`), and only these two use it; a task queue
  shared by several runs keeps one. A ticket build can so hold two sandboxes,
  which `factoryd doctor`'s memory row counts when `roles.challenger` is set.

## One loop: the rounds after the pull request opens

The challenge round and the advice rounds are one loop. Each round is one
build on the ticket's branch, given one brief.

```text
accepted run, pull request open
        |
        v
  open items on the head?  red held tests  +  high review findings
        |                         |                     |
        no ---> accept            +---- one brief ------+
        |                                  |
        |                     round build (gates + fresh review + held tests re-run)
        |                                  |
        |              accepted, and no held test that was green on the head fails?
        |                     yes: push, the round's run is the new head
        |                     no:  restore the branch, post the advice
        |                                  |
        +---------- next round, while the budget lasts ----------+
```

| Rule | What the factory does |
|---|---|
| Open items | The head run's red held tests (from its challenge record, a re-run included) and its `high` review findings |
| Brief | One record file: the red tests first (path, criterion, failing test names, never content), then the review findings (at most 5, by severity). Where a finding and a red test name the same file, the brief says so once; agreement does not raise a severity |
| Budget | `review_advice_rounds` (0 to 3, 3 by default) counts every round. A head with a red test gets at least one round even at 0, the one round a red challenge always had |
| Push | An accepted round is pushed unless a held test green on the head is red or broken on the round. A push that failed once sent halts the request, whatever the round was given: the commit may be on the remote |
| Commit of the tests | Every round while the tests are not yet committed runs the held files with the commit allowed; the first round whose held tests are all green commits them on the branch |
| Stop | No open items; the budget is spent; a round not pushed; a launch-budget refusal; or a round with no red test open that left the count of `high` findings no lower than its head |
| Counts against | Neither `review_corrective_rounds` nor `max_review_rounds`; the launch budget, as every launch |

The last stop rule ends rounds that only chase review advice once they stop
making progress: a red test is evidence the factory ran, a finding is an
opinion, and rounds after the red tests are fixed have the least to gain.

## Why

- Time: the challenger is off the critical path. Serially, its job and its
  test runs came after the whole review, and its round came after the
  advice rounds' first wait.
- More than one try on a red test, inside the budget the operator already
  set, instead of one round on the side.
- One brief: a fix for a review finding cannot break a held test the builder
  has not been shown, because it is shown both.
- One loop: no separate challenge-round code path, push rule or comment.

## Compared with a multi-persona review report

Multi-persona review tools such as Every's compound-engineering
`ce-code-review` run several prose reviewers in parallel and merge their
findings into one report, one pass, which the caller acts on. Three ideas
are taken from that shape: parallel personas with one merged output, a
second model as an adversarial peer, and that agreement between reviewers
does not by itself make a finding important. Two are not: a lead agent that
drops findings by confidence (here a model never removes advice; it is
ordered and listed), and personas chosen per diff by a model's judgment
(a later persona, for example a migration reviewer, would be chosen by the
changed paths). What this design adds is a persona whose evidence the
factory executes, and the round loop itself.

## What is measured

A run of the standing walk against the previous one with the serial
challenger: hidden checks passed, probe false finishes, rounds per request
and minutes per request. The personas stay the default only if the first
two hold and the minutes drop.
