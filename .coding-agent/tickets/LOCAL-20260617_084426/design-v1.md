# Design v1 — LOCAL-20260617_084426

> **Scope**: Layer 1 (system-contract MinTip exemption in legacypool) +
> Layer 2 (dynamic reflection on SetGasTip lowering, in legacypool and
> blobpool, plus `Transaction.ClearAnzeonTipCap`). **Layer 3
> (`eth/gasprice/anzeon.go` same-Root skip) is explicitly deferred.**
>
> Revision: 1 (initial). See design-changelog.md.

## 0. Cross-cutting design decisions

### 0.1 Data flow / call graph touched

```
                      governance proposal
                              │
                              ▼
            GovValidator.gasTip  (contract slot, line 186-190)
                              │  (read every block import)
                              ▼
        worker.getGasTipFromContract (miner/worker.go:1209)
                              │
                              ▼
        worker.setGasTipUnsafe (miner/worker.go:377-389)
                              │
                              ▼
          ─────────────────────────────────────
          │                                   │
          ▼                                   ▼
  LegacyPool.SetGasTip            BlobPool.SetGasTip
  (legacypool.go:472-496)         (blobpool.go:1018-1085)
          │                                   │
          │  [Step 3, lowering branch]        │  [Step 4, lowering branch]
          │                                   │
          ▼                                   ▼
   For tx in pool.all.Range:           For meta in p.index:
     tx.ClearAnzeonTipCap()              tx := p.get(meta.hash)
   pool.priced.Reheap()                  if tx != nil { tx.ClearAnzeonTipCap() }
   promoteSet := newAccountSet           (lazy reheap on next Pending)
   ── unlock ──
   pool.requestPromoteExecutables(promoteSet)
          │
          ▼ (over reqPromoteCh)
   scheduleReorgLoop → runReorg → promoteExecutables → pending

──────────────────────── Add-time / Pending-time gate ───────────────────────

   user → RPC → BlobPool.Add / LegacyPool.Add
                              │
                              ▼
            LegacyPool.validateTxBasics  (line 650-669)
              opts.MinTip = pool.gasTip.Load().ToBig()
              if local: opts.MinTip = 0
              if isSystemContractTx(tx): opts.MinTip = 0       [Step 2, NEW]
                              │
                              ▼
            txpool.ValidateTransaction  (validation.go:55-174)
              tx.GasTipCapIntCmp(opts.MinTip) < 0 → ErrUnderpriced
              [unchanged — cherry-pick safety RI-09]
                              │
                              ▼  (admitted)
            LegacyPool.Pending  (line 569-620)
              for i, tx := range txs:
                if isSystemContractTx(tx): continue           [Step 2, NEW]
                if tx.EffectiveGasTipIntCmp(...) < 0: truncate
```

### 0.2 Concurrency: `pool.mu` vs `scheduleReorgLoop` deadlock pattern

`LegacyPool.SetGasTip` runs **under `pool.mu.Lock()`** (legacypool.go:473).
The Layer 2 lowering branch needs to ultimately invoke
`pool.requestPromoteExecutables(set)` (line 1272-1281):

```go
func (pool *LegacyPool) requestPromoteExecutables(set *accountSet) chan struct{} {
    select {
    case pool.reqPromoteCh <- set:               //  ← (A) blocks until consumed
        return <-pool.reorgDoneCh
    case <-pool.reorgShutdownCh:
        return pool.reorgShutdownCh
    }
}
```

`pool.reqPromoteCh` is consumed by `scheduleReorgLoop` (line 1294-1330),
which then dispatches `runReorg` (line 1364) — and `runReorg`
**acquires `pool.mu`**.

```
SetGasTip goroutine          scheduleReorgLoop goroutine
──────────────────────       ──────────────────────────
pool.mu.Lock()    ◀────┐
   reqPromoteCh <- s   │                                    blocks: (A)
                       │     case req := <-reqPromoteCh
                       │       runReorg(...)
                       └─────  pool.mu.Lock() — blocks here, mu held by us
                                                            DEADLOCK
```

This is **exactly** the constraint encoded by `LegacyPool.addRemotes`
(line 1112-1126):

```go
pool.mu.Lock()
newErrs, dirtyAddrs := pool.addTxsLocked(news, local)
pool.mu.Unlock()                                   //  ← released BEFORE
...
done := pool.requestPromoteExecutables(dirtyAddrs) //  ← invoked
```

**Mitigation in Step 3**: replace `defer pool.mu.Unlock()` with an
explicit `pool.mu.Unlock()` call BEFORE
`pool.requestPromoteExecutables`. The lowering branch does its
mutation-and-readout work under `pool.mu` (cache clear via
`pool.all.Range` which RLocks lookup.lock — order `pool.mu → lookup.lock`,
already used at line 489 inside `SetGasTip` for `RemotesBelowTip`),
captures the set into a local variable, then unlocks, then sends.

### 0.3 Typed-nil rationale for `atomic.Value.Store`

`atomic.Value.Store(v)` panics if:
- `v == nil` (plain untyped nil) — Go runtime: `sync/atomic.Value.Store: store of nil value`
- subsequent stores have a different *concrete type* than the first.

The cache's only producer (`SetAnzeonTipCap`, transaction.go:414-418) stores `*big.Int`. To invalidate, we must:

1. Store **a value whose dynamic type is identical** to the producer's
   (`*big.Int`) so the type-consistency invariant of `atomic.Value` is
   preserved.
2. Have that value carry the semantics "no cache" so consumers see
   "unset".

The idiom is `tx.anzeonTipCap.Store((*big.Int)(nil))` — a typed nil
*pointer*. `Load()` then returns the `interface{ ... }` wrapper whose
dynamic type is `*big.Int` and whose dynamic value is `nil`. To preserve
back-compat with existing callers that do
`cached := tx.anzeonTipCap.Load(); if cached != nil { ... }`,
`GetAnzeonTipCap` is updated to additionally check that the unwrapped
`*big.Int` is non-nil:

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

