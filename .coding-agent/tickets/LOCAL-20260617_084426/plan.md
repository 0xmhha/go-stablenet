# Plan — LOCAL-20260617_084426

> Scope is the **Layer 1 + Layer 2** subset of the diagnosis. Layer 3
> (`eth/gasprice/anzeon.go` same-Root skip) is OUT OF SCOPE per ticket
> mandate. Steps are split-commit-friendly; each step has a precise
> file/symbol/line anchor, an expected test command, and a one-line
> success criterion. Branch `fix/pr-77-gastip-dynamic-reflection-test7`
> (HEAD `0bf2f4d1bfeb6605006d556957ef8c045d8f8ed8`, contains `test7`)
> already satisfies the branch-naming requirement — the Implementer
> commits directly on this branch.

## Go toolchain (Implementer & Evaluator must export this)

```
export PATH=$PATH:/Users/wm-it-25_0220/.gvm/gos/go1.23.12/bin
go version          # → go version go1.23.12 darwin/arm64
```

Matches `go.mod` toolchain directive (`go 1.23.12`). All `go build`,
`go test`, and `go test -race` commands below assume this PATH.

## Step 1: Layer 2 — `Transaction.ClearAnzeonTipCap()` (typed-nil)

**Target file**: `core/types/transaction.go`

**Target symbols**:
- New: `func (tx *Transaction) ClearAnzeonTipCap()` after the existing
  `GetAnzeonTipCap` (current end of getter: line 426).
- Touch: `GetAnzeonTipCap` (line 421-426) — must treat typed-nil store as
  "unset" for back-compat (returns `nil`).

**Line anchors**:
- `anzeonTipCap atomic.Value` — line 77
- `SetAnzeonTipCap` — line 414-418
- `GetAnzeonTipCap` — line 421-426

**Concrete change**:

```go
// ClearAnzeonTipCap invalidates the cached Anzeon tip cap. It uses a
// typed-nil store because atomic.Value.Store(nil) panics: a typed-nil
// keeps the dynamic type of subsequent Loads consistent with prior
// Set/Get cycles.
func (tx *Transaction) ClearAnzeonTipCap() {
    tx.anzeonTipCap.Store((*big.Int)(nil))
}
```

`GetAnzeonTipCap` must explicitly treat the typed-nil sentinel:

```go
func (tx *Transaction) GetAnzeonTipCap() *big.Int {
    if cached := tx.anzeonTipCap.Load(); cached != nil {
        if bi, ok := cached.(*big.Int); ok && bi != nil {
            return bi
        }
    }
    return nil
}
```

**Rationale**: The per-tx cache had **invalidator 0** (analysis §
"Root cause" row 4). Without an invalidator, lowering the floor cannot
make the cache reflect the new env without round-tripping the tx through
the pool — which is exactly the deadlock the analysis identified as the
"companion defect" of Layer 2.

**Dependencies**: none. This is leaf change for the typed cache.

**Verification**:
- `go build ./core/types/...`
- `go test -race ./core/types/...` — existing tx tests still pass.
- One-line success criterion: `tx.SetAnzeonTipCap(big.NewInt(5));
  tx.ClearAnzeonTipCap(); tx.GetAnzeonTipCap() == nil` is true.

---

## Step 2: Layer 1 — system-contract address helper + entry-gate + Pending exemption (legacypool)

**Target file**: `core/txpool/legacypool/legacypool.go`

**Target symbols**:
- New (private helper): `func (pool *LegacyPool) systemContractAddresses() map[common.Address]struct{}` — builds the chainconfig-derived set with nil-guards.
- New (private helper): `func (pool *LegacyPool) isSystemContractTx(tx *types.Transaction) bool` — returns true iff `tx.To() != nil` and `tx.To()` is in the set.
- Modify: `LegacyPool.validateTxBasics` (line 650-669) — add system-contract bypass mirroring the existing `local` branch.
- Modify: `LegacyPool.Pending` (line 569-620) — add system-contract skip to the truncation loop.

**Line anchors (pre-change)**:
- `LegacyPool` struct: `chainconfig *params.ChainConfig` — line 235.
- `pool.gasTip.Load().ToBig()` source — line 660.
- Existing `local` exemption shape — line 662-664:
  ```go
  if local {
      opts.MinTip = new(big.Int)
  }
  ```
- Pending truncation loop — line 593-601:
  ```go
  if minTipBig != nil && !pool.locals.contains(addr) {
      for i, tx := range txs {
          if tx.EffectiveGasTipIntCmp(minTipBig, pool.anzeonTipEnv) < 0 {
              txs = txs[:i]
              break
          }
      }
  }
  ```

