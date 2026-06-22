# Design v1 — LOCAL-20260622_051111

Detailed design for the two-step plan. Rendered against the pinned parent
`0bf2f4d1b` source already cited in `analysis.md` and `related-code.json`
(no re-Reads of pack-cited spans). The fix is localized to `eth/gasprice/anzeon.go`;
the §5.2b write-site analysis confirms no other production site needs
to change.

---

## §5.2b Write-site completeness — derived state we mirror, and where it is maintained

The "derived state" here is `AnzeonTipEnv.currentBlock` — a pointer to the
*latest known* chain head, maintained in `eth/gasprice/anzeon.go` to drive
the effective-tip computation for non-validator senders. Its **source** is
the chain head (`pool.chain.CurrentBlock()` /
`reset.newHead` / `filter.Header`), and the structure it mirrors is the
legacypool's `pool.currentHead` (an `atomic.Pointer[*types.Header]`,
`legacypool.go:1533`).

Per the root-cause-lifecycle skill (lifecycle: produce → store/copy →
consume), the *copies* of "current head" inside the txpool are:

| Copy | Storage | Producer / writers | Currently maintained correctly? |
|------|---------|--------------------|----------------------------------|
| (a)  | `pool.currentHead` (`atomic.Pointer`) | `legacypool.Init:343`; `legacypool.reset:1533` | YES — `Store(head/newHead)` is unconditional. |
| (b)  | `pool.anzeonTipEnv.currentBlock` | `legacypool.Init:346`; `legacypool.Pending:585`; `legacypool.reset:1536` | NO — guarded by `Root != header.Root`; SKIPS on empty blocks. **This is the bug.** |
| (c)  | `tx.anzeonTipCap` (atomic, per-tx) | `core/txpool/validation.go:339` (`tx.SetAnzeonTipCap`) — set once per tx | symptom-only cache; correct *once* (b) is fixed (see "Decision on copy (c)" below). |

Enumerated mutation/maintenance sites for the structure that copy (b)
mirrors (chain head as seen by the txpool). From cks
`find_callers(SetCurrentBlock)` / `impact_analysis(SetCurrentBlock)`:

| # | Call site (file:line)                                            | Caller     | Maintenance for `env.currentBlock` after the fix | Reason |
|---|------------------------------------------------------------------|------------|---------------------------------------------------|--------|
| 1 | `core/txpool/legacypool/legacypool.go:346`                       | `Init`     | `SetCurrentBlock(head)` — KEEP as-is              | Pool startup; sets initial head. Existing call already passes correct argument. |
| 2 | `core/txpool/legacypool/legacypool.go:585`                       | `Pending`  | `SetCurrentBlock(filter.Header)` — KEEP as-is     | Per-mining snapshot. Existing call already passes correct argument. |
| 3 | `core/txpool/legacypool/legacypool.go:1536`                      | `reset`    | `SetCurrentBlock(newHead)` — KEEP as-is           | **The ChainHeadEvent path** — fires on every new head, empty or not. The bug is downstream of this call; the call itself is correct. |
| 4 | `core/txpool/legacypool/legacypool.go:1407` (reorg loop)         | `runReorg` | NO CHANGE — only `SetBaseFee` here                | Reorg loop adjusts base fee but does not own header swap; head swap is owned by site 3. |

**Empty cells**: none. Every site that *should* react to a head change
already calls `SetCurrentBlock` at the txpool boundary. The bug is purely
inside `SetCurrentBlock` itself — the *function body* drops the
header swap on the `Root`-equal path. Therefore the fix is a single-function
change; no upstream caller needs to change.

**Co-location vs scattered maintenance.** The new state (`env.currentBlock`)
is already co-located inside `AnzeonTipEnv` and maintained by its sole
mutator `SetCurrentBlock`. The fix preserves this layering (single mutator,
single guard) rather than spreading maintenance across callers — this is
the strongest form of §5.2b's "co-locate" recommendation.