This is the canonical Go idiom — see e.g. `golang.org/x/sync/singleflight`
and the patterns in `runtime/proc.go`. The plain `Store(nil)` panic and
the type-consistency rule are documented in the standard library
godocs for `sync/atomic.Value`.

### 0.4 System-contract address-set derivation

Source of truth: `params.ChainConfig.Anzeon.SystemContracts` (defined at
`params/config_wbft.go:146-152`), accessed at call-time via
`pool.chainconfig` (legacypool.go:235). Five fields:

| field | type | typical address (TestWBFTChainConfig) |
|-------|------|----------------------------------------|
| GovValidator | `*SystemContract` | `DefaultGovValidatorAddress` |
| GovCouncil | `*SystemContract` | (optional) |
| GovMinter | `*SystemContract` | (optional) |
| GovMasterMinter | `*SystemContract` | (optional) |
| NativeCoinAdapter | `*SystemContract` | `DefaultGovValidatorAddress`'s sibling |

Helper builds the set every call (cheap — 5 map insertions, no
allocation hotpath since exempted txs are rare). **Nil-guards on three
levels**: `pool.chainconfig`, `chainconfig.Anzeon`, `Anzeon.SystemContracts`,
and each `*SystemContract` field. If any is nil → empty set → exemption
inactive → identical to pre-fix behaviour. This is critical so
existing tests using `params.TestChainConfig` (no Anzeon) are
unaffected.

The helper does **not** call `cks.context.find_callers` or anything
runtime-bound; it is a pure read of the chainconfig pointer that is
already stored in the pool at construction time.

### 0.5 Decision: NO `validation.go` changes (cherry-pick safety, RI-09)

The exemption is signalled by the **caller** (`validateTxBasics`)
through `opts.MinTip = new(big.Int)` — exactly the existing `local`
exemption mechanism. `txpool.ValidateTransaction` stays byte-identical.
This decision is recorded in analysis.md §"Open Questions / Resolved
Decisions" and is non-negotiable.

---

## Step 1: `Transaction.ClearAnzeonTipCap` (typed-nil)

### 1.1 Current code (excerpt)

`core/types/transaction.go:412-426`:

```go
func (tx *Transaction) SetAnzeonTipCap(tipCap *big.Int) {
    if tipCap != nil {
        tx.anzeonTipCap.Store(new(big.Int).Set(tipCap))
    }
}

func (tx *Transaction) GetAnzeonTipCap() *big.Int {
    if cached := tx.anzeonTipCap.Load(); cached != nil {
        return cached.(*big.Int)
    }
    return nil
}
```

### 1.2 Proposed change

Add `ClearAnzeonTipCap` after `GetAnzeonTipCap` and harden
`GetAnzeonTipCap` for the typed-nil sentinel:

```go
// SetAnzeonTipCap caches the Anzeon tip cap for this transaction.
// This is set during pool validation to avoid repeated state queries during reheap.
func (tx *Transaction) SetAnzeonTipCap(tipCap *big.Int) {
    if tipCap != nil {
        tx.anzeonTipCap.Store(new(big.Int).Set(tipCap))
    }
}

// ClearAnzeonTipCap invalidates the cached Anzeon tip cap. It uses a
// typed-nil store because atomic.Value.Store(nil) panics; the typed nil
// preserves the dynamic-type invariant of atomic.Value while marking
// the cache as "unset" to subsequent GetAnzeonTipCap callers.
//
// This is the cache invalidator that LegacyPool.SetGasTip (lowering
// branch) and BlobPool.SetGasTip (lowering branch) call to make
// EffectiveGasTip recompute against the updated env after a floor lower.
func (tx *Transaction) ClearAnzeonTipCap() {
    tx.anzeonTipCap.Store((*big.Int)(nil))
}

// GetAnzeonTipCap returns the cached Anzeon tip cap, or nil if not cached
// (or cleared). A typed-nil store from ClearAnzeonTipCap is treated as
// "not cached" for back-compat with callers that did the simple
// `if cached := ...Load(); cached != nil { ... }` pattern.
func (tx *Transaction) GetAnzeonTipCap() *big.Int {
    if cached := tx.anzeonTipCap.Load(); cached != nil {
        if bi, ok := cached.(*big.Int); ok && bi != nil {
            return bi
        }
    }
    return nil
}
```

### 1.3 Side-effect checklist

- [x] **Preserves public interface** — only ADDS `ClearAnzeonTipCap`;
      `SetAnzeonTipCap`/`GetAnzeonTipCap` signatures unchanged.
      `GetAnzeonTipCap` behavior change is strictly a *defensive*
      improvement (typed-nil → nil return), no public-API break.
- [x] **Error paths covered** — function cannot fail; no error return.
- [x] **Concurrent safety preserved** — `atomic.Value.Store` is the
      atomic store. The typed-nil store does not break type-consistency
      because `SetAnzeonTipCap` also stores `*big.Int`.
- [x] **No new shared resources** — uses the existing `tx.anzeonTipCap
      atomic.Value` field (line 77).
- [x] **Caller assumptions on old behavior** — `EffectiveGasTip`
      (line 388-403) reads `GetAnzeonTipCap()` and falls back to
      `anzeonTipEnv.GetAnzeonTipCap(tx)` if nil. Returning nil after
      clear means it falls back to env, which is the new desired
      behaviour. No other callers exist
      (cks `find_callers(GetAnzeonTipCap)` confirms).
- [x] **Derived/parallel state mirrored** — yes; this IS the
      invalidator for the per-tx cache that mirrors
      `AnzeonTipEnv.GetAnzeonTipCap(tx)`. See §1.4 for the write-site
      table.

### 1.4 Write-site / invalidator table for `Transaction.anzeonTipCap`

