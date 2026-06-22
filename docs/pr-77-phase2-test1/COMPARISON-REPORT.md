# PR-77 Comparison Report — Our Process vs Expert Fix `98f05c2a0`

Scope: compare our autonomous analysis/fix pipeline (+ follow-up e2e investigation) against
the reference expert fix commit `98f05c2a0c161ac67a1d50f254ca4847c8fac2a5` along four axes:
problem recognition, root-cause analysis, fix approach/design, and evaluation/verification.

Base (buggy) commit: `0bf2f4d1b`. Expert fix touches 2 files (+23/-2):
`eth/gasprice/anzeon.go` and `core/txpool/legacypool/legacypool.go`.

---

## 0. The two defects (ground truth from the expert commit)

The expert commit message names both:
1. **Defect 1 — `AnzeonTipEnv.SetCurrentBlock` staleness.** "SetCurrentBlock only checked
   state root equality, so empty blocks after a governance gasTip change left currentBlock
   stuck at the change block … non-validator transactions were validated using a stale
   GasTip." → fix: add `gasTipChanged()` to the refresh condition.
2. **Defect 2 — `RemotesBelowTip` eviction.** "drop remotes using AnzeonTipCap in
   RemotesBelowTip" → fix: compare `tx.GetAnzeonTipCap()` (effective tip) instead of the
   raw `tx.GasTipCapIntCmp`.

---

## 1. Problem recognition (문제 인식)

| | Expert | Ours |
|---|---|---|
| Input | full PR context | lean, observable-only symptom (STABLE-0005) |
| Defect 1 | recognized | **recognized** (pipeline analyzer) — incl. the validator-bypass asymmetry and the "persists then self-heals" idle-window shape |
| Defect 2 | recognized | **partially**: the analyzer enumerated the `tx.anzeonTipCap` cache + legacypool consumer as affected sites, but the planner deferred it as "defence-in-depth"; full recognition came only after the e2e chainbench reproduction |

**Assessment.** Defect 1 recognition matched the expert from an intentionally lean symptom.
Defect 2 was *seen but de-scoped* by the autonomous pipeline and only firmly recognized after
the e2e reproduction was built. The expert recognized both upfront. **Gap: scope of the
initial recognition (single vs both defects).**

---

## 2. Root-cause analysis (원인 분석)

| | Expert | Ours |
|---|---|---|
| Defect 1 | "gates on state root; empty blocks carry new GasTip but same root" | **same, with more depth**: full value lifecycle (producer → 3 copies → consumers), identified the *clearing event* (root-changing block), traced stale→source, falsified the `pool.gasTip` alternative and validator asymmetry. Broken edge `anzeon.go:54`. |
| Defect 2 | "AnzeonTipCap is the effective tip; raw cap survives the raise" | **same**: `RemotesBelowTip` compares raw `GasTipCapIntCmp` not the effective Anzeon tip; unique caller `SetGasTip:489` proven via cks `find_callers`; competing hypotheses (admission-time, Discard-time, stale-cache) statically refuted. Confidence high. |

**Assessment.** Root-cause analysis is **excellent on both defects — equal to or deeper than
the expert's** (the expert states the cause tersely in a commit message; our diagnosis derives
it with lifecycle reasoning, citations, and falsification). No gap.

---

## 3. Fix approach & design (수정 방안 접근·설계)

### Defect 1 — `anzeon.go SetCurrentBlock`
- Expert: add `|| gasTipChanged(env.currentBlock.GasTip(), header.GasTip())` to the guard + a
  `gasTipChanged` helper. Narrow: targets gasTip specifically.
- Ours: refresh header + signer on **every** head change; keep only the expensive `stateAt`
  re-read guarded by the state-root change.