**Self-checking invariant + test name** (§5.2b requirement):
- Invariant (function contract): "after `SetCurrentBlock(h)` returns with
  non-nil `h`, `env.currentBlock == h`." (Direct identity, not Root-modulo.)
- Test that pins it: `TestSetCurrentBlock_RefreshesHeaderOnSameRoot`
  (Step 2 below).
- End-to-end oracle: `TestReproduce_GasTipPolicyStaleOnIdleBlocks`
  (Analyzer's pre-written reproduction; RED-confirmed at parent
  `0bf2f4d1b`).

**Decision on copy (c) — `tx.anzeonTipCap`.** Per `analysis.md` §"Step 8"
and the ticket's open question: the per-tx cache has *no* invalidator
(set-once via the `tx.GetAnzeonTipCap() == nil` guard at
`core/txpool/validation.go:337`). Leave it unchanged in this fix:
1. After Step 1 lands, every *newly* validated tx reads from a fresh
   `env.currentBlock` (b) and caches the correct value.
2. Already-pooled txs that were validated during the buggy window carry
   stale `tx.anzeonTipCap`; but the `pool.reset` cycle on the next
   non-empty head invokes `pool.priced.Reheap()` (`legacypool.go:1409`),
   which re-orders the priced list using the **then-current**
   `env.currentBlock` via `EffectiveGasTipCmp` (transactions whose cache
   is set will keep their stale tip ordering, but the *base-fee*-aware
   filter at `Pending` line 596 uses `EffectiveGasTipIntCmp(MinTip,
   anzeonTipEnv)` and routes through `env.currentBlock` for any tx whose
   cache is nil — so any newly-arriving txs are immediately corrected).
3. The ticket's observable ("증상이 새 거래 처리되는 블록에서 정상화됨")
   is fully explained by fixing (b) alone — see analysis.md §"Step 5".
4. Defence-in-depth (`tx.anzeonTipCap` cache-bust on `pool.reset`) is
   captured in `plan.md` §Risks as a follow-up, not a requirement.

---

## Step 1: Fix SetCurrentBlock to refresh header on every head change

### Current code (excerpt)

`eth/gasprice/anzeon.go:48-63`

```go
// SetCurrentBlock updates the current block header and signer.
// This should be called when the blockchain head changes.
func (env *AnzeonTipEnv) SetCurrentBlock(header *types.Header) {
    if header == nil {
        return
    }
    if env.currentBlock == nil || env.currentBlock.Root != header.Root {
        env.currentBlock = header
        if header.Root != (common.Hash{}) {
            env.currentState, _ = env.stateAt(header.Root)
        } else {
            env.currentState = nil
        }
        env.signer = types.MakeSigner(env.config, header.Number, header.Time)
    }
}
```

### Proposed change

Untangle two concerns: (i) the cheap, idempotent header swap — must
happen on every call so `env.currentBlock` always tracks the latest known
head; (ii) the expensive state re-read — keep it guarded by `Root`
equality so we don't refetch `stateAt(Root)` for the same trie root.

```go
// SetCurrentBlock updates the current block header and signer.
// This should be called when the blockchain head changes.
//
// The header pointer is updated on every non-nil call so that
// env.currentBlock always reflects the most recently observed head — this
// is the contract that downstream consumers (GetAnzeonTipCap, the priced
// heap via EffectiveGasTip) rely on. The expensive stateAt re-read is
// skipped when the new header has the same state Root as the stored one,
// preserving the original optimisation for empty / state-unchanged
// blocks without conflating header identity with state identity.
func (env *AnzeonTipEnv) SetCurrentBlock(header *types.Header) {
    if header == nil {
        return
    }
    rootChanged := env.currentBlock == nil || env.currentBlock.Root != header.Root
    env.currentBlock = header
    env.signer = types.MakeSigner(env.config, header.Number, header.Time)
    if rootChanged {
        if header.Root != (common.Hash{}) {
            env.currentState, _ = env.stateAt(header.Root)
        } else {
            env.currentState = nil
        }
    }
}
```

Notes:
- The header pointer assignment and `signer` re-derivation are cheap and
  do not require I/O. They run on every call.