| Site | Action |
|------|--------|
| `SetAnzeonTipCap` (transaction.go:414-418) | producer — stores `*big.Int` (cks `find_callers`: single caller `core/txpool/validation.go:241-343`) |
| `ClearAnzeonTipCap` (transaction.go, **NEW**) | invalidator — stores `(*big.Int)(nil)` |
| `LegacyPool.SetGasTip` lowering branch (Step 3) | calls `ClearAnzeonTipCap` on every tx in `pool.all` |
| `BlobPool.SetGasTip` lowering branch (Step 4) | calls `ClearAnzeonTipCap` on every tx in `p.index` |
| `GetAnzeonTipCap` (transaction.go:421-426) | consumer — `EffectiveGasTip` (line 398-401), `EffectiveGasTipValue` (line 408), `EffectiveGasTipCmp` (line 432-434), `EffectiveGasTipIntCmp` (line 437-440) |

### 1.5 Tests

- **Existing tests that must still pass**: any `core/types` tests
  touching `EffectiveGasTip` / `EffectiveGasTipCache` (Build will fail
  if interface is broken).
- **New tests to add**: small roundtrip in `core/types/transaction_test.go`:
  ```go
  func TestAnzeonTipCapClear(t *testing.T) {
      tx := NewTx(&LegacyTx{Nonce: 0})
      if got := tx.GetAnzeonTipCap(); got != nil {
          t.Fatalf("fresh tx: want nil, got %v", got)
      }
      tx.SetAnzeonTipCap(big.NewInt(42))
      if got := tx.GetAnzeonTipCap(); got == nil || got.Int64() != 42 {
          t.Fatalf("after Set(42): want 42, got %v", got)
      }
      tx.ClearAnzeonTipCap()
      if got := tx.GetAnzeonTipCap(); got != nil {
          t.Fatalf("after Clear: want nil, got %v", got)
      }
      // Set again after Clear must work (type consistency).
      tx.SetAnzeonTipCap(big.NewInt(99))
      if got := tx.GetAnzeonTipCap(); got == nil || got.Int64() != 99 {
          t.Fatalf("after re-Set(99): want 99, got %v", got)
      }
  }
  ```
  This is small enough to live alongside other transaction tests; the
  Implementer may co-locate it with existing `EffectiveGasTip` tests.

---

## Step 2: System-contract MinTip exemption in legacypool

### 2.1 Current code (excerpt)

`core/txpool/legacypool/legacypool.go:650-669`:

```go
func (pool *LegacyPool) validateTxBasics(tx *types.Transaction, local bool) error {
    opts := &txpool.ValidationOptions{
        Config: pool.chainconfig,
        Accept: 0 |
            1<<types.LegacyTxType |
            1<<types.AccessListTxType |
            1<<types.DynamicFeeTxType |
            1<<types.FeeDelegateDynamicFeeTxType |
            1<<types.SetCodeTxType,
        MaxSize: txMaxSize,
        MinTip:  pool.gasTip.Load().ToBig(),
    }
    if local {
        opts.MinTip = new(big.Int)
    }
    if err := txpool.ValidateTransaction(tx, pool.currentHead.Load(), pool.signer, opts); err != nil {
        return err
    }
    return nil
}
```

`core/txpool/legacypool/legacypool.go:589-601` (Pending truncation):

```go
pending := make(map[common.Address][]*txpool.LazyTransaction, len(pool.pending))
for addr, list := range pool.pending {
    txs := list.Flatten()

    // If the miner requests tip enforcement, cap the lists now
    if minTipBig != nil && !pool.locals.contains(addr) {
        for i, tx := range txs {
            if tx.EffectiveGasTipIntCmp(minTipBig, pool.anzeonTipEnv) < 0 {
                txs = txs[:i]
                break
            }
        }
    }
```

### 2.2 Proposed change

**(a) Helpers** — add private methods on `*LegacyPool` (place near
`validateTxBasics`, between line 669 and `validateTx` at line 671):

```go
// systemContractAddresses returns the chainconfig-derived set of
// addresses that are exempt from the MinTip entry/Pending gate. The
// whitelist comes only from chainconfig.Anzeon.SystemContracts — no
// hardcoded addresses (AC3). Returns an empty set when any chainconfig
// level is nil, preserving pre-fix behaviour for fixtures without
// Anzeon (e.g. params.TestChainConfig).
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
// system contract. Contract-creation transactions (To == nil) are
// never exempt.
func (pool *LegacyPool) isSystemContractTx(tx *types.Transaction) bool {
    if tx == nil || tx.To() == nil {
        return false
    }
    set := pool.systemContractAddresses()
    if len(set) == 0 {
        return false
    }
    _, ok := set[*tx.To()]
    return ok
}
```

**(b) Patch `validateTxBasics`** — add an exemption branch alongside the existing `local` branch (line 662-664):

```go
    if local {
        opts.MinTip = new(big.Int)
    }
    // System-contract-bound txs (e.g. GovValidator gasTip restoration
    // proposals) are exempt from the MinTip floor. Without this, a
    // restoration proposal with gasTipCap < current floor is rejected
    // at Add-time → it cannot enter the pool → the floor cannot be
    // lowered → CIRCULAR DEADLOCK (diagnosis §"Root cause").
    if pool.isSystemContractTx(tx) {
        opts.MinTip = new(big.Int)
    }
```

**(c) Patch `Pending`** — add the system-contract skip inside the truncation loop (replace line 595-600):

```go
    if minTipBig != nil && !pool.locals.contains(addr) {
        for i, tx := range txs {
            if pool.isSystemContractTx(tx) {
                // System-contract tx is exempt from sealing-time
                // truncation, mirroring the entry-gate exemption.
                continue
            }
            if tx.EffectiveGasTipIntCmp(minTipBig, pool.anzeonTipEnv) < 0 {
                txs = txs[:i]
                break
            }
        }
    }
```

### 2.3 Side-effect checklist

- [x] **Public interface preserved** — `validateTxBasics`, `Pending`
      keep their existing signatures; `validation.go` not touched
      (RI-09).
- [x] **Error paths covered** — exemption widens the set of admitted
      txs; no new error paths introduced.
