# PR-77 (Anzeon dynamic gasTip) — phase2-test1 fix & analysis

Base commit: `0bf2f4d1b` (buggy parent of PR #77). Reference expert fix: `98f05c2a0` (go-stablenet PR #77).

This branch fixes **two** defects in the Anzeon dynamic gas-tip path and bundles the
analysis/reproduction artifacts that located them.

## Symptom
Right after governance changes the gas priority-tip (`gasTip`) policy, non-validator
(unauthorized) accounts' normal transactions are mishandled for a while during the
idle (empty-block) window.

## Root causes & fixes

### Defect 1 — stale `AnzeonTipEnv.currentBlock` (admission / effective-tip path)
`eth/gasprice/anzeon.go` `SetCurrentBlock` gated the header swap on **state-root**
change only. Empty blocks after a governance gasTip change share the prior state root
but carry the updated `GasTip` in their header, so `env.currentBlock` stayed pinned to
the change block (stale gasTip).

**Fix:** refresh the header pointer + signer on **every** head change; keep the
expensive `stateAt` re-read guarded by the state-root change (preserves the empty-block
optimisation without conflating header identity with state identity).

### Defect 2 — `RemotesBelowTip` uses the raw tip, not the Anzeon-effective tip (pool eviction)
`core/txpool/legacypool/legacypool.go` `lookup.RemotesBelowTip` (called by `SetGasTip`
when the tip floor RISES) compared `tx.GasTipCapIntCmp(threshold)` — the **raw**
`maxPriorityFeePerGas`. For an unauthorized account the effective tip is the block
`GasTip` (cached on the tx at pool entry as `anzeonTipCap`), so a tx with a high raw tip
but a low effective tip is **not evicted** on a floor raise despite being permanently
unminable, clogging the pool / the account's nonce queue.

**Fix:** compare the cached Anzeon-effective tip (`tx.GetAnzeonTipCap()`, falling back to
the raw tip when uncached). Regression test: `core/txpool/legacypool/anzeon_remotesbelowtip_test.go`
(RED on the buggy parent, GREEN after the fix; existing `TestRepricing*` stay green).

## How the defects were found
- **Defect 1** by the autonomous pipeline (analyzer→planner→implementer→evaluator) on a
  lean, observable-only symptom — see `pipeline-analysis/` (`analysis.md`, `plan.md`,
  `design-v1.md`, `reproduction.json`, `test-report.md`).
- **Defect 2** was surfaced by an end-to-end chainbench reproduction and pinned by a
  read-only root-cause diagnosis — see `chainbench-e2e/` (`remotesbelowtip-diagnosis.md`,
  `reproduction-evidence.md`, `c-09-remotesbelowtip-anzeon-eviction.sh`, logs).

## Accuracy vs the expert fix (`98f05c2a0`)
- Defect 1: our `SetCurrentBlock` rewrite is functionally a superset of the expert's
  `gasTipChanged()` guard (refreshes on any header change, not only gasTip).
- Defect 2: same function, same root cause as the expert. The diagnosis first proposed
  `EffectiveGasTipIntCmp` (which also applies the EIP-1559 `min(tip, feeCap-baseFee)`
  clamp and regressed `TestRepricing`/`TestRepricingDynamicFee`); this branch adopts the
  expert's more surgical cached-value comparison (`GetAnzeonTipCap` + raw fallback),
  which fixes the bug with no regression. See `chainbench-e2e/our-fix-final.diff` vs
  `chainbench-e2e/expert-98f05c2a0.diff`.

## Files
- `eth/gasprice/anzeon.go` — Defect 1 fix.
- `core/txpool/legacypool/legacypool.go` — Defect 2 fix.
- `core/txpool/legacypool/anzeon_remotesbelowtip_test.go` — Defect 2 regression test.
- `docs/pr-77-phase2-test1/` — analysis, reproduction, diagnosis, chainbench tests, diffs.