- The `env.currentState` re-read (`env.stateAt(...)`) only runs when
  Root actually changed — same condition as before. This preserves the
  optimisation the original author was reaching for, *without* swallowing
  the header refresh.
- We also re-derive `env.signer` unconditionally. The signer depends on
  `header.Number` and `header.Time` (and the chain config), both of
  which advance on every block — including empty ones. Recomputing it
  per head is cheap (`types.MakeSigner` is a constructor with a few
  conditionals over `params.ChainConfig`) and avoids the same class of
  staleness bug for any future consumer that depends on the signer
  tracking the latest block's hardfork rules.
- `env.baseFee` is **not** managed here; it is owned by `SetBaseFee` and
  the legacypool already calls `SetBaseFee` on the relevant paths
  (`legacypool.go:348`, `587`, `1407`). Out of scope for this fix.
- No new lock. Writes remain serialised by the existing caller
  discipline (`pool.mu` held at `Init` / `Pending` / `reset` — see the
  write-site table above). Reads via `EffectiveGasTip` are unchanged.

### Side-effect checklist

- [x] **Does the change preserve the public interface?** Yes — the
      function signature `(*AnzeonTipEnv).SetCurrentBlock(*types.Header)`
      and its semantic contract ("after this returns, env.currentBlock
      tracks the latest head") are preserved. The doc comment is
      clarified, not changed in meaning.
- [x] **Are all error paths covered?** Yes — the only error-bearing call
      is `env.stateAt(header.Root)`; its error-suppressing pattern
      (`_, _ = env.stateAt(...)`) is preserved unchanged. This matches
      the surrounding pattern in `GetAnzeonTipCap` at line 108.
- [x] **Is concurrent safety preserved?** Yes — all three production
      callers hold `pool.mu` while calling this (see write-site table
      rows 1–3). Readers via `EffectiveGasTip` (`priceHeap.cmp` during
      Reheap; `Pending` filter at line 596) do not hold `pool.mu`. Both
      groups are unchanged. The fix performs an additional pointer-and-
      signer assignment per call but does not introduce any new sharing
      or remove any existing serialisation. Per invariant #11 the lock
      contract is preserved.
- [x] **Are there new shared resources that need protection?** No.
- [x] **Does any caller assume the old behavior?** Reviewed all 4
      production callers in the write-site table. None depends on
      "header stays stale across empty blocks" — that was the bug, not
      a contract. Specifically:
      - `Init` (line 346): expects the env to be initialised to `head`.
        The new behaviour is the same on the first call (`env.currentBlock
        == nil` branch).
      - `Pending` (line 585): expects the env to reflect `filter.Header`
        for the upcoming snapshot. The new behaviour is stricter (it
        actually updates), which is what Pending wants.
      - `reset` (line 1536): expects the env to reflect `newHead`. Same
        as above.
      - `runReorg` (line 1407): does not call `SetCurrentBlock`. N/A.
- [x] **Does this introduce derived/parallel state — an aggregate, cache,
      index, counter, or map that mirrors another structure?** Yes —
      already analyzed in §5.2b above. `env.currentBlock` is a copy of
      the txpool's chain head, and `tx.anzeonTipCap` is a downstream
      cache of `env.currentBlock.GasTip()`. Write-site table is
      exhaustive and the per-tx cache decision is recorded.

### Tests

- **Existing tests that must still pass:**
  - All current tests in `eth/gasprice/...` (gasprice oracle tests,
    other anzeon paths).
  - All current tests in `core/txpool/...` (29 SetCurrentBlock test
    sites surfaced by cks impact_analysis; these test integration with
    the pool and will catch any regression in the public contract).
  - All current tests in `miner/...` (consumers of `Pending(filter)`).
- **New tests to add (Step 2):**
  - `TestSetCurrentBlock_RefreshesHeaderOnSameRoot` — pins the §5.2b
    invariant.
  - `TestSetCurrentBlock_SkipsStateReadOnSameRoot` — pins the
    optimisation (counts `stateAt` invocations via a fake).
  - `TestSetCurrentBlock_ReadsStateOnRootChange` — pins the inverse:
    `stateAt` IS called when Root changes.
- **Reproduction oracle (Analyzer-authored, carried unchanged):**
  - `TestReproduce_GasTipPolicyStaleOnIdleBlocks` — must flip RED →
    GREEN on this commit. **Implementer MUST NOT modify it.**

---

## Step 2: Add focused regression unit tests for SetCurrentBlock semantics

### Current code (excerpt)

No existing `eth/gasprice/anzeon_test.go` file is referenced in the
cks pack or in the Analyzer's citations (the reproduction lives at
`eth/gasprice/anzeon_repro_test.go`). Implementer should check
`ls eth/gasprice/*_test.go`; if a same-named file already exists, append
the three tests to it instead of overwriting.