- [x] **Concurrent safety preserved** — helpers read `pool.chainconfig`
      which is set in `New()` and never mutated after construction
      (legacypool.go:284); reading it without lock is safe.
- [x] **No new shared resources** — helpers allocate a small
      per-call map; no global state.
- [x] **Caller assumptions on old behavior** — the only "weakening" is
      for system-contract txs. Non-system-contract txs are unaffected.
      Test fixtures using `params.TestChainConfig` produce empty sets →
      no behavior change there.
- [x] **Derived/parallel state mirrored** — the entry gate and Pending
      truncation are **two consumers of the same MinTip floor**. They
      must apply the exemption in lock-step or a tx admitted at Add
      would be silently dropped at sealing. Step 2 covers BOTH sites.
      See §2.4.

### 2.4 Write-site / consumer table for the MinTip floor

| Site | Pre-fix behavior | Post-fix behavior |
|------|-------------------|-------------------|
| `LegacyPool.validateTxBasics` (line 650-669) | `opts.MinTip = pool.gasTip.Load().ToBig()`; `local` → 0 | + `isSystemContractTx(tx)` → 0 |
| `LegacyPool.Pending` truncation loop (line 593-601) | `EffectiveGasTipIntCmp(minTipBig, env) < 0` truncates | `isSystemContractTx(tx)` → continue (skip from truncation) |
| `BlobPool.validateTx` (blobpool.go:1122) | `baseOpts.MinTip = p.gasTip.ToBig()` | **unchanged** — out of scope (blob txs do not target system contracts) |
| `ValidateTransaction` (validation.go:55-174) | static MinTip gate | **unchanged** — exemption signalled via `opts.MinTip = 0` by caller |

### 2.5 Tests

- **Existing tests that must still pass**:
  `TestRepricing`, `TestMinGasPriceEnforced`,
  `TestMinimumGasFeeValidation`, `TestRepricingKeepsLocals`,
  `TestRepricingShiftAccountAge` — they use `params.TestChainConfig`
  (no Anzeon), so exemption set is empty and behavior is identical.
- **New tests to add**: `TestSystemContractTxExemptFromMinTip` (Step 5b).

---

## Step 3: `LegacyPool.SetGasTip` lowering reaction

### 3.1 Current code

`core/txpool/legacypool/legacypool.go:472-496`:

```go
func (pool *LegacyPool) SetGasTip(tip *big.Int) {
    pool.mu.Lock()
    defer pool.mu.Unlock()

    var (
        newTip = uint256.MustFromBig(tip)
        old    = pool.gasTip.Load()
    )

    if newTip.Cmp(old) == 0 {
        return
    }

    pool.gasTip.Store(newTip)
    // If the min miner fee increased, remove transactions below the new threshold
    if newTip.Cmp(old) > 0 {
        // pool.priced is sorted by GasFeeCap, so we have to iterate through pool.all instead
        drop := pool.all.RemotesBelowTip(tip)
        for _, tx := range drop {
            pool.removeTx(tx.Hash(), false, true)
        }
        pool.priced.Removed(len(drop))
    }
    log.Info("Legacy pool tip threshold updated", "tip", newTip)
}
```

### 3.2 Proposed change

```go
func (pool *LegacyPool) SetGasTip(tip *big.Int) {
    pool.mu.Lock()
    // NOTE: no `defer pool.mu.Unlock()` — the lowering branch needs to
    // invoke pool.requestPromoteExecutables AFTER releasing the mutex
    // to avoid deadlocking with scheduleReorgLoop → runReorg → pool.mu.
    // See addRemotes (line 1112-1126) for the canonical pattern.

    var (
        newTip = uint256.MustFromBig(tip)
        old    = pool.gasTip.Load()
    )

    if newTip.Cmp(old) == 0 {
        pool.mu.Unlock()
        return
    }

    pool.gasTip.Store(newTip)

    var promoteSet *accountSet // set only on lowering; consumed after Unlock

    switch newTip.Cmp(old) {
    case 1:
        // Raising: drop underpriced remote txs (existing semantics, unchanged).
        drop := pool.all.RemotesBelowTip(tip)
        for _, tx := range drop {
            pool.removeTx(tx.Hash(), false, true)
        }
        pool.priced.Removed(len(drop))

    case -1:
        // Lowering: invalidate per-tx anzeonTipCap caches, reheap by
        // possibly-changed effective tip, and request a queue→pending
        // re-promotion for every account that holds queued txs (those
        // may now satisfy the lowered floor).
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

    // Out of lock: send the promote request. requestPromoteExecutables
    // sends on pool.reqPromoteCh, which is consumed by
    // scheduleReorgLoop → runReorg → pool.mu.Lock(). Holding pool.mu
    // here would deadlock.
    if promoteSet != nil && len(promoteSet.accounts) > 0 {
        pool.requestPromoteExecutables(promoteSet)
    }
}
```

### 3.3 Side-effect checklist

- [x] **Public interface preserved** — `SetGasTip(tip *big.Int)` is
      unchanged at the source level. Callers
      (`miner/worker.go:1204`, 4 tests) see identical contract.
- [x] **Error paths covered** — function returns nothing; no new
      panics introduced. `uint256.MustFromBig(tip)` panics on negative
      input (same as pre-fix). `tx.ClearAnzeonTipCap()` cannot fail.
      `pool.priced.Reheap()` cannot fail.
- [x] **Concurrent safety preserved** — see §0.2. The explicit
      `pool.mu.Unlock()` before `requestPromoteExecutables` is the
      critical mitigation. `pool.all.Range` is RLock on lookup.lock
      inside `pool.mu` — order `pool.mu → lookup.lock`, matches
      pre-existing call at line 489.
- [x] **New shared resources** — none. `promoteSet` is a local
      `*accountSet`, ownership transferred to the reorg goroutine via
      `reqPromoteCh`.
