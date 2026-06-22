# Diagnosis — c-09 RemotesBelowTip eviction stale on tip-floor raise

Retrieval backend: cks ok (ckg + ckv/bge-m3), indexed_head = 0bf2f4d1bfeb6605006d556957ef8c045d8f8ed8 (= buggy parent). Pack used: get_for_task → cited bodies inline (no re-Read of pack spans).

## 1. Root cause

`(*lookup).RemotesBelowTip(threshold)` in `core/txpool/legacypool/legacypool.go:2058-2067` decides which pooled transactions to drop when the legacy pool's tip floor RISES. It compares **`tx.GasTipCapIntCmp(threshold) < 0`** — i.e. the transaction's **raw `maxPriorityFeePerGas`** — against the new floor. For an unauthorized (non-validator) sender on StableNet (Anzeon), the value that actually matters for mineability is **NOT** the raw tx tip but the **block header `GasTip` floor** that `EffectiveGasTip` clamps unauthorized accounts to (see `eth/gasprice/anzeon.go:90-120` `AnzeonTipEnv.GetAnzeonTipCap` → "For unauthorized accounts, use block header's gas tip"). So a tx with raw tip 50 000 Gwei but Anzeon-effective tip 1 000 Gwei (= old block GasTip) "passes" `50000 < 30000` = false in `RemotesBelowTip`, is kept in the pool, yet is permanently unminable because the network now requires 30 000 Gwei effective tip from unauthorized senders.

In root-cause-lifecycle terms the broken edge is `consume`: the eviction reader of "tip-below-threshold?" reads the WRONG source — raw `tx.gasTipCap()` instead of the Anzeon effective tip the network actually enforces at mining. The lookup function never sees the AnzeonTipEnv (the pool keeps one at `pool.anzeonTipEnv` and already feeds it to `pricedList` for ordering, but it is not threaded into the "drop-below-tip-floor" sweep).

One-liner: **`RemotesBelowTip` filters by raw `tx.GasTipCap` instead of the Anzeon-effective tip; for unauthorized senders the effective tip is the block's `GasTip` floor (currently 1 000 Gwei) — so a raw 50 000 Gwei tx survives a raise to 30 000 Gwei despite being unminable.**

## 2. Evidence

- Pack body — `LegacyPool.SetGasTip` (core/txpool/legacypool/legacypool.go:472-496): on tip raise calls `pool.all.RemotesBelowTip(tip)` and `removeTx(...)` for each. The log line in the e2e evidence — `INFO ... Legacy pool tip threshold updated tip=30,000,000,000,000` — is emitted from this exact function (`log.Info("Legacy pool tip threshold updated", ...)`), confirming the path runs and writes `pool.gasTip` to 30 000 Gwei but selects an empty `drop` set.
- Broken edge — `lookup.RemotesBelowTip` (core/txpool/legacypool/legacypool.go:2058-2067):
  ```
  if tx.GasTipCapIntCmp(threshold) < 0 { found = append(found, tx) }
  ```
  `GasTipCapIntCmp` (core/types/transaction.go:380-383) is the **raw** inner gasTipCap — NOT EffectiveGasTip/Anzeon. There is no second eviction path with the same observable: cks `find_callers RemotesBelowTip` returns ONLY `LegacyPool.SetGasTip` (+ tests); cks search for "GasTipCapIntCmp threshold" in `core/txpool/**` returns ONLY this site. Effect-completeness holds: this is the unique site that performs the "drop because below current tip floor" semantics.
- Anzeon API already present, just not used here:
  - `AnzeonTipEnv.GetAnzeonTipCap(tx)` (eth/gasprice/anzeon.go:90-120): "For unauthorized accounts, use block header's gas tip" → returns `env.currentBlock.GasTip()`; for authorized accounts returns `tx.GasTipCap()`.
  - `Transaction.EffectiveGasTip(anzeonTipEnv)` (core/types/transaction.go:388-403): clamps to `min(GetAnzeonTipCap(tx), GasFeeCap - BaseFee)`.
  - `Transaction.EffectiveGasTipIntCmp(other, anzeonTipEnv)` (core/types/transaction.go:437-442): the correct counterpart of `GasTipCapIntCmp` for this comparison.
