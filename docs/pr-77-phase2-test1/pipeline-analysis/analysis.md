# Analysis — LOCAL-20260622_051111

## Ticket

Bug: 거버넌스로 가스 우선순위 팁(minTip) 정책을 변경한 직후, 일반(비-밸리데이터) 계정의 정상 거래가
한동안 부당하게 거부된다. 빈 블록만 생성되는 idle 구간 동안 지속되다가, 새 거래가 처리되는
블록이 나오면 저절로 정상화된다. **밸리데이터 계정은 영향받지 않는다.**

- Mode: bugfix
- Retrieval backend: cks (ok / serviceable; ckg + ckv reachable; bge-m3; pinned to 0bf2f4d1b — the buggy parent)
- Freshness: fresh (indexed_head == current_head == 0bf2f4d1b; untracked .claude/.coding-agent/CLAUDE.md ignored as out-of-build-scope)

## Domain & Complexity

- Primary domain: `txpool` (concretely: shared per-block "effective-tip" environment used by `legacypool`)
- Secondary: `consensus/wbft/engine` (producer of `Header.WBFTExtra.GasTip`), `miner` (consumer
  of the same effective tip via `Pending(filter)`), `governance / system contracts` (the
  trigger — `GovValidator.proposeGasTip`).
- Complexity: **moderate**. Single file change, but the variable participates in a
  multi-stage lifecycle (consensus header → pool env → tx cache → priceHeap ordering) and
  is read concurrently by mining/Reheap and written under pool.mu by reset/Pending/Init.