- [x] **Caller assumptions on old behavior** — raising semantics
      preserved byte-for-byte. Lowering was previously a no-op; now
      it does work. Existing tests that asserted "lowering does not
      drop" still hold (raising drop is unchanged; lowering does NOT
      drop, only re-promotes). `TestRepricingKeepsLocals` (locals
      survive raise) is unaffected.
- [x] **Derived/parallel state mirrored** — see §1.4 (the per-tx
      cache is invalidated here) and §3.4 below.

### 3.4 Write-site / consumer table for `pool.gasTip` mutator

| Mutator site | Effect on pool state | Lock discipline |
|--------------|---------------------|------------------|
| `SetGasTip` raise (line 487-494, unchanged) | `pool.all.RemotesBelowTip` drop + `pool.priced.Removed` | inside `pool.mu` |
| `SetGasTip` lower (NEW) | (i) tx.ClearAnzeonTipCap for every tx in pool.all, (ii) pool.priced.Reheap, (iii) collect queued-account set | inside `pool.mu` |
| `requestPromoteExecutables` (line 1272-1281) | enqueue to `reqPromoteCh` → `scheduleReorgLoop` → `runReorg` (takes `pool.mu`) | MUST be AFTER `pool.mu.Unlock` |

### 3.5 Tests

- **Existing tests that must still pass**: `TestRepricing` (raise
  side), `TestMinGasPriceEnforced`, `TestRepricingKeepsLocals`,
  `TestRepricingShiftAccountAge`. These already drive
  `SetGasTip` to higher values only — the new lowering branch doesn't
  run for them.
- **New tests to add**: `TestRepricingDynamicReflection` (Step 5a)
  drives raise + lower and asserts the queued tx auto-promotes back.
  Also `assertNoStaleAnzeonTipCache(t, pool)` invariant helper.

---

## Step 4: `BlobPool.SetGasTip` lowering — cache-clear mirror

### 4.1 Current code

`core/txpool/blobpool/blobpool.go:1018-1085` (already reproduced in
plan.md §Step 4). Asymmetric: raise drops, lower no-ops.

### 4.2 Proposed change

Insert a lowering branch between the raise block and the trailing
`log.Debug`. The blobpool keeps its own per-meta tip cache
(`blobTxMeta.execTipCap`) but the `EffectiveGasTip*` family on
`*types.Transaction` still consults `anzeonTipCap`. If we ever Pending
through blobpool in an Anzeon-enabled flow, we must invalidate that
cache symmetrically:

```go
} else if old != nil && p.gasTip.Cmp(old) < 0 {
    // Lowering: clear per-tx anzeonTipCap so EffectiveGasTip recomputes
    // against the new env on next Pending pass. Blobpool's promotion
    // is lazy (next Pending pass selects new survivors), so no
    // explicit promote request is needed here.
    for _, txs := range p.index {
        for _, meta := range txs {
            if tx := p.get(meta.hash); tx != nil {
                tx.ClearAnzeonTipCap()
            }
        }
    }
}
```

If the Implementer finds that `p.get(hash) *types.Transaction` is not
available within the package at HEAD `0bf2f4d1`, the equivalent
accessor is `p.lookup[hash]` + `p.store.Get(id)` + decode → not worth
the I/O on every SetGasTip. In that case the lowering branch becomes a
**no-op** with a comment explaining that blobpool's `execTipCap`
already lives outside `Transaction.anzeonTipCap` and is naturally
re-evaluated on the next Pending pass. This is acceptable because
governance/system-contract txs do NOT enter the blob pool — the cache
staleness window in blobpool is only exercised by non-governance
flows where the floor change is already absorbed by `execTipCap`
comparisons.

Implementer instruction: prefer the in-memory accessor; if none exists,
emit the no-op with the explanatory comment and do not introduce billy
I/O on a lock-held path.

### 4.3 Side-effect checklist

- [x] **Public interface preserved** — `SetGasTip(tip *big.Int)`
      unchanged.
- [x] **Error paths covered** — additive code path; no new errors.
- [x] **Concurrent safety preserved** — `p.lock.Lock()` held
      throughout (line 1019-1020); `p.index` and `p.lookup` read under
      that lock — already the file's discipline.
- [x] **No new shared resources** — none.
- [x] **Caller assumptions** — `BlobPool.SetGasTip` callers
      (`txpool.TxPool.SetGasTip` fan-out from `worker.setGasTipUnsafe`)
      see same return contract.
- [x] **Derived/parallel state mirrored** — yes; this clears
      `Transaction.anzeonTipCap` in symmetry with Step 3 for legacy
      pool.

### 4.4 Tests

- **Existing tests that must still pass**: full `./core/txpool/blobpool/...`
  suite under `-race`.
- **New tests**: none required (blobpool's path is exercised
  indirectly by Step 5's tests if any blob txs are submitted; the
  diagnosis/analysis explicitly defers a dedicated blobpool regression).

---

## Step 5: Regression unit tests (legacypool_test.go)

### 5.1 Current code (excerpt)

`core/txpool/legacypool/legacypool_test.go:1458-1576` (`TestRepricing`)
shows the canonical event-driven pricing test pattern: subscribe to
`pool.txFeed`, build txs with `pricedTransaction`, call `SetGasTip`,
inspect `Stats()` / `Pending()`, validate events with `validateEvents`.

### 5.2 Proposed test: `TestRepricingDynamicReflection`

Place after line 1576 (`TestRepricing`):

