# Design Changelog — LOCAL-20260617_084426

## v1 (initial, 2026-06-17T09:25:29Z)

Initial design at revision 1. Scope: Layer 1 (system-contract MinTip
exemption in legacypool entry-gate + Pending filter) + Layer 2
(`Transaction.ClearAnzeonTipCap` typed-nil invalidator,
`LegacyPool.SetGasTip` lowering reaction with post-unlock
`requestPromoteExecutables`, `BlobPool.SetGasTip` cache-clear mirror).
Layer 3 (`eth/gasprice/anzeon.go` same-Root skip) explicitly deferred
per ticket mandate.

Key decisions captured:
- `validation.go` is NOT modified — exemption signalled by caller via
  `opts.MinTip = 0` (mirrors existing `local` exemption); preserves
  cherry-pick safety (RI-09).
- `pool.mu.Unlock()` is an explicit call (not `defer`) in the new
  SetGasTip body so `requestPromoteExecutables` can run after unlock,
  avoiding deadlock with `scheduleReorgLoop → runReorg → pool.mu`
  (RI-21, mirrors `addRemotes` line 1112-1126).
- Cache invalidation uses typed-nil `atomic.Value.Store((*big.Int)(nil))`;
  `GetAnzeonTipCap` is hardened to treat typed-nil as "unset" for
  back-compat.
- System-contract address set is built fresh from
  `pool.chainconfig.Anzeon.SystemContracts` with three-level nil-guards
  (chainconfig / Anzeon / SystemContracts). Test fixtures without
  Anzeon (e.g. `params.TestChainConfig`) yield an empty set →
  exemption inactive → pre-fix behaviour preserved.

Self-review pass: no issues found; design is final at revision 1.