- Concurrency-sensitive: yes (invariant #11). Writes serialized under `pool.mu`; reads on
  `EffectiveGasTip` path can race with the writer.

## Related Code (CKV)

The cks `get_for_task` pack surfaced the full lifecycle in one call. Key bodies (cited from
the pack — not re-Read here):

- `eth/gasprice/anzeon.go:50-63` — `AnzeonTipEnv.SetCurrentBlock`. **Broken edge.**
- `eth/gasprice/anzeon.go:87-120` — `AnzeonTipEnv.GetAnzeonTipCap`. Routes non-validator
  senders to `env.currentBlock.GasTip()`; validators bypass via `tx.GasTipCap()`.
- `core/types/block.go:111-117` — `Header.GasTip()` (extracts from WBFTExtra).
- `consensus/wbft/engine/engine.go:622-645` — `Engine.getGasTip` reads gasTip from the
  **parent** state and `WriteGasTip` stamps it into the new header's WBFTExtra. Therefore
  header N (the gov-change block) carries the OLD policy and header N+1 (next block) carries
  the NEW policy.
- `core/txpool/legacypool/legacypool.go:340-350 / 575-619 / 1395-1410 / 1446-1542` — pool
  reset / Pending / Init all call `anzeonTipEnv.SetCurrentBlock(head)` and `SetBaseFee`.

Governance trigger (CKV): `systemcontracts/solidity/v1/GovValidator.sol:175-194`
(`proposeGasTip`, `_setGasTip`, `GasTipUpdated`); native side `systemcontracts/gov_validator.go:212-215`
(`GetGasTip`).

## Structural Context (CKG)

`SetCurrentBlock` callers in production (cks impact_analysis using canonical id):
1. `core/txpool/legacypool/legacypool.go:346` — `Init` (startup).
2. `core/txpool/legacypool/legacypool.go:585` — `Pending(filter)` (per-mining snapshot).
3. `core/txpool/legacypool/legacypool.go:1536` — `reset(oldHead, newHead)` — **the
   ChainHeadEvent-driven path**, fires on EVERY new head, empty or not. This is the call
   that gets EATEN by the buggy inner guard on idle blocks.

`GetAnzeonTipCap` callers (cks find_callers):
1. `core/txpool/validation.go:337-340` — `ValidateTransactionWithState` caches the result
   on `tx.anzeonTipCap` (secondary cache; root source is `env.currentBlock`).
2. `core/types/transaction.go:400` — `EffectiveGasTip` fallback when the tx cache is nil;
   feeds `EffectiveGasTipCmp` / `EffectiveGasTipIntCmp` used by `priceHeap.cmp` (Reheap)
   and `legacypool.Pending` filter (line 596).

## Impact Analysis

The seed `(*AnzeonTipEnv).SetCurrentBlock` reverse-closure is dominated by `legacypool_test.go`
test sites (29 callers). Production callers reduce to the 4 listed above. Effective tip is
the SOLE non-validator pricing oracle in non-test code paths, so any stale `env.currentBlock`
propagates to ALL non-validator txs that pass through validation while the staleness
persists. The fix is local (one function), but completeness requires that **every** place
that should-react-to a head change actually does react, not just the ones where state Root
changes.

## Reproduction

Authored once at `/Users/wm-it-25_0220/Work/github/test/pr-77/eth/gasprice/anzeon_repro_test.go`.

- Test name: `TestReproduce_GasTipPolicyStaleOnIdleBlocks`
- Run: `go test -count=1 -run 'TestReproduce_GasTipPolicyStaleOnIdleBlocks' ./eth/gasprice/...`
- RED CONFIRMED on pinned parent `0bf2f4d1b`:

  --- FAIL: TestReproduce_GasTipPolicyStaleOnIdleBlocks (0.00s)
    anzeon_repro_test.go:190: non-validator effective tip is stale: got 5000000000,
        want 1000000000 (env.currentBlock should track the latest header even when only
        the GasTip changes and state Root is unchanged)
    anzeon_repro_test.go:197: BUG REPRODUCED: non-validator tip resolved to OLD policy
        5000000000 (header N.GasTip), not NEW policy 1000000000 (header N+1.GasTip).
        AnzeonTipEnv.SetCurrentBlock at eth/gasprice/anzeon.go:54 skipped the header update
        because parent.Root == child.Root on the empty block.

The test is intentionally focused: it constructs two `*types.Header`s with the same `Root`
but different RLP-encoded WBFTExtra GasTip, drives `SetCurrentBlock(N) → SetCurrentBlock(N+1)`,
then asserts on `GetAnzeonTipCap` for both a non-authorized and an authorized sender. It
spins up no chain — making it deterministic and fast — and it also documents the
distinguishing feature (validators are unaffected) the ticket calls out.

The test will be CARRIED unchanged by the Implementer as the acceptance oracle for the
fix (reproduce-first contract): RED here → GREEN after the fix → re-verified by the
Evaluator. The Implementer must NOT modify it.

## Root cause

Apply the root-cause-lifecycle skill — value, lifecycle, broken edge, falsification, source.

**Value(s):** the effective per-block gas-tip used to evaluate non-validator tx pricing.
Lifecycle:

- producer  : Engine.getGasTip(parent_state) → WriteGasTip → Header.WBFTExtra.GasTip
            (a header field, stamped at sealing time)
- copies    :
    1. `pool.gasTip` (atomic.Pointer[uint256.Int]) — refreshed via worker.SetGasTip
       on every block (incl. empty); kept FRESH on this path.
    2. `env.currentBlock` (*types.Header) inside AnzeonTipEnv — supposed to be
       refreshed on every head change; *not* refreshed on empty blocks due to the
       buggy guard below.
    3. `tx.anzeonTipCap` (atomic.Pointer) — per-tx CACHE written once during
       `ValidateTransactionWithState` from `env.currentBlock.GasTip()`. Once set,
       never recomputed (the `if tx.GetAnzeonTipCap() == nil` guard at
       core/txpool/validation.go:337). This is the SYMPTOM cache, not the source.
- consumers : `EffectiveGasTip` → `EffectiveGasTipCmp` / `EffectiveGasTipIntCmp` →
            `priceHeap.cmp` (Reheap), `legacypool.Pending` filter (line 596),
            `ValidateTransactionWithState` caching path (line 337).

**Broken edge — `eth/gasprice/anzeon.go:54`:**

    func (env *AnzeonTipEnv) SetCurrentBlock(header *types.Header) {
        if header == nil { return }
        if env.currentBlock == nil || env.currentBlock.Root != header.Root {
            env.currentBlock = header
            ...
        }
    }

The function comment one line above (line 49) states *"This should be called when the
blockchain head changes."* — every head change. The implementation, however, gates the
update on **state Root** change. For an EMPTY block (no transactions), `parent.Root ==
child.Root` even though the header itself is brand new and may carry an updated
`WBFTExtra.GasTip`. The result: after a governance change at block N, headers N+1, N+2, …
each carry the NEW gasTip, but `env.currentBlock` stays pinned to **block N** (whose
`WBFTExtra.GasTip` is the OLD policy, because gasTip is sourced from N's PARENT state).
`env.currentBlock.GasTip()` therefore returns the OLD policy to every non-validator query
until a non-empty block finally moves the Root forward (where the symptom resolves on its
own — matching the ticket exactly).

**Step 5 (time sequence — what *clears* the symptom):** the clearing event is `pool.reset`
firing for a block whose `newHead.Root != oldHead.Root`. That call hits `SetCurrentBlock`
with a header whose Root differs, the guard finally allows the swap, and `env.currentBlock`
catches up. This points at the missing update.

**Step 7 (source vs symptom):** `tx.anzeonTipCap` (copy 3) holds a stale value for txs
validated during the idle window — but those values were poured in from `env.currentBlock`
at validation time. Fixing the per-tx cache alone would mask the source; the true source is
the `env.currentBlock` update being skipped (copy 2). `pool.gasTip` (copy 1) is REFRESHED
promptly even on empty blocks via `bc.gasTipUpdater → worker.updateGasTipFromContract →
TxPool().SetGasTip`, so it is *not* the source (refutes a tempting alternative hypothesis).

**Step 6 (falsification with distinguishing features):**
- "한동안 지속되다 새 거래가 처리되면 정상화" — duration = idle empty-block window;
  resolution = state-root-changing block. EXACTLY matches the Root-guarded skip.
- "밸리데이터 계정은 영향받지 않는다" — `GetAnzeonTipCap` line 112: `if env.currentState !=
  nil && !env.currentState.IsAuthorized(from) && env.currentBlock.GasTip() != nil { return
  env.currentBlock.GasTip() }`. Authorized senders fall through to the `tx.GasTipCap()`
  return at line 119 — they never touch the stale `env.currentBlock.GasTip()`. EXACTLY
  matches the asymmetry.
- The candidate "pool.gasTip is stale" cannot explain either feature: `pool.gasTip` is
  written on every head, *and* its check (`ValidateTransaction` line 122) treats validators
  and non-validators identically — so it cannot produce the validator-bypass pattern.

**Step 9 (multi-candidate runtime probe):** not needed. The reproduction test in §Reproduction
is itself the runtime confirmation — it drives the exact lifecycle described above and
observes the stale value at the precise consumer site `GetAnzeonTipCap`, matching the
predicted OLD value (5 gwei) exactly.

**Step 8 (every cache has an invalidator?):** copy 2 has the broken invalidator (the §54
guard). Copy 3 has NO invalidator at all (`tx.anzeonTipCap` is "set once, read forever").
Within a fix cycle copy 3 is acceptable iff copy 2 is correct — because any tx is validated
*after* the head update has propagated to copy 2. The Planner should consider whether to
leave copy 3 alone (acceptable once 2 is fixed) or add a Reset path (defence in depth);
that is a design decision, not a diagnosis question.

**Confidence: high.** Runtime-confirmed via the reproduction test; the broken edge is named
with `file:line`; the failure-mode (a guard that conflates "state changed" with "header
changed") matches every observable in the ticket; the bypass for validators is mechanically
explained by the same function.

## Affected sites the Planner must address (forward / write-site completeness)

Carrying the diagnosis forward, the fix needs to ensure:

1. **eth/gasprice/anzeon.go:54** — the broken `Root != header.Root` guard must be replaced
   so that `env.currentBlock` is updated on every header change (e.g. compare header
   pointers / hashes / numbers — anything that doesn't conflate header identity with state
   identity). The `stateAt` re-read CAN still be skipped when Root is unchanged (preserves
   the optimisation the original author was reaching for, without dropping the header
   refresh).
2. **Concurrency contract** — `SetCurrentBlock` callers (line 346 Init, line 585 Pending,
   line 1536 reset) all run under `pool.mu` write/lock. Readers via `EffectiveGasTip`
   (e.g. `priceHeap.cmp` during Reheap) do NOT hold `pool.mu`. The fix must preserve this
   layering; the safest change is purely an in-method tweak (no new locks, no contract
   change for callers). Pointer-and-field assignments are not atomic in Go — if the fix
   writes both `env.currentBlock` AND `env.currentState`/`env.signer` together (as today),
   it must remain inside the existing serialised call paths. No new exposure introduced.
3. **No change** required to `pool.gasTip` / `worker.tip` / `bc.gasTipUpdater` plumbing —
   those paths are already correct.
4. **Per-tx `anzeonTipCap` cache (3)** — DESIGN DECISION for the Planner. Recommendation:
   leave as-is. New txs after the fix will cache the CORRECT (fresh) tip; old txs in the
   pool during a real-world upgrade would be re-tested only on the next reset where the
   priced list re-heaps. If the team wants defence-in-depth, an additional cache-bust on
   reset is a small follow-up.
5. **Regression test** — the reproduction test is already in place; the Planner should
   require it stays unchanged and is the gating GREEN check in the Evaluator.

## Risk Assessment

- Surface area: extremely small (one function in `eth/gasprice/anzeon.go`).
- Consensus risk: none — the change is in a client-local cache (effective tip seen by the
  txpool and miner). Header production / validation are upstream and untouched.
- Concurrency risk: low — preserve existing serialisation; no new goroutines / channels.
- Backwards compatibility: none affected (no API surface change).
- Stablenet invariants:
  - #1 (EQUAL POWER) — unaffected (gasTip is administrative, not weighting).
  - #6 (INSTANT FINALITY) — unaffected (no consensus path touched).
  - #11 (CONCURRENCY DISCIPLINE) — preserved by keeping writes under `pool.mu`.

## Open Questions

- (Planner judgement) Optimisation choice on the fix: replace the Root guard with a
  pointer-/hash-based "has header changed" check, or unconditionally update the header and
  guard only the `stateAt` re-read? The latter is slightly cleaner — the original
  optimisation was clearly to avoid re-fetching state for the same root. Either is correct;
  pick the smaller diff.
- (Planner judgement) Whether to add defence-in-depth on the per-tx `anzeonTipCap` cache
  (copy 3) at this time, or follow up separately. The current diagnosis does not require it
  to ship a correct fix.
