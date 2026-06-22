# Design Changelog — LOCAL-20260622_051111

- v1 (2026-06-22T06:51:44Z): initial draft. Two-step plan: (1) fix
  `eth/gasprice/anzeon.go:50-63` `SetCurrentBlock` to refresh the header
  pointer on every non-nil call and guard only the `stateAt` re-read by
  Root equality; (2) add focused regression tests in
  `eth/gasprice/anzeon_test.go` (`TestSetCurrentBlock_RefreshesHeaderOnSameRoot`,
  `TestSetCurrentBlock_SkipsStateReadOnSameRoot`,
  `TestSetCurrentBlock_ReadsStateOnRootChange`) to pin the invariant.
  §5.2b write-site table is exhaustive; every production caller of
  `SetCurrentBlock` is enumerated and the row reasoning recorded. The
  per-tx cache `tx.anzeonTipCap` is intentionally left unchanged
  (decision recorded in §5.2b "Decision on copy (c)"). Self-review
  found no issues; v1 is final.
