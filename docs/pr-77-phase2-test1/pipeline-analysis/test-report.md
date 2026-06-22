# Test Report: LOCAL-20260622_051111
Generated: 2026-06-22T08:15:00Z
Branch: fix/LOCAL-20260622_051111
HEAD: 2e83c3183662050c03cc76abb3aa3e47e1cce300

## Summary
| Stage | Status | Notes |
|-------|--------|-------|
| Unit Test | PASS | 2062 passed, 0 failed, 42 skipped; coverage 81.2% |
| Lint & Format | WARN | golangci-lint not installed; go vet clean; gofmt clean; goimports not installed |
| Security Scan | PASS | go vet clean; gosec not installed; no new security issues |
| ChainBench | SKIPPED | chainbench-net infrastructure binary not installed on this machine |
| **Overall** | **PASS** | All run stages PASS/WARN/SKIPPED |

Note: ChainBench SKIPPED (infrastructure binary absent) counts as graceful degradation per §7.0, not as a stage failure. Correctness is gated by the reproduction oracle (GREEN) and unit/lint/security stages.

---

## Reproduction Oracle (TestReproduce_GasTipPolicyStaleOnIdleBlocks)

- **Result: GREEN** (bug fixed)
- GREEN at HEAD (fix/LOCAL-20260622_051111 @ 2e83c318): PASS
- RED at reproduction commit (d2fdc17f): FAIL (confirmed — bug reproduced as expected)
- Test file diff between reproduction commit and HEAD: empty (oracle not modified)

The red→green arc is proven. The oracle drives the empty-block / same-root scenario (headerN and headerN+1 share Root but carry different GasTip values). Post-fix, GetAnzeonTipCap for a non-validator returns newTip (1000000000) not the stale oldTip (5000000000).

---

## Unit Test

- passed: 2062
- failed: 0
- skipped: 42
- coverage (eth/gasprice): 81.2% of statements
- race detection: none detected (go test -race ./eth/gasprice/... — clean, ld warning only, not a test failure)

### Focused regression tests (all PASS):
- TestReproduce_GasTipPolicyStaleOnIdleBlocks (reproduction oracle) — PASS
- TestSetCurrentBlock_RefreshesHeaderOnSameRoot (§5.2b consistency invariant) — PASS
- TestSetCurrentBlock_SkipsStateReadOnSameRoot (preserved optimisation) — PASS
- TestSetCurrentBlock_ReadsStateOnRootChange (inverse path) — PASS

### Derived state gate (§4.6):
Design §5.2b declares env.currentBlock as derived state (copy of chain head maintained by SetCurrentBlock). Required tests:
- Consistency-invariant test: TestSetCurrentBlock_RefreshesHeaderOnSameRoot — PRESENT and PASS
- Adversarial-path test: TestReproduce_GasTipPolicyStaleOnIdleBlocks exercises the empty-block / same-root path (the exact eviction/bypass scenario) — PRESENT and PASS
Gate: PASS

### Build note:
`go build ./...` emits a compile error in golang.org/x/tools/internal/tokeninternal (pre-existing dependency issue unrelated to this fix). The changed packages (eth/gasprice, cmd/gstable) build cleanly; the failure is in a dev-tooling dependency, not a production package. The implementer's artifact at build/bin/gstable was built successfully and matches HEAD commit.

---

## Lint & Format

- golangci-lint: not installed — fell back to go vet (WARNING: golangci-lint absent)
- go vet ./eth/gasprice/...: CLEAN (0 issues)
- gofmt -l on changed files: CLEAN (no format violations)
- goimports -l: not installed (WARNING)
- Format violations: 0

Status: WARN (tooling not fully installed; go vet + gofmt checks clean)

---

## Security Scan

- go vet ./eth/gasprice/...: CLEAN
- gosec: not installed (skipped)
- Hard-coded secrets scan: NONE found
- Unsafe usage scan: NONE found
- Silently-ignored errors:
  - eth/gasprice/anzeon.go:67 — `env.currentState, _ = env.stateAt(header.Root)`
    Severity: LOW. Pre-existing pattern (identical idiom at line 117 in GetAnzeonTipCap). Not introduced by the fix; the stateAt error is intentionally suppressed — if state is unavailable, currentState is set to nil and the caller handles nil gracefully. No new risk.
- Newly-shared fields without mutex protection: NONE. CKG concurrency_impact confirms SetCurrentBlock writes remain under pool.mu; no new sharing introduced. Invariant #11 preserved.

Status: PASS (no critical/high findings)

---

## ChainBench

Status: SKIPPED — chainbench-net infrastructure binary not found on this machine.

The MCP tools (chainbench_init, chainbench_start, etc.) are registered and available, but the underlying `chainbench-net` binary that the plugin delegates to is not installed. Per §7.0 graceful degradation: SKIPPED does not count as a stage failure.

To enable e2e: install the chainbench-net binary and re-run evaluation.

The fix is localized to `eth/gasprice/anzeon.go` (SetCurrentBlock pointer logic only); it does not affect network protocol, block propagation, consensus, or transaction pool acceptance paths in ways that would cause chainbench's basic/tx-send to behave differently for normal EIP-1559 txs. The governance-gasTip→idle-block scenario is covered by the unit-level reproduction oracle.

---

## Overall Verdict: PASS

All run stages are PASS or WARN. ChainBench is SKIPPED (infrastructure absent, graceful degradation). Reproduction oracle is GREEN with confirmed red→green arc. Derived state gate passed. No security issues introduced. The bug (stale AnzeonTipEnv.currentBlock on empty blocks with unchanged state Root) is fixed.
