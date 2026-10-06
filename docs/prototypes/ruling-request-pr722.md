<!-- PROTOTYPE — the Ruling request Smart Smoker PR 722 should have carried.
     Throwaway: answers "can the operator rule without opening the diff?" -->

<!-- auto-agent:ruling-request head=2e204022 decisions=2 -->
## 🧑‍⚖️ Ruling request · 2 decisions

Reply with one line, one letter per decision: `1A 2A`. Nothing else in the reply is read.

### 1. Cook log stamp time

The cook says "I just wrapped it", leaves the review list up for two minutes, then taps Fill.

- **Now:** the stamp is logged at the moment of Fill, on the backend's clock. Never backdated.
- **Reviewer wants:** the entry stamped at the Ramble's time (AC 5, "at the time of the Ramble").
- **Why this is yours:** AC 5 and Spec #700's "backend untouched" cannot both hold; the cook sees a different time on the cook-log entry either way.

| | Option | What it costs |
|---|---|---|
| **A** | Fill time. Keep as is. | Nothing. Seconds of drift in the normal flow. _Recommended._ |
| **B** | Ramble time, later. | This PR merges as is; a new Slice adds an optional time to `POST cook-events`. |
| **C** | Ramble time, in this PR. | Backend change inside a frontend Slice; verification re-runs. |

### 2. Serve time or rest spoken with no Serve Plan card

The planner is off; the cook says "rest it 45 minutes".

- **Now:** goes to Notes; the shared rest duration is not written.
- **Reviewer wants:** written to the Serve Plan anyway (stories 40 and 43 say the Ramble sets serve time and rest).
- **Why this is yours:** Spec #700 says both "a Ramble fills only the screen it was spoken on" and stories 40/43; the cook sees either a filled rest or a Notes line.

| | Option | What it costs |
|---|---|---|
| **A** | Notes. Keep as is. | Nothing. _Recommended._ |
| **B** | Write the Serve Plan value even with the card hidden. | Fix in this PR; verification re-runs. |

---

Head `2e204022`: CI green, manual verification 6/6. A reply of `1A 2A` leaves the PR ready to merge with no further Fire.

<!-- ===================== after the human replies "1A 2B" ===================== -->

<!-- auto-agent:ruling-applied head=9f3c1a0 ruling="1A 2B" -->
## ✅ Ruling applied · `1A 2B`

1. **A** — Fill time stays. Thread on `useSmokeScreenBinding.ts:88` resolved, no change.
2. **B** — Serve Plan written with the card hidden. Fixed in `9f3c1a0`; thread on `extractionContract.ts:912` resolved.

Verification re-ran on `9f3c1a0`: CI green, manual 6/6. Ready to merge.

<!-- ===================== in-thread, on each ruled thread ===================== -->

<!-- auto-agent:ruling 1A -->
Ruling 1A: Fill time stays. Resolving, no change.