**Concrete change**:

1. Add helpers (placed near `validateTxBasics`, end of helper region — keep
   StableNet-specific branch out of `validation.go` per RI-09):

   ```go
   // systemContractAddresses returns the set of chainconfig-derived system
   // contract addresses (GovValidator, GovCouncil, GovMinter,
   // GovMasterMinter, NativeCoinAdapter). It is nil-guarded for any
   // missing chainconfig level so test fixtures (params.TestChainConfig
   // without Anzeon) keep the existing behaviour: empty set, no exemption.
   func (pool *LegacyPool) systemContractAddresses() map[common.Address]struct{} {
       set := map[common.Address]struct{}{}
       if pool.chainconfig == nil || pool.chainconfig.Anzeon == nil {
           return set
       }
       sc := pool.chainconfig.Anzeon.SystemContracts
       if sc == nil {
           return set
       }
       for _, c := range []*params.SystemContract{
           sc.GovValidator, sc.GovCouncil, sc.GovMinter,
           sc.GovMasterMinter, sc.NativeCoinAdapter,
       } {
           if c != nil {
               set[c.Address] = struct{}{}
           }
       }
       return set
   }

   // isSystemContractTx reports whether tx targets a chainconfig-derived
   // system contract. Contract-creation txs (To()==nil) are never system
   // contract txs.
   func (pool *LegacyPool) isSystemContractTx(tx *types.Transaction) bool {
       if tx == nil || tx.To() == nil {
           return false
       }
       set := pool.systemContractAddresses()
       _, ok := set[*tx.To()]
       return ok
   }
   ```

2. Patch `validateTxBasics` (insert after the existing `if local` block at line 664):

   ```go
   if local {
       opts.MinTip = new(big.Int)
   }
   // System-contract-bound txs (governance proposals targeting GovValidator,
   // GovCouncil, GovMinter, GovMasterMinter, NativeCoinAdapter) are exempt
   // from the MinTip gate. This mirrors the `local` exemption and breaks
   // the circular deadlock where a gas-tip restoration proposal cannot
   // enter the pool because its target tip is below the current floor.
   if pool.isSystemContractTx(tx) {
       opts.MinTip = new(big.Int)
   }
   ```

