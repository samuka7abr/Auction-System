# bid-storm

Three concurrency strategies for the same problem, measured side by side.

This is not an auction project. It is a project about **contention under write pressure**, using an auction as the workload.

---

## The problem

An auction has a property almost no CRUD system has: extreme contention on a **single row**.

A thousand people bidding on the same item in the last second is the worst concurrency case available. Every write competes for the same record, every one of them must read the latest state before deciding, and none may be lost or applied twice.

The question the whole repository exists to answer:

> **Which concurrency strategy sustains throughput and latency when N clients fight over the same auction, and exactly where does each one break?**

The deliverable is not a working auction. It is **the graph where the three curves cross as contention rises**.

---

## The hypothesis

| Strategy | Mechanism | Cost per bid | Expected behaviour |
| --- | --- | --- | --- |
| **Optimistic** | Compare-and-set on a version column; the loser gets a conflict and retries | One round-trip, plus N retries | Wins at low contention and with many parallel auctions; collapses when contention is high |
| **Pessimistic** | Row lock held inside an explicit transaction while the decision is made | One round-trip, plus lock wait | Stable, but holds a pool connection and caps concurrency at the pool size |
| **Single-writer** | One goroutine exclusively owns each auction; bids arrive over a channel and commit in batches | One channel send, plus one batched commit | Wins at high contention: no conflict, no retry, no lock |

The third is the project's bet. If every auction has exactly one writer, serialisation becomes structural — there is no race to resolve because there is no race. The database stops being the synchronisation point and becomes durability only.

The optimistic strategy is the one every tutorial reaches for, and the hypothesis is that it is the worst possible choice at the point where it matters most.

---

## What keeps the comparison honest

Three strategies are only comparable if everything else is identical. Four method invariants hold that line, and each exists because the alternative would produce a graph that is beautiful and false.

| Method invariant | What breaks if it is violated |
| --- | --- |
| A success response means **durable** in all three engines | The single-writer wins by having promised less, not by being better |
| The client behaves **identically** against all three | The engines receive different loads and "accepts per second" stops comparing anything |
| Pool, CPU and memory **fixed and equal** across all three | You measure your configuration and call it a result |
| Every cell starts from the **same state** | The last strategy in the list looks worst through ordering effects alone |

---

## How the experiment is shaped

### The axes

The measurement is a matrix of **36 cells**: three strategies × three contention levels × two load scenarios × two retry policies.

- **Contention** is the independent variable: the same offered load spread over 1, 10 or 1000 auctions. One auction is total contention; a thousand is almost none. This axis is what reveals the crossing point.
- **Scenario** is either a ramp that climbs to 500 concurrent clients over two minutes, or a spike that drops 1000 concurrent clients at once for fifteen seconds — the last-second sniping case.
- **Retry policy** is immediate retry or exponential backoff with full jitter.

Retry policy is an axis rather than a constant because of the strongest criticism the project can receive: *"your optimistic engine collapsed because you retried without backoff — with jitter it survives and your thesis falls."* That criticism is valid, so both policies are measured. If backoff saves the optimistic engine, the hypothesis is falsified — and that is a result, not a failure.

### What is constant, stated precisely

What is identical across contention levels is the **offered** load — same clients, same duration, same bidder. The number of requests actually delivered is *not* identical, and cannot be: under high contention latency rises and each client completes fewer iterations. Forcing equal delivered load would clamp the dependent variable by hand, and the graph would measure the load generator's patience instead of the concurrency mechanism.

### The bidder model

This is the decision that determines whether the graph measures anything at all.

If each client sent a fixed amount, everyone would be outbid after the first round and give up. The conflict rate would collapse toward zero and the optimistic strategy would appear optimal — proving the opposite of the hypothesis by accident of instrumentation.

So the client is **aggressive**: it always wants to win, and re-aims on top of whatever state the rejection returned. Every rejection becomes genuine contention, which is exactly the variable under test. It gives up after a bounded number of attempts or a deadline, whichever comes first, and that give-up rate is a headline number rather than a footnote — under high contention it *is* the optimistic collapse made visible.