### Proposed change

Add a new test file `eth/gasprice/anzeon_test.go` (or append to existing)
containing focused unit tests that exercise `SetCurrentBlock` semantics
directly — without spinning up a chain, mirroring the style of the
reproduction test.

```go
// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2025 The go-stablenet Authors
// ...

package gasprice

import (
    "math/big"
    "testing"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/core/state"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/params"
)

// stateAtCounter wraps a stateAt closure that records invocation count
// and the Root values it was called with.
type stateAtCounter struct {
    calls []common.Hash
    fn    func(common.Hash) (*state.StateDB, error)
}

func (c *stateAtCounter) stateAt(root common.Hash) (*state.StateDB, error) {
    c.calls = append(c.calls, root)
    if c.fn != nil {
        return c.fn(root)
    }
    return nil, nil
}

// makeHeader returns a synthetic *types.Header carrying the given
// number and root. Time defaults to 0; callers can override.
func makeHeader(num uint64, root common.Hash) *types.Header {
    return &types.Header{
        Number: new(big.Int).SetUint64(num),
        Root:   root,
    }
}

// TestSetCurrentBlock_RefreshesHeaderOnSameRoot pins the §5.2b
// invariant: after SetCurrentBlock(h) returns with non-nil h, the
// stored header pointer equals h — even when h.Root matches the
// previously-stored header's Root (the empty-block scenario).
func TestSetCurrentBlock_RefreshesHeaderOnSameRoot(t *testing.T) {
    counter := &stateAtCounter{}
    env := NewAnzeonTipEnv(params.AllEthashProtocolChanges, counter.stateAt)

    sharedRoot := common.HexToHash("0xaaaa")
    h1 := makeHeader(1, sharedRoot)
    h2 := makeHeader(2, sharedRoot) // same Root, different identity

    env.SetCurrentBlock(h1)
    if env.currentBlock != h1 {
        t.Fatalf("after first SetCurrentBlock: env.currentBlock = %p, want %p", env.currentBlock, h1)
    }

    env.SetCurrentBlock(h2)
    if env.currentBlock != h2 {
        t.Fatalf("after second SetCurrentBlock (same Root, new header): env.currentBlock = %p, want %p (invariant: header is refreshed on every non-nil call)", env.currentBlock, h2)
    }
}

// TestSetCurrentBlock_SkipsStateReadOnSameRoot pins the preserved
// optimisation: stateAt is invoked exactly once across two calls with
// the same Root.
func TestSetCurrentBlock_SkipsStateReadOnSameRoot(t *testing.T) {
    counter := &stateAtCounter{}
    env := NewAnzeonTipEnv(params.AllEthashProtocolChanges, counter.stateAt)

    sharedRoot := common.HexToHash("0xbbbb")
    env.SetCurrentBlock(makeHeader(1, sharedRoot))
    env.SetCurrentBlock(makeHeader(2, sharedRoot))

    if len(counter.calls) != 1 {
        t.Fatalf("stateAt called %d times, want 1 (Root unchanged across the two SetCurrentBlock calls)", len(counter.calls))
    }
    if counter.calls[0] != sharedRoot {
        t.Fatalf("stateAt called with wrong root: %x, want %x", counter.calls[0], sharedRoot)
    }
}

// TestSetCurrentBlock_ReadsStateOnRootChange pins the inverse: stateAt
// IS re-invoked when Root differs across calls.
func TestSetCurrentBlock_ReadsStateOnRootChange(t *testing.T) {
    counter := &stateAtCounter{}
    env := NewAnzeonTipEnv(params.AllEthashProtocolChanges, counter.stateAt)

    rootA := common.HexToHash("0xcccc")
    rootB := common.HexToHash("0xdddd")
    env.SetCurrentBlock(makeHeader(1, rootA))
    env.SetCurrentBlock(makeHeader(2, rootB))

    if len(counter.calls) != 2 {
        t.Fatalf("stateAt called %d times, want 2 (Root changed across the two SetCurrentBlock calls)", len(counter.calls))
    }
    if counter.calls[0] != rootA || counter.calls[1] != rootB {
        t.Fatalf("stateAt called with wrong roots: %v, want [%x %x]", counter.calls, rootA, rootB)
    }
}
```