3. Patch `Pending` truncation loop (replace the inner `for i, tx := range txs` block at line 595-600):

   ```go
   if minTipBig != nil && !pool.locals.contains(addr) {
       for i, tx := range txs {
           if pool.isSystemContractTx(tx) {
               continue // system-contract tx is exempt from sealing-time truncation
           }
           if tx.EffectiveGasTipIntCmp(minTipBig, pool.anzeonTipEnv) < 0 {
               txs = txs[:i]
               break
           }
       }
   }
   ```

   Note: `continue` keeps the nonce-sorted iteration intact and only
   exempts the system-contract tx from being the truncation point.
   Subsequent non-conforming non-system-contract txs at higher nonces still
   cause truncation at their position (semantics: "skip the *exempt* tx
   for truncation decisions, but the next non-exempt non-conforming tx
   still cuts the list").

**Rationale**: The deadlock root cause is the static MinTip gate
(`core/txpool/validation.go:118-131`) rejecting the restoration proposal
at Add-time. The fix mirrors the existing `local` exemption — same
mechanism, same caller-side signal (`opts.MinTip = 0`), so
`validation.go` itself is **not** modified (cherry-pick safety, RI-09).
The Pending filter mirrors the entry gate so a system-contract tx
admitted at Add can't be silently dropped at sealing time.

**Dependencies**: independent of Step 1. Step 2 can land first.

**Verification**:
- `go build ./core/txpool/legacypool/...`
- `go test -race -run 'TestRepricing|TestMinGasPriceEnforced|TestMinimumGasFeeValidation' ./core/txpool/legacypool/...`
- One-line success criterion: A tx with `to == GovValidator.Address` and
  `gasTipCap = 1` Gwei enters the pool's `pending` set even when
  `pool.gasTip == 30000` Gwei (would otherwise be `ErrUnderpriced`).

---

## Step 3: Layer 2 — `LegacyPool.SetGasTip` lowering reaction

**Target file**: `core/txpool/legacypool/legacypool.go`

**Target symbols**:
- Modify: `LegacyPool.SetGasTip` (line 472-496).

**Line anchors (pre-change)**:
- Lock acquisition: `pool.mu.Lock(); defer pool.mu.Unlock()` — line 473-474.
- Early-return on equal tip — line 481-483.
- Raising-side drop — line 487-494.
- (Reference) `requestPromoteExecutables` post-unlock pattern in `addRemotes` — line 1113-1126 (Lock at 1113, Unlock at 1115, `requestPromoteExecutables` at 1126).
- `pool.all.Range` (lookup.Range with its own RLock) — line 1923-1941.

**Concrete change**:

Restructure `SetGasTip` to (a) remove `defer pool.mu.Unlock()` and use an
explicit unlock point so the post-unlock `requestPromoteExecutables`
call is unambiguous, (b) keep the raising-drop branch byte-for-byte
intact, (c) add a lowering branch that clears all per-tx caches, reheaps,
collects the queued-account set, and after unlock requests promotion.

```go
func (pool *LegacyPool) SetGasTip(tip *big.Int) {
    pool.mu.Lock()

    var (
        newTip = uint256.MustFromBig(tip)
        old    = pool.gasTip.Load()
    )

    if newTip.Cmp(old) == 0 {
        pool.mu.Unlock()
        return
    }

    pool.gasTip.Store(newTip)

    var promoteSet *accountSet // populated only on lowering; consumed AFTER unlock
    switch newTip.Cmp(old) {
    case 1:
        // Raising: drop underpriced remote txs (existing semantics, unchanged).
        drop := pool.all.RemotesBelowTip(tip)
        for _, tx := range drop {
            pool.removeTx(tx.Hash(), false, true)
        }
        pool.priced.Removed(len(drop))
    case -1:
        // Lowering: previously a no-op. Now (i) clear per-tx anzeonTipCap
        // caches so EffectiveGasTip recomputes against the new env on
        // next read, (ii) Reheap so pricedList orders by the possibly
        // changed effective tip, (iii) collect addresses that hold queued
        // txs and request a queue->pending re-promotion AFTER unlock.
        pool.all.Range(func(_ common.Hash, tx *types.Transaction, _ bool) bool {
            tx.ClearAnzeonTipCap()
            return true
        }, true, true)
        pool.priced.Reheap()
        promoteSet = newAccountSet(pool.signer)
        for addr := range pool.queue {
            promoteSet.add(addr)
        }
    }
    log.Info("Legacy pool tip threshold updated", "tip", newTip)

    pool.mu.Unlock()

    // requestPromoteExecutables sends on pool.reqPromoteCh, which is
    // consumed by scheduleReorgLoop -> runReorg, which itself takes
    // pool.mu. Calling it with pool.mu still held would deadlock. This
    // mirrors the pattern in addRemotes (line 1113-1126).
    if promoteSet != nil && len(promoteSet.accounts) > 0 {
        pool.requestPromoteExecutables(promoteSet)
    }
}
```

**Rationale**:
- `pool.all.Range` holds the inner `lookup.lock` (RLock). The outer
  `pool.mu.Lock` is held in this call, giving the lock order
  `pool.mu → lookup.lock` — already the order used elsewhere in this
  file (e.g. line 489 `pool.all.RemotesBelowTip` inside `SetGasTip`).
- `requestPromoteExecutables` sends on `pool.reqPromoteCh` (line 1276)
  which is consumed by `scheduleReorgLoop` (line 1294, see `case req :=
  <-pool.reqPromoteCh` at line 1330). `runReorg` takes `pool.mu`. So
  calling `requestPromoteExecutables` under `pool.mu` deadlocks
  (orchestrator → blocked send / receiver waiting on mutex). The fix is
  to **release `pool.mu` first** — identical pattern to
  `addRemotes` (line 1113-1126).
- The `anzeonTipCap` cache must be cleared **before** Reheap so that the
  `pricedList`-internal `EffectiveGasTip*` calls observe the fresh env.

**Dependencies**: depends on Step 1 (`ClearAnzeonTipCap` exists).

**Verification**:
- `go build ./core/txpool/legacypool/...`
- `go test -race -run 'TestRepricing|TestMinGasPriceEnforced|TestRepricingKeepsLocals|TestRepricingShiftAccountAge|TestRepricingDynamicReflection' ./core/txpool/legacypool/...`
- One-line success criterion: after the sequence
  `SetGasTip(27600)` → add tx@27600 → `SetGasTip(30000)` (tx is dropped
  / moved to queue) → `SetGasTip(27600)`, that tx becomes pending again.

---

## Step 4: Layer 2 mirror — `BlobPool.SetGasTip` cache-clear on lowering

**Target file**: `core/txpool/blobpool/blobpool.go`

**Target symbols**:
- Modify: `BlobPool.SetGasTip` (line 1018-1085).

**Line anchors (pre-change)**:
- Lock acquisition — line 1019-1020.
- Asymmetric branch — line 1033 (`if old == nil || p.gasTip.Cmp(old) > 0`).
- `p.lookup` (hash → id) — line 310-311.
- `p.index[addr]` (per-account `*blobTxMeta` slice) — line 311.

**Concrete change**:

Add an `else` branch matching the existing `if old == nil || p.gasTip.Cmp(old) > 0` raising block. Note: `*blobTxMeta` does NOT hold a `*types.Transaction` directly. We must look up the tx via the blob store (`p.store.Get(meta.id)` returns the raw bytes — but the simpler approach is to walk `p.lookup` and for each hash, retrieve the in-memory tx via `p.get(hash)` if available). However, the blobpool's design caches `execTipCap` on `blobTxMeta` (line 1036) rather than relying on `Transaction.anzeonTipCap`. Therefore, for the blobpool, **the only required Layer 2 action is to NOT skip the lowering branch entirely**; blob txs are sorted by `execTipCap` and the next Pending pass naturally re-considers them. The change here is minimal:

```go
// (Inside SetGasTip, after raising branch, before "log.Debug ..."):
//
// On lowering, the blobpool's lazy reheap will re-consider previously
// underpriced txs at the next Pending pass. Clear any cached Anzeon tip
// on the underlying Transaction objects so EffectiveGasTip recomputes
// against the new env when consumers read it (e.g. via Pending()).
if old != nil && p.gasTip.Cmp(old) < 0 {
    for _, txs := range p.index {
        for _, meta := range txs {
            if tx := p.get(meta.hash); tx != nil {
                tx.ClearAnzeonTipCap()
            }
        }
    }
}
```

If `BlobPool` does not expose a `(hash) -> *types.Transaction` accessor
inside the package (it does — `BlobPool.Get(hash)` at line 1188+
returns `*types.Transaction`), use that. If only `meta` is available and
the underlying Transaction isn't easily reachable without I/O, this step
becomes a no-op: blobpool blobs reload from the billy store and
construct a fresh `*types.Transaction` whose `anzeonTipCap` is already
empty. The Implementer chooses based on the in-file API at HEAD
0bf2f4d1.

**Rationale**: Blobpool's `SetGasTip` is symmetric to legacy in
asymmetry but **its tip ordering uses `execTipCap` stored on
`blobTxMeta`**, not the `Transaction.anzeonTipCap` cache. So the
companion-defect surface here is narrower: only the per-tx
`anzeonTipCap` cache needs clearing if blob txs flow through the same
Pending filter. The diagnosis and analysis explicitly mark blobpool's
Layer 1 exemption as **out of scope** (governance proposals are not
blob txs); only the Layer 2 cache mirror applies.

**Dependencies**: depends on Step 1 (`ClearAnzeonTipCap` exists).

**Verification**:
- `go build ./core/txpool/blobpool/...`
- `go test -race ./core/txpool/blobpool/...` — existing blob tests stay
  green.
- One-line success criterion: existing blobpool tests still pass; no new
  cache staleness introduced on lowering.

---

## Step 5: Regression unit tests (legacypool_test.go) — both AC-critical

**Target file**: `core/txpool/legacypool/legacypool_test.go`

**Target symbols (new)**:
- `TestRepricingDynamicReflection` — AC1.
- `TestSystemContractTxExemptFromMinTip` — AC2 + AC3.
- (Optional small helper) `assertNoStaleAnzeonTipCache(t, pool)` — invariant from analysis §"Self-checking invariant".

**Anchors**:
- Use `setupPoolWithConfig(params.TestWBFTChainConfig)` (line 215) to
  obtain a pool with Anzeon + SystemContracts configured. `TestChainConfig`
  has no Anzeon, so the exemption set is empty under
  `setupPool()` and existing tests are unaffected — verify this with a
  no-Anzeon control.
- Place new tests after `TestRepricing` (line 1458) for visual proximity.

### 5a. TestRepricingDynamicReflection (AC1 — must FAIL on pre-fix)

Asserts the 27600 → 30000 → 27600 lifecycle:

```
1. setupPoolWithConfig(params.TestWBFTChainConfig) (Anzeon-aware)
2. Fund a regular sender (not in pool.locals, not a system contract).
3. pool.SetGasTip(27600) — base floor.
4. addRemote(tx with gasTipCap=27600, to=regularSender).  → must be admitted (pending).
5. pool.SetGasTip(30000)  → tx must be dropped from pending (existing
   raise behaviour) — pre-fix passes here.
6. pool.SetGasTip(27600)  → after this point, an equivalent
   gasTipCap=27600 tx (same sender, NEXT nonce) added now MUST be
   admitted to pending; AND any tx that was demoted to queue under
   step 5 must auto-promote back to pending.
7. assertNoStaleAnzeonTipCache(t, pool): every tx in pool.all has
   either nil cached tip OR a cached tip == anzeonTipEnv.GetAnzeonTipCap(tx).
```

**Why this fails on pre-fix**: step 6 fails — on pre-fix, the second
`SetGasTip(27600)` is a no-op (`newTip < old` branch absent), so the
queued tx is not promoted and a re-add attempt with the same nonce/
sender hits "known transaction" or the cache stale path. With Step 3 in
place, the lowering branch clears caches, reheaps, and requests
promotion → tx flows back into pending.

### 5b. TestSystemContractTxExemptFromMinTip (AC2/AC3 — must FAIL on pre-fix)

Iterates over each of the 5 system contracts in
`TestWBFTChainConfig.Anzeon.SystemContracts`:

```
1. setupPoolWithConfig(params.TestWBFTChainConfig).
2. pool.SetGasTip(30000) — raise the floor.
3. For each contract address in {GovValidator, GovCouncil, GovMinter,
   GovMasterMinter, NativeCoinAdapter}:
   a. Build a remote tx with to = contract.Address, gasTipCap = 1 Gwei,
      gasFeeCap = 1 Gwei (clearly below floor).
   b. err := pool.addRemote(tx) — must be nil (admitted to the pool).
   c. The tx must appear in pool.Pending(filter with MinTip=30000) — i.e.
      NOT truncated by the Pending tip filter.
4. Negative control: same tx with to = random EOA → must be rejected with
   txpool.ErrUnderpriced (gate still active for non-system-contract txs).
```

**Why this fails on pre-fix**: step 3.b fails with `ErrUnderpriced`
because pre-fix `validateTxBasics` does not exempt system-contract txs.
With Step 2 in place, the exemption is honoured.

**Dependencies**: depends on Steps 1, 2, 3 (need cache clear + exemption + lowering reaction).

**Verification**:
- `go test -race -run 'TestRepricingDynamicReflection|TestSystemContractTxExemptFromMinTip' ./core/txpool/legacypool/...`
- One-line success criterion: both new tests PASS post-fix and FAIL on
  HEAD before fix (regression detection power).

---

## Step 6: Chain simulation test (multiengine_test.go pattern)

**Target file (new)**: `core/txpool/legacypool/gastip_restore_sim_test.go`

**Target symbol (new)**: `TestGasTipRestoreChainSim`

**Pattern reference**:
- `consensus/wbft/backend/multiengine_test.go:62-130` (`MakeMultiEngineTestEnv`).
- Uses `consensus/wbft/testutils.GenesisAndFixedKeys(n)` (`testutils/genesis.go`).
- Builds `core.NewBlockChain` via `rawdb.NewMemoryDatabase()` + `triedb.NewDatabase`.

**Why a separate file**: keep the chain-construction boilerplate
isolated from `legacypool_test.go` (which uses the lightweight
`newTestBlockChain` fake). This file uses the *real* `core.BlockChain`
+ genesis so it more closely mirrors what happens in a live node.

**Test outline**:

```
1. testutils.GenesisAndFixedKeys(4)  → genesis with SystemContracts
   populated at DefaultGovValidatorAddress etc.
2. Build core.BlockChain with this genesis (in-memory rawdb).
3. Build LegacyPool wired to this chain.
4. Inject a regular sender key + a "governance signer" key.
5. Sequence:
   a. pool.SetGasTip(27600)
   b. Submit regularSender tx (nonce 0, gasTipCap=27600). Assert in pending.
   c. pool.SetGasTip(30000) — regularSender tx is dropped/demoted.
   d. Submit governance restore tx (to=GovValidator.Address,
      gasTipCap=1 Gwei). Assert in pending (Layer 1 exemption).
   e. pool.SetGasTip(27600). Assert regularSender tx auto-promotes back
      to pending (Layer 2 lowering reaction).
6. Inspect pool.Pending(...) and verify both governance tx and
   regularSender tx appear and are not truncated.
7. Run under `go test -race`.
```

**Note**: this test does NOT drive the WBFT engine to seal blocks
(that would replicate `multiengine_test.go` wholesale). It uses the
multiengine *chain construction* pattern but stops at "Pending() returns
the expected txs", which is what the sealer would consume.

**Dependencies**: depends on Steps 1-3.

**Verification**:
- `go test -race -run 'TestGasTipRestoreChainSim' ./core/txpool/legacypool/...`
- One-line success criterion: the gov restore tx and the auto-promoted
  regular tx both appear in `pool.Pending(...)` after the final
  `SetGasTip(27600)`; pre-fix code fails on this output.

---

## Verification Plan

- **Unit tests (per step)**: listed in each step's Verification.
- **Integration tests (cross-package)**: Step 6's chain-sim test exercises
  `core/txpool/legacypool` + `consensus/wbft/testutils` + `core/rawdb`
  + `triedb` together.
- **`go build` verification**: required after every step.
  - Step 1: `go build ./core/types/...`
  - Step 2-3, 5, 6: `go build ./core/txpool/legacypool/...`
  - Step 4: `go build ./core/txpool/blobpool/...`
  - Final: `go build ./...`
- **`go test -race` scope (RI-21)**: derived from CKG `concurrency_impact`
  (`pool.mu` is the only synchronization point, depth 3, single
  module). Required race scope:
  - `go test -race ./core/types/...`
  - `go test -race ./core/txpool/legacypool/...`
  - `go test -race ./core/txpool/blobpool/...`
- **ChainBench**: Not required for txpool entry-gate semantics; no
  consensus/governance/state-root change in this PR.
- **Acceptance Criteria coverage** (from analysis.md §"Acceptance Criteria Mapping"):
  - AC1 ← Step 3 + Step 5a (`TestRepricingDynamicReflection`).
  - AC2 ← Step 2 + Step 5b (`TestSystemContractTxExemptFromMinTip`).
  - AC3 ← Step 2 helpers (chainconfig-derived, no hardcoded addresses).
  - AC4 ← Both new tests must FAIL on pre-fix HEAD (regression power).
  - AC5 ← Step 6 (`TestGasTipRestoreChainSim`).
  - AC6 ← `-race` scope above.
  - AC7 ← Branch already `fix/pr-77-gastip-dynamic-reflection-test7`.

## Risks

1. **Deadlock with `scheduleReorgLoop` (RI-21)**: `requestPromoteExecutables`
   sends on `pool.reqPromoteCh`, consumed by `scheduleReorgLoop`, which
   calls `runReorg` which takes `pool.mu`. **Mitigation**: Step 3
   releases `pool.mu` via explicit `pool.mu.Unlock()` (no `defer`)
   before invoking `requestPromoteExecutables`, mirroring the
   `addRemotes` pattern at line 1113-1126.
2. **Cache staleness (`atomic.Value.Store(nil)` panics)**: Layer 2 cache
   clear must use **typed-nil store** (`Store((*big.Int)(nil))`), not
   `Store(nil)`. **Mitigation**: Step 1 implements `ClearAnzeonTipCap`
   with typed nil, and `GetAnzeonTipCap` checks `bi != nil` to treat
   typed-nil as "unset".
3. **Whitelist drift**: hardcoding addresses would diverge from
   chainconfig. **Mitigation**: Step 2's helpers read **only** from
   `pool.chainconfig.Anzeon.SystemContracts` with nil-guards.
4. **Test fixture regressions**: existing tests use `params.TestChainConfig`
   which has no Anzeon. **Mitigation**: `systemContractAddresses()`
   returns empty set when `Anzeon == nil` → exemption inactive →
   existing behaviour preserved.
5. **Cherry-pick safety (RI-09)**: putting StableNet-specific code in
   `core/txpool/validation.go` would break upstream rebase.
   **Mitigation**: zero changes to `validation.go`. Caller signals
   exemption via `opts.MinTip = 0` (same mechanism as `local`).
6. **Pending ordering**: nonce gaps in the truncation loop must not be
   misinterpreted. **Mitigation**: Step 2's Pending patch uses `continue`
   inside the `for i, tx := range txs` loop so iteration proceeds; a
   subsequent non-conforming non-system-contract tx still truncates at
   its actual index.
7. **Out-of-scope**: Layer 3 (`AnzeonTipEnv.SetCurrentBlock` same-Root
   skip) is a real companion defect but **explicitly deferred** per
   ticket. This PR does not touch `eth/gasprice/anzeon.go`.