A losing bid arrives as one of two different rejections depending on the engine. The client treats both identically, and that is what keeps the offered load equivalent across the three.

---

## What counts as a result

### Invariants, checked after every cell

A verifier runs against the database after each cell and proves eight properties that must hold whichever strategy produced them: dense bid sequences with no gaps, strictly increasing amounts, a recorded winner matching the highest accepted bid, no bid after closing, durability against the client's own count, idempotency keys present and unique, coherent closing, and whether the cell is worth reading at all.

A violated invariant stops the matrix. So does a cell the verifier could not check — and the two are deliberately different exit codes, because one is a finding about the engine and the other is the absence of a finding.

### The independent source

The claim *"every bid that received a success response is in the database"* depends on a number that only exists **client-side**. The load generator exports it and the verifier consumes it.

Comparing against the server's own metrics would be the server checking itself: if the handler lies, the metric lies with it and the invariant passes green. This is the rule the whole harness is built on — no published number ever comes from the process under test.

The same rule governs the aggregator that turns 37 directories into one artefact: it reads the environment record, the client record and the verifier's report, and nothing else. Never the raw generator summary, whose shape is internal to the tool and changes silently between versions.

### The control cell

The first cell is run again as the last one. The aggregator compares the two and publishes the divergence.

Below 10% is noise this project cannot distinguish from drift. Between 10% and 25% the matrix is publishable and the divergence is published beside the graph. Above 25% the matrix is refused: a gap that large means the distance between two curves could be explained entirely by execution order, and the deliverable stops meaning anything.

The control block is published even when it passes. A control that only appears when it fails is a control the reader cannot tell ran.

### Refusing to publish

Roughly half the aggregator exists to reject its own matrix. Nine conditions block publication — a missing cell, a directory the plan never asked for, a cell that was never verified, a chaos cell mixed in with healthy ones, cells built from different commits or a dirty tree, a cell whose recorded engine disagrees with its own directory name, a pool size that drifted, a missing measurement window, zero accepts.

Five of those are statements the per-cell verifier is structurally unable to make: it checks one cell at a time against a freshly reset database, and cannot know which directory the cell landed in or what the other 36 did. The worst case is a perfectly green cell that ran against the wrong engine — a plausible, false row, and the only layer that can see it is the one that reads all 37 together.

All refusals are reported at once, never one per run. A matrix with three problems that reports one at a time costs three runs to find the third, and each run that needs cells re-measured costs hours.

---

## Failure, on purpose

Correctness under load is not the same claim as correctness under load *while things break*. A separate stage kills the closing worker mid-processing, kills the API process, freezes Redis, and saturates the connection pool — all from outside the processes, while real load is running.

Two verdicts loosen under injected failure, and only two: the error rate stops failing the cell, because under an injected failure the errors *are* the injection; and an injection that never landed starts failing it, because a chaos cell that broke nothing is the worst result available — it looks like proof and is not.

Chaos cells are marked in their own artefacts and can never enter the matrix. A cell with a process killed inside it measured a different system.

---

## What the system is made of

An HTTP API process serving bids through a pluggable engine interface, a worker process that materialises auction closings from a durable stream, PostgreSQL for state and durability, Redis for idempotency and the closing stream, and a load generator driving the whole thing. Metrics are scraped throughout, but no metric is ever the source of a published number.

The three strategies sit behind one interface, so swapping them changes an environment variable and nothing else. Before any cell is measured, the harness asks the running process which engine it is actually running — not what the configuration requested — because a cell run against the wrong engine produces a plausible, false row and leaves no trace.

---

## Status

The system, its proofs and its chaos scenarios are built. The matrix run is the instrument that turns them into data, and publishing the numbers and the crossing-point graph is the stage after that.

Measurement artefacts are deliberately not versioned. What becomes durable is the *read* result — the filled table, the graph, and the text explaining them — reviewed as prose rather than as shell.