- Pool already owns the env: `pool.anzeonTipEnv` initialized at `core/txpool/legacypool/legacypool.go:267` and `:302` (`gasprice.NewAnzeonTipEnv(chain.Config(), chain.StateAt)`); `priceHeap.cmp` at `core/txpool/legacypool/list.go:498-516` already uses `EffectiveGasTipCmp(.., h.anzeonTipEnv)` for ordering — establishing the precedent that the legacy pool MUST use Anzeon-effective tips when comparing across heterogeneous sender authority. `RemotesBelowTip` is the lone non-Anzeon-aware filter.
- cks edges:
  - `LegacyPool.SetGasTip` --calls--> `lookup.RemotesBelowTip` (only caller in production code; `find_callers` distance=1).
  - `LegacyPool.SetGasTip` is called via `subpool.SetGasTip` → `txpool.SetGasTip` → consensus/governance update path (e.g. `eth/api_miner.go`, `eth/backend.go`, `miner/worker.go`). Triggered when `gov_validator` `gasTip` (`systemcontracts/solidity/v1/GovValidator.sol:47 — uint256 public gasTip; // 0x39`) changes; the DEBUG line `Updated gasTip from GovValidator contract newTip=...` confirms the governance-driven entry.
- Falsification of competing hypotheses:
  - **"admission-time validation rejected it"** — refuted by the evidence: the tx was submitted BEFORE the raise, when the floor was 1 000 Gwei. Raw 50 000 ≫ 1 000 ⇒ admitted cleanly. Symptom is post-raise persistence, not admission failure.
  - **"`pool.priced.Discard` underpriced eviction should catch it"** — `Discard` runs on pool overflow, not on threshold raise; nothing in the e2e shows pool saturation. Even if it ran, `priceHeap.cmp` orders by `EffectiveGasTipCmp` and would correctly rank the unauthorized tx LOW — that is supporting evidence the Anzeon-effective tip is the right comparator. The threshold-raise path simply skips that comparator.
  - **"`Transaction.anzeonTipCap` cache is stale"** — even if that cached value were stale, `RemotesBelowTip` never reads it; it bypasses the cache entirely and reads `tx.inner.gasTipCap()` (raw). The cache lifecycle is therefore not on the broken edge.
- Lifecycle of the value (`effective minTip` for an unauthorized account):
  - producer: governance `gov_validator.gasTip` → consensus update → header `GasTip` field; consumed by `AnzeonTipEnv.SetCurrentBlock` (eth/gasprice/anzeon.go:50-63) → `currentBlock`.
  - copies/derived: `pool.gasTip` (legacypool floor, used here as `threshold`); `currentBlock.GasTip()` returned by `GetAnzeonTipCap` for unauthorized senders; cached `tx.anzeonTipCap` (set during validation/ordering).
  - consumer (broken): `RemotesBelowTip` reads `tx.gasTipCap()` raw, ignoring all of the above.
- e2e log timing matches: `21:42:36 tip=27 600 Gwei → 21:43:08 tip=1 000 Gwei → 21:43:15 tip=30 000 Gwei`. The lowering at 21:43:08 admits the 50 000 Gwei tx easily, then the raise at 21:43:15 runs `SetGasTip` (log line proves it) but `RemotesBelowTip(30 000)` returns empty because `50 000 < 30 000` is false.

## 3. Affected sites / fix point

**Single fix point** — make the threshold comparison Anzeon-effective:

- `core/txpool/legacypool/legacypool.go:2058-2067` — `func (t *lookup) RemotesBelowTip(threshold *big.Int) types.Transactions`. The comparator on **line 2061** (`if tx.GasTipCapIntCmp(threshold) < 0`) must be replaced with the Anzeon-effective tip comparator, e.g. `tx.EffectiveGasTipIntCmp(threshold, anzeonTipEnv) < 0`. Since the function lives on `lookup` (which has no env), the cleanest plumbing is either (a) thread `anzeonTipEnv` into `lookup` (the same way `pricedList`/`priceHeap` already do — `core/txpool/legacypool/list.go:481, 559-565`), or (b) change the signature to take it as a parameter and pass `pool.anzeonTipEnv` from the **only** caller `LegacyPool.SetGasTip` at `core/txpool/legacypool/legacypool.go:489`.