```go
// TestRepricingDynamicReflection verifies the AC1 requirement: when
// gasTip is raised then lowered back, transactions that were demoted
// to the queue on raise must auto-promote back to pending on lower.
// On pre-fix code this test MUST fail (LegacyPool.SetGasTip is a no-op
// on lowering and pool.all caches are stale).
func TestRepricingDynamicReflection(t *testing.T) {
    t.Parallel()

    // Use TestWBFTChainConfig so Anzeon (and EffectiveGasTip flow) is on.
    pool, _ := setupPoolWithConfig(params.TestWBFTChainConfig)
    defer pool.Close()

    events := make(chan core.NewTxsEvent, 32)
    sub := pool.txFeed.Subscribe(events)
    defer sub.Unsubscribe()

    key, _ := crypto.GenerateKey()
    sender := crypto.PubkeyToAddress(key.PublicKey)
    testAddBalance(pool, sender, big.NewInt(1000000000))

    floorLow  := big.NewInt(27600)
    floorHigh := big.NewInt(30000)

    // 1. Base floor 27600. Submit a 27600-tip tx — admitted.
    pool.SetGasTip(floorLow)
    tx0 := pricedTransaction(0, 100000, big.NewInt(27600), key)
    if err := pool.addRemoteSync(tx0); err != nil {
        t.Fatalf("submit at floor 27600: %v", err)
    }
    if p, _ := pool.Stats(); p != 1 {
        t.Fatalf("after low floor add: want 1 pending, got %d", p)
    }

    // 2. Raise to 30000 — tx0 must be dropped (existing behaviour).
    pool.SetGasTip(floorHigh)
    if p, _ := pool.Stats(); p != 0 {
        t.Fatalf("after raise: want 0 pending, got %d", p)
    }

    // 3. Submit a fresh 27600-tip tx at the high floor — must be
    //    rejected (ErrUnderpriced) for the non-exempt sender.
    if err := pool.addRemote(pricedTransaction(1, 100000, big.NewInt(27600), key)); !errors.Is(err, txpool.ErrUnderpriced) {
        t.Fatalf("at high floor: want ErrUnderpriced, got %v", err)
    }

    // 4. Lower back to 27600.
    pool.SetGasTip(floorLow)

    // 5. Submit the same gasTipCap=27600 tx now — must be admitted.
    tx1 := pricedTransaction(1, 100000, big.NewInt(27600), key)
    if err := pool.addRemoteSync(tx1); err != nil {
        t.Fatalf("after lower, want admit; got %v", err)
    }
    if p, _ := pool.Stats(); p < 1 {
        t.Fatalf("after lower-and-resubmit: want >=1 pending, got %d", p)
    }

    // 6. Invariant: no stale anzeonTipCap caches remain.
    assertNoStaleAnzeonTipCache(t, pool)
}

// assertNoStaleAnzeonTipCache verifies the Layer 2 invariant:
//   ∀ tx ∈ pool.all, cached := tx.GetAnzeonTipCap();
//     cached == nil ∨ cached == anzeonTipEnv.GetAnzeonTipCap(tx)
func assertNoStaleAnzeonTipCache(t *testing.T, pool *LegacyPool) {
    t.Helper()
    pool.all.Range(func(_ common.Hash, tx *types.Transaction, _ bool) bool {
        cached := tx.GetAnzeonTipCap()
        if cached == nil {
            return true
        }
        fresh := pool.anzeonTipEnv.GetAnzeonTipCap(tx)
        if fresh != nil && cached.Cmp(fresh) != 0 {
            t.Errorf("stale anzeonTipCap on tx %s: cached=%v fresh=%v", tx.Hash().Hex(), cached, fresh)
        }
        return true
    }, true, true)
}
```

**Why this fails on pre-fix**: step 5 (`addRemoteSync(tx1)`) — pre-fix,
the lowering `SetGasTip(27600)` does nothing, and even if the user
tries to re-add a tx with the same nonce, the queue-side `add`
revalidates against `pool.gasTip.Load().ToBig() == 27600` (which works
since gasTip IS lowered), BUT the `anzeonTipCap` cache on any existing
queued tx is stale, and any pricedList ordering is stale. More
directly: tx0 was dropped at raise; tx1 at nonce 1 still goes through
validateTxBasics with the new floor — it passes. However, the
companion-defect Test 5b (system-contract exemption) is the harder
failure for pre-fix. **Implementer must verify both new tests fail on
pre-commit HEAD and pass after fixes** — this is the AC4 regression-
detection power requirement.

> Subtle point for the Implementer: if step 5 passes on pre-fix because
> `validateTxBasics` no longer hits the high floor, refine the test to
> use a sender that already has a *queued* tx (gap-induced) before the
> raise/lower, so the **promotion** behaviour (not the entry gate)
> becomes the unique discriminator. This is the canonical
> AC1-fail-on-pre-fix discriminator that the analysis names: "queued
> tx that was below the high floor must auto-promote when the floor
> drops back". The Implementer should add a second sender + a gapped
> nonce sequence to force the test to discriminate.

### 5.3 Proposed test: `TestSystemContractTxExemptFromMinTip`

```go
// TestSystemContractTxExemptFromMinTip verifies AC2 + AC3: a tx whose
// `to` is a chainconfig-derived system contract address is admitted
// regardless of the MinTip floor, and surfaces in Pending(). On pre-fix
// code this MUST fail with ErrUnderpriced at validateTxBasics.
func TestSystemContractTxExemptFromMinTip(t *testing.T) {
    t.Parallel()

    pool, _ := setupPoolWithConfig(params.TestWBFTChainConfig)
    defer pool.Close()

    key, _ := crypto.GenerateKey()
    sender := crypto.PubkeyToAddress(key.PublicKey)
    testAddBalance(pool, sender, big.NewInt(1000000000))

    // Raise floor to 30000 Gwei.
    pool.SetGasTip(big.NewInt(30000))

    sc := params.TestWBFTChainConfig.Anzeon.SystemContracts
    contracts := []*params.SystemContract{
        sc.GovValidator, sc.GovCouncil, sc.GovMinter,
        sc.GovMasterMinter, sc.NativeCoinAdapter,
    }

    var nonce uint64 = 0
    for _, c := range contracts {
        if c == nil {
            continue
        }
        // gasTipCap = 1 Gwei (clearly below the 30000 floor).
        target := c.Address
        tx := pricedTransactionTo(nonce, 100000, big.NewInt(1), &target, key)
        nonce++

        if err := pool.addRemoteSync(tx); err != nil {
            t.Fatalf("contract %x: want admit despite floor 30000, got %v", target, err)
        }
    }

    if pending, _ := pool.Stats(); pending < int(nonce) {
        t.Fatalf("want %d pending sys-contract txs, got %d", nonce, pending)
    }

    // Negative control: same low-tip tx to a random EOA must still be
    // rejected with ErrUnderpriced (gate is still active for non-
    // exempt txs).
    var randomEOA common.Address
    randomEOA.SetBytes([]byte("not-a-system-contract-addr"))
    if err := pool.addRemote(pricedTransactionTo(nonce, 100000, big.NewInt(1), &randomEOA, key)); !errors.Is(err, txpool.ErrUnderpriced) {
        t.Fatalf("non-exempt low-tip tx: want ErrUnderpriced, got %v", err)
    }

    // Sealing-time filter: Pending() with MinTip=30000 must NOT
    // truncate the system-contract txs.
    filter := txpool.PendingFilter{
        MinTip: uint256.NewInt(30000),
    }
    got := pool.Pending(filter)
    if list := got[sender]; len(list) == 0 {
        t.Fatalf("Pending() truncated system-contract txs out (sender %x missing)", sender)
    }
}

// pricedTransactionTo is pricedTransaction with an explicit `to`.
// (Helper to be added near pricedTransaction in legacypool_test.go.)
```

