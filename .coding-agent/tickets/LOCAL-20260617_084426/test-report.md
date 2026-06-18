# Test Report — LOCAL-20260617_084426

Branch: `fix/pr-77-gastip-dynamic-reflection-test7` (base `0bf2f4d1b`)
Toolchain: go1.25.11 (GOTOOLCHAIN=local; module `go 1.23.0`)

## Summary

| Stage | Result | Notes |
|-------|--------|-------|
| Build | PASS | `go build ./core/... ./miner/... ./eth/...` clean |
| Unit tests (-race) | PASS | core/types, core/txpool/legacypool, core/txpool/blobpool all green |
| Lint / format | PASS | `gofmt -l` empty; `go vet` clean on changed packages |
| Security scan | SKIP | no scanner configured in this environment |
| ChainBench integration | SKIP | no chainbench environment available locally |

## Regression power (AC4 — must fail on pre-fix code)

Verified by restoring pre-fix production source (`git checkout 0bf2f4d1b -- transaction.go legacypool.go blobpool.go`) while keeping the new tests:

| Test | pre-fix | post-fix |
|------|---------|----------|
| `TestSystemContractTxExemptFromMinTip` | FAIL (`ErrUnderpriced`) | PASS |
| `TestRepricingDynamicReflection` | FAIL (stale anzeonTipCap not cleared) | PASS |
| `TestGasTipRestoreChainSimulation` | FAIL (AC2 proposal rejected) | PASS |

## Acceptance criteria

- AC1 (non-conforming tx not left pending after gasTip change) — covered by sim block 2 + `TestRepricingDynamicReflection`.
- AC2 (system-contract restore proposal admitted regardless of high floor) — `TestSystemContractTxExemptFromMinTip` + sim block 3.
- AC3 (exemption is chainconfig-derived, no hardcoding) — `systemContractAddresses()` reads `chainconfig.Anzeon.SystemContracts`; nil-guarded.
- AC4 (regression tests fail pre-fix) — table above.
- AC5 (multiengine-pattern chain simulation) — `gastip_restore_sim_test.go` (txpool-level reorg simulation; rationale documented in file header).
- AC6 (`go test -race` passes) — all affected packages green.
- AC7 (branch contains `test7`, based on current HEAD) — satisfied.

## Test commands

```
export GOTOOLCHAIN=local
go build ./core/... ./miner/... ./eth/...
gofmt -l <changed files>          # empty
go vet ./core/txpool/legacypool/ ./core/txpool/blobpool/ ./core/types/
go test -race ./core/types/ ./core/txpool/legacypool/ ./core/txpool/blobpool/ -count=1
```