Notes for Implementer:
- The Implementer should *first* check whether an `anzeon_test.go` already
  exists in `eth/gasprice/` and, if so, append these three test functions
  + the small helpers (`stateAtCounter`, `makeHeader`) to it (skipping the
  package/import block, merging imports).
- `stateAt` returning `(nil, nil)` is fine for these tests — the SUT only
  inspects the *call pattern*, not the returned StateDB. The reproduction
  test `TestReproduce_GasTipPolicyStaleOnIdleBlocks` already exercises the
  state-aware path.
- If `params.AllEthashProtocolChanges` is not the appropriate config for
  this package (it should be — used by the reproduction test too — verify
  by `Read`), substitute the same value the reproduction test uses.

### Side-effect checklist

- [x] **Does the change preserve the public interface?** Yes — adds tests
      only, no production code touched.
- [x] **Are all error paths covered?** N/A (test-only).
- [x] **Is concurrent safety preserved?** Yes — tests are single-goroutine.
- [x] **Are there new shared resources that need protection?** No.
- [x] **Does any caller assume the old behavior?** N/A — these tests pin
      the post-fix contract; they would FAIL on the pre-fix (line 54)
      code, which is exactly what we want.
- [x] **Does this introduce derived/parallel state?** No.

### Tests

- The three new tests are themselves the verification — they must PASS
  on the post-fix code. (The reproduction test
  `TestReproduce_GasTipPolicyStaleOnIdleBlocks` is the end-to-end
  acceptance oracle; these are the focused unit-level invariants.)
- `go test -count=1 ./eth/gasprice/...` must pass.

---

## §5.3 Self-review

Read what was written above; reviewed for:

- **Inconsistent function signatures**: none — the new
  `SetCurrentBlock` keeps the exact same signature
  `func (env *AnzeonTipEnv) SetCurrentBlock(header *types.Header)`.
- **Missing nil/error checks**: the `header == nil` early-return is
  preserved; the `stateAt` error is suppressed exactly as before.
- **Side-effect checklist items left unanswered**: none — every box is
  ticked with a substantive answer in both steps.
- **§5.2b derived state**: write-site table is rendered, every cell is
  populated (no empty cells), invariant + test name are named.
- **Dependencies between steps**: Step 2 depends on Step 1 (the new
  invariant tests would themselves FAIL on the pre-fix code), and the
  plan and design agree on this order.
- **CKG concurrency hints**: the fix performs no new lock; writers
  remain under `pool.mu`. Readers via `EffectiveGasTip` are unchanged.
  Invariant #11 preserved.
- **Stablenet invariants** (always-on backstop): #1 (equal power), #6
  (instant finality), #11 (concurrency discipline) — all preserved.
  #2/#3 (epoch-length / round-change neutrality) — not touched. #7 (base
  fee redistribution) — not touched (base fee is owned by `SetBaseFee`,
  which is unchanged). #11 specific to txpool — `env.currentBlock`
  writes stay inside `pool.mu.Lock` at every caller.

No issues found → v1 is final.