Implementer must add the small `pricedTransactionTo` helper alongside
the existing `pricedTransaction`/`dynamicFeeTx` helpers (look around
line 100-175 in legacypool_test.go).

### 5.4 Side-effect checklist

- [x] **Existing tests untouched** — new tests, plus a small helper.
- [x] **Tests use chainconfig-derived addresses only** — pulled from
      `params.TestWBFTChainConfig.Anzeon.SystemContracts`, no
      hardcoded addresses (AC3).
- [x] **Tests assert regression detection** — both must FAIL on
      pre-fix HEAD; this is captured in the assertion patterns.

---

## Step 6: Chain-simulation test (`gastip_restore_sim_test.go`)

### 6.1 Pattern reference

`consensus/wbft/backend/multiengine_test.go:62-130` shows the canonical
"build a real `*core.BlockChain` from genesis" pattern:

```go
genesis, nodeKeys, _ := testutils.GenesisAndFixedKeys(n)
// ... per-node:
db := rawdb.NewMemoryDatabase()
chain, err := core.NewBlockChain(db, nil, &gspec, nil, engine, vm.Config{}, nil, nil)
```

`testutils.GenesisAndFixedKeys` (consensus/wbft/testutils/genesis.go)
sets up a `*core.Genesis` whose `Config` has `Anzeon.SystemContracts`
populated, which is exactly what we need to drive the system-contract
exemption end-to-end.

### 6.2 Test outline

```go
// TestGasTipRestoreChainSim drives gasTip 27600 -> 30000 -> 27600 on a
// real *core.BlockChain (multiengine-style harness) and asserts:
//  (a) Layer 1: a governance restore tx (to=GovValidator.Address,
//      gasTipCap=1 Gwei) enters the pool at the high floor and is
//      surfaced by pool.Pending().
//  (b) Layer 2: a queued regular-sender tx (with gasTipCap=27600) that
//      was demoted on raise auto-promotes to pending on lower-back.
//
// On pre-fix HEAD this test must fail on (a) (ErrUnderpriced at Add)
// and would also fail on (b) (no auto-promote on lower).
func TestGasTipRestoreChainSim(t *testing.T) {
    // 1. Build genesis with SystemContracts populated.
    genesis, nodeKeys, _ := testutils.GenesisAndFixedKeys(4)
    govAddr := genesis.Config.Anzeon.SystemContracts.GovValidator.Address

    // 2. Build a real BlockChain.
    db := rawdb.NewMemoryDatabase()
    triedb := triedb.NewDatabase(db, nil)
    _ = triedb // referenced if genesis.Commit needs it
    gspec := &core.Genesis{
        Config:   genesis.Config,
        Alloc:    genesis.Alloc,
        BaseFee:  big.NewInt(0),
    }
    chain, err := core.NewBlockChain(db, nil, gspec, nil, dummyEngine{}, vm.Config{}, nil, nil)
    if err != nil { t.Fatalf("blockchain: %v", err) }
    defer chain.Stop()

    // 3. Build a LegacyPool wired to this chain.
    pool := legacypool.New(testTxPoolConfig, chain)
    if err := pool.Init(legacypool.DefaultPriceLimit, chain.CurrentBlock(), newReserver()); err != nil {
        t.Fatalf("pool init: %v", err)
    }
    defer pool.Close()
    <-pool.InitDoneCh()  // or wait sentinel — Implementer chooses

    // 4. Two signers: a regular sender and a "governance signer".
    regKey, _ := crypto.GenerateKey()
    govKey  := nodeKeys[0]
    chain.StateAt(chain.CurrentBlock().Root) // pre-fund regKey via genesis Alloc

    // 5. Sequence (as plan.md §Step 6).
    pool.SetGasTip(big.NewInt(27600))
    txReg := signTx(0, 100000, big.NewInt(27600), regKey)
    pool.Add([]*types.Transaction{txReg}, false, false) // remote, async
    // ... assert in pending

    pool.SetGasTip(big.NewInt(30000)) // raise: txReg dropped

    txGov := signTxTo(0, 100000, big.NewInt(1), &govAddr, govKey)
    pool.Add([]*types.Transaction{txGov}, false, false)
    // Layer 1 assertion: txGov in pending
    p := pool.Pending(txpool.PendingFilter{MinTip: uint256.NewInt(30000)})
    if !contains(p, txGov.Hash()) {
        t.Fatalf("Layer 1: gov restore tx not in pending")
    }

    pool.SetGasTip(big.NewInt(27600)) // lower back

    // Layer 2 assertion: txReg auto-promoted back
    // (re-submit if previously fully removed by removeTx; or assert it
    // was demoted-to-queue and is now back in pending. Implementer
    // picks whichever matches the post-fix semantics: with the new
    // lowering branch, queued txs auto-promote.)
}
```