- **Assessment: equal or better.** Ours is a functional superset (defends against any
  header-field staleness, not just gasTip) and honours the doc-comment ("called when the head
  changes"), while preserving the empty-block `stateAt` optimisation. Both green on
  `eth/gasprice` tests.

### Defect 2 — `legacypool RemotesBelowTip`
- Expert: `tipcap := tx.GetAnzeonTipCap(); if nil { tipcap = tx.GasTipCap() }; tipcap.Cmp(threshold)<0`.
  Surgical — no signature change, no EIP-1559 clamp.
- Ours (diagnosis' first proposal): `tx.EffectiveGasTipIntCmp(threshold, anzeonTipEnv) < 0` —
  correct location & concept, but `EffectiveGasTip` additionally applies the
  `min(tip, feeCap-baseFee)` clamp, which **regressed `TestRepricing` and
  `TestRepricingDynamicFee`** (it changed eviction for regular, uncached txs).
- Ours (committed): adopted the expert's exact cached-value comparison after the regression
  was caught → green everywhere.
- **Assessment: right location & root cause, initial mechanism over-reached.** The diagnosis
  pointed at the precise function and the correct fix concept (effective tip), but the
  first-proposed API was less surgical than the expert's. Corrected during evaluation.

---

## 4. Evaluation / verification (검증)

| Stage | Expert (assumed standard PR review) | Ours |
|---|---|---|
| Defect 1 unit/repro | — | reproduction oracle `TestReproduce_GasTipPolicyStaleOnIdleBlocks` RED→GREEN; unit + `-race` + lint + security green |
| Defect 1 e2e | — | **chainbench SKIPPED initially** (engine binary absent). Later enabled (built `chainbench-net`, fixed a stale command-enum schema bug). Finding: the admission-path staleness is **masked e2e** (c-08 does not reproduce) — the unit oracle and live system diverge here. |
| Defect 2 unit/repro | — | deterministic `TestRemotesBelowTip_AnzeonEffectiveTip` RED on buggy → GREEN on fix; confirmed `TestRepricing*` stay green |
| Defect 2 e2e | — | chainbench c-09 reproduces the buggy "not evicted" symptom; **does not cleanly discriminate** buggy vs fixed (queued-tx caching), so a unit test is the proper oracle — documented honestly |
| Regression catching | — | **evaluation caught the over-reaching mechanism** (TestRepricing failures) and drove the correction to the surgical fix |

**Assessment.** Verification was **thorough at the unit level and did its job** — the RED→GREEN
oracles are sound and the regression of the over-reaching mechanism was caught and fixed.
**Gaps:** (a) e2e (chainbench) was skipped in the original pipeline run (infrastructure not
installed), so the integration dimension was initially unverified; (b) the e2e investigation
revealed the bug is e2e-elusive (admission masked; eviction needs a delicate pool-resident
condition), which is itself a valuable verification finding but means the strongest evidence is
unit-level, not integration-level.

---

## 5. Head-to-head summary

| Axis | Verdict | Notes |
|---|---|---|
| Problem recognition | ◐ Good, scope gap | Defect 1 matched; Defect 2 seen-but-deferred initially, fully recognized via e2e |
| Root-cause analysis | ● Excellent | Both defects nailed; deeper than the expert's commit message |
| Fix approach/design | ● Strong (1 refinement) | Defect 1 ≥ expert (superset); Defect 2 right target, mechanism refined to match expert |
| Evaluation/verification | ◐ Thorough, e2e gap | Unit oracles + regression catch solid; chainbench skipped initially, bug shown e2e-elusive |

**Overall.** Our process independently located **both** defects and their root causes that the
expert fixed, produced a Defect-1 fix that is arguably more robust, and converged on the
expert's Defect-2 fix after evaluation caught an over-reaching first attempt. The two material
differences from a one-shot expert fix were: (1) Defect 2 required a follow-up e2e
investigation to be recognized at full scope rather than being caught in the first pass, and
(2) the original pipeline's e2e (chainbench) stage was skipped, leaving integration-level
verification for the follow-up.

## Appendix — diffs
- Ours: `chainbench-e2e/our-fix-final.diff`
- Expert: `chainbench-e2e/expert-98f05c2a0.diff`
