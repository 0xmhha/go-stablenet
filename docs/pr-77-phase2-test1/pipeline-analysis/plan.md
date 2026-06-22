# Plan — LOCAL-20260622_051111

Bug-fix plan derived from analysis.md. Single-source, atomic, reviewable.

The diagnosis is runtime-confirmed (see `reproduction.json`,
`TestReproduce_GasTipPolicyStaleOnIdleBlocks`, RED at pinned parent `0bf2f4d1b`).
The broken edge is `eth/gasprice/anzeon.go:54` — `AnzeonTipEnv.SetCurrentBlock`
conflates "state changed (Root)" with "header changed (identity)", so on empty
blocks (`parent.Root == child.Root`) the header swap is skipped and
`env.currentBlock` is pinned to the *previous* header. Non-validator senders
then read `env.currentBlock.GasTip()` and see the OLD policy. Validators bypass
this branch (`GetAnzeonTipCap` line 119), which is exactly the asymmetry in the
ticket.

## Step 1: Fix SetCurrentBlock to refresh header on every head change

- Target files:
  - `eth/gasprice/anzeon.go`
- Target symbols:
  - `(*AnzeonTipEnv).SetCurrentBlock` (`eth/gasprice/anzeon.go:50-63`)
- Rationale:
  The Root-equality guard was intended to skip the *expensive* `stateAt`
  re-read, not the cheap pointer swap of `env.currentBlock`. Untangle the
  two: always assign the new header (and re-derive `signer` if necessary);
  only guard the `stateAt` re-read by Root equality. This restores the
  invariant *"`env.currentBlock` tracks the latest known header"* (which is
  what the function's doc-comment on line 49 promises) without losing the
  original optimisation.
- Dependencies: none.
- Verification:
  - `TestReproduce_GasTipPolicyStaleOnIdleBlocks` flips from RED → GREEN
    on this commit. (The reproduction was written before this step; it
    drives `SetCurrentBlock(N) → SetCurrentBlock(N+1)` with same Root but
    new `WBFTExtra.GasTip` and asserts `GetAnzeonTipCap` returns the
    NEW policy for a non-authorized sender and `tx.GasTipCap()` for an
    authorized sender.)
  - `go build ./...` clean.
  - `go test -count=1 ./eth/gasprice/...` clean.
  - `go test -count=1 ./core/txpool/...` clean (existing legacypool tests
    use `SetCurrentBlock`, so this asserts no regression in the caller
    contract).
  - `go test -count=1 -race ./eth/gasprice/... ./core/txpool/... ./miner/...`
    clean (concurrency invariant #11 — see write-site table in design).

## Step 2: Add focused regression unit tests for SetCurrentBlock semantics

- Target files:
  - `eth/gasprice/anzeon_test.go` (new; if a same-named file already
    exists, append the new tests rather than overwrite)
- Target symbols (new tests):
  - `TestSetCurrentBlock_RefreshesHeaderOnSameRoot`
  - `TestSetCurrentBlock_SkipsStateReadOnSameRoot`
  - `TestSetCurrentBlock_ReadsStateOnRootChange`
- Rationale:
  The Analyzer's reproduction test (`TestReproduce_GasTipPolicyStaleOnIdleBlocks`
  in `eth/gasprice/anzeon_repro_test.go`) acts as the end-to-end acceptance
  oracle and **must not be modified**. The reproduction proves the *symptom*
  is fixed; these focused unit tests pin down the *invariants* of
  `SetCurrentBlock` so a future "optimisation" cannot silently restore the
  bug:
  1. Header pointer is updated whenever the new header differs from the
     stored one (even when Root is identical).
  2. `stateAt` is **not** re-invoked when Root is unchanged (preserves the
     original perf optimisation — guards regression in the other direction).
  3. `stateAt` IS re-invoked when Root changes.
- Dependencies: Step 1.
- Verification:
  - All three tests PASS after Step 1 is applied. (Confirm RED → GREEN by
    asserting they would fail against the unmodified buggy line 54 — done
    indirectly by `TestReproduce_GasTipPolicyStaleOnIdleBlocks` which
    already RED-confirms case 1.)
  - `go test -count=1 ./eth/gasprice/...` clean.

## Verification Plan

- **Reproduction oracle (PRIMARY GATE)**:
  - `go test -count=1 -run 'TestReproduce_GasTipPolicyStaleOnIdleBlocks' ./eth/gasprice/...`
    must PASS after Step 1. (RED-confirmed on parent `0bf2f4d1b`; Evaluator
    re-verifies after the fix.)
- **Unit tests** (per step):
  - Step 1: `go test -count=1 ./eth/gasprice/... ./core/txpool/...`.
  - Step 2: focused tests above, in `eth/gasprice/`.
- **Build verification** (after each step):
  - `go build ./...`.
- **`-race` scope** (RI-21 / Invariant #11):
  Per `related-code.json.ckg.concurrency_impact` and the impacted modules
  cataloged in the design's write-site table:
  - `go test -race -count=1 ./eth/gasprice/...`
  - `go test -race -count=1 ./core/txpool/...`
  - `go test -race -count=1 ./miner/...`
  These cover the writers (legacypool `Init` / `Pending` / `reset`) and the
  read paths reached from miner snapshot generation. No new lock or
  contract change is introduced, so the existing `pool.mu` discipline
  carries through unchanged.
- **ChainBench**: scope.modules includes consensus / governance / state in
  the ticket, but the actual code change is contained to `eth/gasprice/`
  (client-local cache). Per the stablenet-context guidance, ChainBench is
  recommended when consensus paths are touched; here only a downstream
  consumer of header values is touched. The Evaluator will still run the
  pipeline's default chainbench profile if its policy requires it; we do
  not skip it from the plan.
- **Acceptance criteria coverage** (from `ticket-parsed.json`):
  - "가스 팁 정책 변경 후 idle 구간에서도 일반 계정 정상 거래가 수락된다"
    → covered by `TestReproduce_GasTipPolicyStaleOnIdleBlocks` + the
    fix in Step 1.
  - "위 시나리오를 재현하는 회귀 unit test 추가" → covered by the
    existing reproduction test (carried unchanged) and the focused
    invariants in Step 2.
  - "race test 통과" → covered by the `-race` scope above.
  - "chainbench basic/tx-send 통과" → covered by Evaluator's chainbench
    stage.

## Risks

- **Spurious `stateAt` calls if the optimisation is dropped.** Mitigation:
  in Step 1 we keep the Root-equality guard but move it to gate only the
  `stateAt` re-read, not the header swap. Step 2 pins this with an explicit
  invariant test.
- **Stale `tx.anzeonTipCap` (copy 3) on txs already validated during the
  buggy window.** Mitigation: no code change. Reasoning: (a) the cache is
  only set when `tx.GetAnzeonTipCap() == nil` (validation.go:337), so once
  Step 1 is applied, every *newly* validated tx reads from the correct
  `env.currentBlock` and caches the correct value; (b) priced-list re-heap
  is triggered by `pool.reset` on the next ChainHeadEvent, so the priced
  list is recomputed; (c) the symptom described in the ticket is "rejected
  for a while, then resolves on its own" — the on-its-own resolution is
  driven by the same Root-change event that today unsticks
  `env.currentBlock` and will still unstick anything residual after the fix.
  Defence-in-depth via cache-bust on reset is a possible follow-up but is
  not required to ship a correct fix.
- **Concurrency**: writes to `env.currentBlock` remain serialised under
  `pool.mu` at every production caller (Init / Pending / reset). The fix
  does not move the assignment outside the lock and does not add new
  goroutines / channels. Invariant #11 preserved.
- **Consensus / fairness invariants**: untouched. The fix is in a
  client-local cache used for *pricing*; it does not alter header
  production, header validation, validator weighting, quorum, base-fee
  redistribution, or any byzantine-fairness surface.