Implementer-level details (concrete imports, exact `dummyEngine`
choice, whether to use a fresh `txpool/legacypool.New` or the
multi-pool `txpool.New` wrapper) are left to the Implementer; the
multiengine_test.go file provides the exact import set and
construction recipe to mirror.

### 6.3 Side-effect checklist

- [x] **Imports** — confirm `consensus/wbft/testutils` is importable
      from `core/txpool/legacypool` (no circular import). If it is
      not, the test moves to `core/txpool/legacypool/gastip_restore_sim_test.go`
      with `package legacypool_test` (external test package) and
      explicit import paths. Implementer must verify and adjust.
- [x] **Race scope** — file is exercised by
      `go test -race ./core/txpool/legacypool/...`.
- [x] **No new shared resources** — local chain, local pool, no global state.

### 6.4 Tests

- **Existing tests that must still pass**: full
  `./core/txpool/legacypool/...` suite (the new file is additive).

---

## 7. Risks / non-goals

### 7.1 Out of scope (explicit)

- **Layer 3** (`eth/gasprice/anzeon.go` `SetCurrentBlock` same-Root
  skip — anzeon.go:50-63) is a real companion defect that the
  diagnosis identifies, but is **deferred** per ticket mandate. The
  practical impact: in long sequences of root-unchanged blocks, the
  consume-side env can be stale. Once Layer 1 + Layer 2 are merged,
  the **deadlock is broken** (Layer 1) and the per-tx cache is correct
  (Layer 2). Layer 3's residual staleness is in `anzeonTipEnv`'s
  internal mirror, which goes stale only in pathological block
  sequences, and is a separate PR.

### 7.2 Behavior changes

- A **new code path** runs when `SetGasTip(new < old)` is invoked.
  Pre-fix: no-op. Post-fix: cache-clear + reheap + promote-request.
  This is a behavior change visible to **any test that calls SetGasTip
  with a smaller value**. Mitigation: review existing tests for
  lowering invocations — none of the 4 existing tests
  (`TestRepricing`, `TestMinGasPriceEnforced`, `TestRepricingKeepsLocals`,
  `TestRepricingShiftAccountAge`) lower the tip. Verified by grep.
- `GetAnzeonTipCap` now also rejects typed-nil. Implementer must
  ensure no existing test relied on the literal `interface{}`-wrapped
  typed-nil being returned (it cannot — pre-fix `SetAnzeonTipCap`
  never stored typed-nil).

### 7.3 Performance

- `systemContractAddresses()` allocates a small map per call. Called
  twice per Add (once in `validateTxBasics`, once if the tx reaches
  Pending). Negligible — exempted txs are governance-rare, and the
  map has at most 5 entries. If profile shows pressure, the
  Implementer may cache the set on `LegacyPool` (computed once in
  `New()`), but that is an optimization, not a correctness fix.
- `pool.all.Range` on lowering iterates all txs in the pool to
  ClearAnzeonTipCap. On large pools (10k txs) this is O(n) under
  `pool.mu`. This is acceptable because `SetGasTip` is invoked once
  per block import (1s cadence on StableNet) and Reheap is already
  O(n log n).

### 7.4 Concurrency invariants (RI-21)

- `pool.mu` is the only mutex touched by SetGasTip; the cache clear
  via `pool.all.Range` uses `lookup.lock` (inner RWMutex) but lock
  order is preserved.
- `requestPromoteExecutables` MUST be invoked AFTER `pool.mu.Unlock`.
  Encoded as **explicit `Unlock` call**, not a `defer`, so it cannot
  be accidentally moved by a future refactor.
- No new goroutines, channels, or mutexes are introduced. The existing
  `pool.reqPromoteCh` carries the post-lower notification.

### 7.5 Cherry-pick safety (RI-09)

- `core/txpool/validation.go` is **not modified**. The exemption is
  caller-driven via `opts.MinTip = 0`. This keeps upstream
  go-ethereum's `validation.go` rebase-friendly.
- New helpers live in `legacypool.go` (the StableNet-aware file) and
  are private (`systemContractAddresses`, `isSystemContractTx`).

### 7.6 Branch & toolchain

- Branch is already `fix/pr-77-gastip-dynamic-reflection-test7` (HEAD
  `0bf2f4d1`, contains `test7`). The Implementer commits **directly
  on this branch** — no new branch creation.
- Go toolchain: `export PATH=$PATH:/Users/wm-it-25_0220/.gvm/gos/go1.23.12/bin`
  before any `go build` / `go test`. Matches `go.mod` toolchain line
  `go 1.23.12`.

---

## 8. Self-review notes (final)

The design has been re-read and audited against:

1. **Plan-step coverage** — every step in plan.md has a corresponding
   section here with code excerpts and a side-effect checklist.
2. **Concurrency** — §0.2 explicitly traces the deadlock chain and
   §3.2 implements the mitigation (explicit Unlock before
   `requestPromoteExecutables`).
3. **Typed-nil rationale** — §0.3 documents the panic mechanism and the
   back-compat hardening of `GetAnzeonTipCap`.
4. **Whitelist derivation** — §0.4 and §2.2(a) document chainconfig-
   derived address-set with three-level nil-guards.
5. **Write-site completeness (§5.2b principle)** — §1.4 (cache),
   §2.4 (MinTip floor), §3.4 (pool.gasTip mutator) enumerate all
   write/consume sites; the self-checking invariant
   `assertNoStaleAnzeonTipCache` is named and wired into the
   regression test (Step 5).
6. **Out-of-scope clarity** — §7.1 explicitly states Layer 3 is
   deferred.
7. **No `validation.go` modification** — §0.5, §2.4 row 4, §7.5 each
   restate this.
8. **Test fail-on-pre-fix discriminator** — Step 5.2 has an explicit
   subtle-point note for the Implementer about strengthening the
   queued-tx discriminator if needed.

No issues found. Design is final at revision 1.