Correct value to compare against the new `threshold`: for each tx, `EffectiveGasTip(anzeonTipEnv)` — i.e. `min( GetAnzeonTipCap(tx), GasFeeCap − BaseFee )`. For an unauthorized sender this collapses to (a clamp of) `currentBlock.GasTip()`; for authorized senders it stays at the raw `tx.GasTipCap()` (so existing tests like `TestRepricing` — which use no Anzeon env — keep passing because `EffectiveGasTipIntCmp` falls back to `GasTipCapIntCmp` when `anzeonTipEnv == nil || GetBaseFee() == nil`, see core/types/transaction.go:437-442).

No other call site needs to change. `SetGasTip` at `:489` is the single production caller of `RemotesBelowTip`; the test callers at `legacypool_test.go:1458/1578/1627/1751` exercise the non-Anzeon path and will be unaffected by the fallback semantics of `EffectiveGasTipIntCmp`.

Touched downstream (read-only, no change): `LegacyPool.SetGasTip` at `core/txpool/legacypool/legacypool.go:472-496` — it already drives the loop and emits the `Legacy pool tip threshold updated` log; the planner only needs to provide `pool.anzeonTipEnv` at the call site (or via a `lookup.anzeonTipEnv` field analogous to `priceHeap.anzeonTipEnv`).

## 4. Confidence

**High.**

Why: (1) the broken edge is reproduced precisely by the e2e log line which is emitted from the exact function that calls `RemotesBelowTip` (`Legacy pool tip threshold updated tip=30,000,000,000,000`); (2) `find_callers RemotesBelowTip` proves it is the unique production caller — no parallel path can mask or duplicate the bug; (3) the raw vs. effective tip distinction is already encoded in the codebase (`AnzeonTipEnv`, `EffectiveGasTip*`, `priceHeap.cmp`), so the fix maps to existing APIs rather than new design; (4) admission-time and `Discard`-time hypotheses are statically refuted using the e2e timing and the in-pack `SetGasTip` body; (5) the StableNet invariant context is consistent — equal-power validators on PoA where governance updates a global tip floor and unauthorized senders are clamped to it.

Would raise confidence further (not needed for diagnosis):
- A runtime probe via `investigative-probe` running the e2e scenario and `t.Logf` of `tx.GasTipCap()` vs `tx.EffectiveGasTip(pool.anzeonTipEnv)` immediately inside `RemotesBelowTip` on the unauthorized tx: prediction is `GasTipCap=50000, EffectiveGasTip=1000 (or 30000 cap after raise) < threshold` ⇒ should be evicted. The static evidence already determines this; the probe would only convert "high" to "very high".

## 5. Suggested direction (planner hand-off, not a plan)

Thread `pool.anzeonTipEnv` (already constructed at `legacypool.go:302`) into the eviction filter, mirroring the precedent set by `pricedList`/`priceHeap` (`list.go:481, 559-565`). Concretely: give `lookup` an `anzeonTipEnv types.AnzeonGasTipEnv` field assigned at `newLookup(...)` from `pool.anzeonTipEnv`, and rewrite `RemotesBelowTip`'s predicate to `tx.EffectiveGasTipIntCmp(threshold, t.anzeonTipEnv) < 0`. The nil-safe fallback in `EffectiveGasTipIntCmp` (transaction.go:437-442) preserves existing non-Anzeon test behavior. Add a reproduction-style unit test in `core/txpool/legacypool` that wires a `testAnzeonTipEnv` (already exists in `list_test.go:115-143`) where the unauthorized sender's effective tip is the block GasTip, raises `SetGasTip` above that effective tip but below the raw tx tip, and asserts `pool.all.Get(hash) == nil`. The byzantine-fairness invariant (equal-power, governance-controlled fee floor) is preserved — in fact the fix is required to honor it, since today an unauthorized account can perpetually pin pool/account-nonce slots with an unminable tx, which is a fairness violation against other users.
