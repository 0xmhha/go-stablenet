# Analysis — LOCAL-20260617_084426

> **Source of truth.** The root-cause analysis for this bug has already been
> produced and lives at
> `.coding-agent/diagnoses/DIAG-20260617_074336/diagnosis.md` (see PR-77 pass
> criteria in `docs/pr-77.md`). This file refines the diagnosis against live
> code via cks (ckg/ckv head `0bf2f4d1bfeb6605006d556957ef8c045d8f8ed8`,
> identical to the workspace HEAD) and scopes it to **Layer 1 + Layer 2** as
> mandated by the ticket. Layer 3 (`eth/gasprice/anzeon.go` same-Root skip) is
> explicitly out of scope and is not planned here.

## Retrieval backend
- cks.ops.health: `ok`, `serviceable=true`
  - ckg reachable, schema 1.15, indexed_head 0bf2f4d1
  - ckv reachable, model bge-m3 via Ollama, indexed_head 0bf2f4d1
- cks.ops.freshness: `fresh=true` (indexed_head == current_head)
- cks `source_root` is `.../pr-77` but worktrees `pr-77` and `pr-77-test7` are
  byte-identical for every cited file at this HEAD (verified by `diff -q` on
  the lead path `core/txpool/validation.go`). The index therefore applies to
  this workspace unchanged.

## Ticket
- Type: bugfix
- Summary: governance(GovValidator) gasTip 인상 후 복원 제안 tx 가 pending pool
  에 정체되어 블록에 포함되지 않는 순환 데드락. Layer 1(governance 면제) +
  Layer 2(SetGasTip 동적 반영) 범위로 수정.
- Scope (declared): `core/txpool/validation.go`,
  `core/txpool/legacypool/legacypool.go`, `core/txpool/blobpool/blobpool.go`,
  `core/types/transaction.go`

## Domain & Complexity
- Primary domain: `txpool` (confidence: high)
- Domains touched: `core/txpool`, `core/types`, `params` (config 참조)
- Complexity: **complex** — `core/txpool` 는 동시성 민감 모듈이고
  (`pool.mu`, `scheduleReorgLoop` goroutine, `pool.all`/`pending`/`queue`
  맵 mutate 규율 RI-21 적용), per-tx 캐시(`anzeonTipCap`)가 풀 외부에서 일생을
  넘게 살아남는 derived state 이며, governance 시스템 컨트랙트 면제는
  byzantine-fairness 인접 결정이라 화이트리스트가 chainconfig-derived 로
  엄격히 제한되어야 한다.

## Root cause (carried verbatim from diagnosis.md §1, refined)

값 = **유효 minimum tip (effective floor)**. 복사본 라이프사이클:

| # | 위치 | 역할 | invalidator |
|---|------|------|-------------|
| 1 | `GovValidator` contract slot `gasTip` (`systemcontracts/solidity/v1/GovValidator.sol:186-190`, event line 175) | source of truth | governance proposal (`_setGasTip`) |
| 2 | `worker.tip` (`miner/worker.go:377-389`) | mining side mirror | `setGasTipUnsafe` (방향 무관 store) |
| 3 | `pool.gasTip` atomic Pointer (`core/txpool/legacypool/legacypool.go:237`) | pool entry/Pending gate | `LegacyPool.SetGasTip` (`legacypool.go:472-496`) |
| 4 | `Transaction.anzeonTipCap` per-tx cache (`core/types/transaction.go:77,412-426`) | reheap 가속 캐시 | **없음 (invalidator 0)** ← 동반 결함 |
| 5 | `AnzeonTipEnv.currentBlock` / `currentState` (`eth/gasprice/anzeon.go:50-63,90-120`) | consume side env | `SetCurrentBlock` (same-Root skip, **Layer 3 — 본 스코프 제외**) |

**깨진 lifecycle edge: produce -> consume at Add-time entry gate**

`core/txpool/validation.go:118-131` (`ValidateTransaction`):

```go
if tx.GasTipCapIntCmp(opts.MinTip) < 0 {
    return fmt.Errorf("%w: gas tip cap %v, minimum needed %v", ErrUnderpriced, tx.GasTipCap(), opts.MinTip)
}
if opts.Config.IsLondon(head.Number) && opts.Config.AnzeonEnabled() {
    minBaseFee := opts.Config.MinBaseFee()
    minFee := new(big.Int).Add(minBaseFee, opts.MinTip)
    if tx.GasFeeCapIntCmp(minFee) < 0 {
        return fmt.Errorf("%w: gas fee cap %v, minimum needed %v", ErrUnderpriced, tx.GasFeeCap(), minFee)
    }
}
```

`opts.MinTip = pool.gasTip.Load().ToBig()` (`legacypool.go:660`)이고 `local`
인 경우만 `opts.MinTip = new(big.Int)` 로 0 면제 (`legacypool.go:662-664`).

따라서 "27600 으로 복원" 제안 tx 가 `gasTipCap = 27600` 으로 들어오면 →
`27600 < 30000` → **Add 단계에서 ErrUnderpriced 로 거절** → pending pool 진입
실패. 그리고 governance floor 를 27600 으로 내리려면 바로 이 제안 tx 가 블록에
포함되어야 하는 → **순환 데드락 (circular deadlock)**.

**경쟁 가설 반증 (비대칭: 올림 정상 / 내림 정체):**

- (A) `Transaction.anzeonTipCap` stale: 본 사건 tx 는 풀에 들어가지조차 못해
  캐시가 채워지는 단계 자체에 도달 못 함. 비대칭을 설명하지 못한다. 단, 동적
  반영의 **동반 결함** 이므로 Layer 2 에서 함께 처리.
- (B) `AnzeonTipEnv.currentBlock` same-Root skip: Pending(consume) 측 결함. Add
  거절 경로는 env 와 무관. 비대칭 미설명. **본 스코프(Layer 3) 제외.**
- (C) `SetGasTip` lowering 시 drop 없음: 사실이지만 *2차 증상*. 이미 풀에 들어가
  살아남은 tx 의 거동 정책이고, 본 사건 tx 는 진입조차 못한다. 단, 동적 반영의
  **동반 결함** 이므로 Layer 2 에서 함께 처리.

진단의 root cause 사슬에서 깨진 edge 는 **Add-time 의 정적 minimum-tip 게이트**
이며 이것이 Layer 1 의 수정 대상이다. 이후 동적-반영 인프라 결함(4 의 invalidator
0 + 3 의 단방향 drop)이 Layer 2 다.

## Related Code (verified against live code via cks/Read)

| File | Symbol | Lines | Role |
|------|--------|-------|------|
| `core/txpool/validation.go` | `ValidateTransaction` | 55-174 | 정적 MinTip + (Anzeon) MinBaseFee+MinTip 게이트 — Layer 1 변경 |
| `core/txpool/legacypool/legacypool.go` | `LegacyPool.validateTxBasics` | 650-669 | `opts.MinTip` 결정 (`local` 면제 패턴) — Layer 1 변경 |
| `core/txpool/legacypool/legacypool.go` | `LegacyPool.SetGasTip` | 472-496 | 단방향 drop 만, lowering 시 무동작 — Layer 2 변경 |
| `core/txpool/legacypool/legacypool.go` | `LegacyPool.Pending` | 569-620 | MinTip 필터(소비 측) — Layer 1 (locals 식 면제 mirror) |
| `core/txpool/legacypool/legacypool.go` | `LegacyPool` 구조체 | 233-268 | `gasTip`, `pending/queue/all`, `anzeonTipEnv` 필드 |
| `core/txpool/blobpool/blobpool.go` | `BlobPool.SetGasTip` | 1018-1085 | 단방향 drop — Layer 2 mirror |
| `core/txpool/blobpool/blobpool.go` | `BlobPool.validateTx` | 1122-1208 | `MinTip = p.gasTip.ToBig()` 게이트 — Layer 1 외 (본 스코프는 적용 안 함, 아래 §결정 참조) |
| `core/types/transaction.go` | `Transaction.anzeonTipCap` | 77 | per-tx 캐시 (invalidator 없음) |
| `core/types/transaction.go` | `Transaction.SetAnzeonTipCap` / `GetAnzeonTipCap` | 412-426 | 캐시 setter/getter; `ClearAnzeonTipCap` 부재 — Layer 2 신규 추가 |
| `params/config_wbft.go` | `SystemContracts` 구조체 | 146-152 | `GovValidator`, `NativeCoinAdapter`, `GovMinter`, `GovMasterMinter`, `GovCouncil` — chainconfig 파생 화이트리스트 source |
| `params/config_wbft.go` | `AnzeonConfig` | 55-59 | `ChainConfig.Anzeon`(line 831), `c.Anzeon.SystemContracts` 경로 |

## Structural Context (cks)

- `find_callers(legacypool.LegacyPool.SetGasTip)` — 호출처는 `setGasTipUnsafe`
  (miner/worker) 와 테스트 4 개. RPC/p2p 경유 직접 호출 없음.
- `find_callers(types.Transaction.SetAnzeonTipCap)` — **단일 호출처**
  `core/txpool/validation.go:241-343` (`ValidateTransactionWithState`)
  + 테스트. 새로 추가할 `ClearAnzeonTipCap` 의 호출처는 `SetGasTip` 1+1
  (legacy/blob) + 테스트.
- `impact_analysis(txpool.ValidateTransaction)`: legacypool `Add`(line 1080),
  legacypool `validateTxBasics`(650), blobpool `validateTx`(1122),
  blobpool `Get/Add`-side(1273). → Layer 1 의 MinTip-bypass 시그니처는 이 네
  콜사이트 모두에 적용 가능해야 한다. 본 스코프는 (a) legacypool entry/Pending,
  (b) blobpool entry 까지를 다룬다.
- `concurrency_impact(LegacyPool.SetGasTip)` (depth 3): `pool.mu` (Mutex)
  단일 모듈. SetGasTip 은 항상 `pool.mu.Lock()` 안에서 동작하므로 Layer 2 에서
  추가하는 `requestPromoteExecutables` 호출은 lock 의 안과 밖 경계를 정확히 다뤄야
  한다 (§Design 의 race 분석 참조).

## Write-site / consumer table (Layer 1 — exemption gate)

| Mutation/consume site | 현재 floor 적용 | 시스템-컨트랙트 면제 액션 |
|------------------------|----------------|---------------------------|
| `legacypool.validateTxBasics` (line 650-669) | `opts.MinTip = pool.gasTip.Load().ToBig()`; `local` 이면 0 | `tx.To()` 가 시스템 컨트랙트 주소면 `opts.MinTip = new(big.Int)` 로 강제(이미 `local` 면제와 동일 메커니즘) |
| `legacypool.Pending` (line 590-601) | `txs[i].EffectiveGasTipIntCmp(minTipBig, env) < 0` 시 truncation; `locals.contains(addr)` 면 skip | 시스템-컨트랙트 면제: `txs[i].To()` 가 시스템 컨트랙트면 그 tx 는 truncation 에 걸리지 않도록 skip |
| `blobpool.validateTx` (line 1122-1131) | `baseOpts.MinTip = p.gasTip.ToBig()` | (blob tx 가 시스템 컨트랙트로 향하지 않는다 — `to` 는 보통 EOA 또는 일반 컨트랙트) — 본 스코프는 Layer 1 면제를 **legacypool 만** 적용한다. blobpool 은 Layer 2(SetGasTip 동적 반영)만 적용. (decision rationale, §Design) |
| `ValidateTransaction` (validation.go:118-131) | static gate | `validateTxBasics` 에서 `opts.MinTip=0` 으로 통제하면 자연히 우회됨. validation.go 자체에는 signal(`opts.MinTip`) 을 통해 면제하고, 함수 내부에 추가 분기를 두지 않아 cherry-pick 위험을 최소화(RI-09). |

> **Decision**: 시스템-컨트랙트 면제는 호출자(`legacypool`)가 `opts.MinTip = 0`
> 으로 보내고, `validation.go` 는 **인터페이스를 유지** 한다. 이는 (a) 기존
> `local` 면제와 동일한 메커니즘, (b) cherry-pick 안전, (c) blobpool 에서
> 의도적으로 적용하지 않을 수 있는 자유도, 세 가지를 동시에 만족시킨다.

## Write-site / consumer table (Layer 2 — dynamic reflection)

값 4(`Transaction.anzeonTipCap`)의 mutation/consume sites 와 3(`pool.gasTip`)의
mutator 1 곳(`SetGasTip`)을 도표화. **새 helper `Transaction.ClearAnzeonTipCap()`**
를 추가하고 다음에서 호출한다.

| Edge | 현재 동작 | Layer 2 수정 |
|------|----------|--------------|
| `Transaction.anzeonTipCap` set (validation.go:337-340) | `tx.GetAnzeonTipCap() == nil` 일 때만 store. 변경 없음. | (변경 없음) |
| `Transaction.anzeonTipCap` clear | 메서드 부재 — Layer 2 신규 `ClearAnzeonTipCap()` 추가 (`atomic.Value` reset). | 신규 추가 |
| `LegacyPool.SetGasTip` raising (`newTip>old`) | `RemotesBelowTip(tip)` drop + `priced.Removed(...)` | (유지) + drop 된 tx 에서는 캐시도 함께 의미가 없어지지만 어차피 풀에서 제거되므로 별도 clear 불필요 |
| `LegacyPool.SetGasTip` lowering (`newTip<old`) | **무동작** | **신규**: `pool.all.Range` 로 모든 tx 에 `ClearAnzeonTipCap()` → `pool.priced.Reheap()` → `accountSet` 으로 dirtyAccounts 모아서 `requestPromoteExecutables` 호출 → queue 에 묶여있던 conformant tx 가 pending 으로 promote 됨. lock 경계: `pool.mu.Lock` 안에서 캐시 clear + reheap, lock unlock 후 `requestPromoteExecutables` 호출 (RI-21 — reorg 루프 쪽에 enqueue) |
| `LegacyPool.SetGasTip` equal (no change) | 조기 return | (유지) |
| `BlobPool.SetGasTip` raising | drop 루프 + Storage GC | (유지) |
| `BlobPool.SetGasTip` lowering | **무동작** | **신규**: blob tx 도 동일하게 `ClearAnzeonTipCap()` 적용 — `p.lookup` 또는 `p.index[addr]` 순회. blobpool 은 lazy reheap 없이 다음 Pending/eviction 사이클에 자연 반영 |

### Self-checking invariant (RI-13)

Layer 2 에서 lowering 후의 풀 상태에 대한 invariant:

```
∀ tx ∈ pool.all,
  cached := tx.GetAnzeonTipCap()
  cached == nil   ∨   cached == anzeonTipEnv.GetAnzeonTipCap(tx)
```

즉 lowering 직후에는 모든 cached 가 nil 이거나 (다음 호출까지 lazy fill), 동일
env 에서 다시 계산한 값과 일치해야 한다. 이 invariant 는 Layer 2 회귀 테스트의
helper assertion `assertNoStaleAnzeonTipCache(t, pool)` 으로 검증한다 (design v1
§Step 4).

## Impact Analysis (top symbols)

- `legacypool.LegacyPool.SetGasTip` — risk: **medium-high** (callers: miner via
  `setGasTipUnsafe`, 4 테스트). lowering 분기 추가는 `pool.mu` lock 안에서 동작
  하므로 lock-acquire 순서/재진입 위반이 없음을 확인. `requestPromoteExecutables`
  는 lock unlock 후 호출하도록 한다 (`Add` 와 동일 패턴, line 1113-1126).
- `txpool.ValidateTransaction` — risk: **low** (signature 미변경; semantic only
  `opts.MinTip == 0` 일 때 자연히 우회). blobpool 호출도 그대로 호환.
- `types.Transaction.SetAnzeonTipCap` — risk: **low** (단일 호출처). 신규
  `ClearAnzeonTipCap` 는 `atomic.Value.Store(nil)` 가 panic 하므로 typed-nil
  접근(`Store((*big.Int)(nil))`)이 안전 — design 에서 다룸.
- `legacypool.Pending` — risk: **medium** (모든 sealing 경로가 의존). 시스템-
  컨트랙트 truncation skip 추가 시 정렬 순서 가정 위반이 없는지 확인 필요 →
  `list.Flatten()` 은 nonce 정렬이므로 단순 skip 이 아니라 *해당 tx 까지는 살아남되
  그 다음 nonce 들이 hole 로 처리되지 않도록* 정책을 명시 (design Step 1.b).

## Risk Assessment

- **Byzantine-fairness (StableNet RI #5/#8)**: governance 면제는 **address-set
  화이트리스트**(chainconfig 파생) 에 엄격히 제한한다. 누구나 동일 화이트리스트
  주소로 dummy tx 를 만들어 `MinTip=0` 우회를 시도할 수 있으나, (a) gas 는 정상
  소비, (b) `to == GovValidator` 등으로의 가짜 호출은 시스템 컨트랙트가 ACL/권한
  체크로 거절 (블록 자체에는 실패 tx 로 포함되어 정상 처리). 따라서 economic
  invariant (base fee redistribution, validator 보상) 에는 영향 없음.
- **Race conditions**: `pool.all.Range` 는 자체 RLock (lookup.lock) 을 잡음
  → `pool.mu.Lock()` 안에서 호출해도 lock 순서는 `pool.mu` → `lookup.lock` 으로
  일관 (다른 곳도 동일 순서). `requestPromoteExecutables` 는 channel send 이므로
  `pool.mu` unlock 후 호출하지 않으면 `scheduleReorgLoop` 가 `pool.mu` 를 잡으면서
  교착 가능 → unlock 후 호출.
- **Cross-module dependencies**: chainconfig 의존을 `pool.chainconfig` 통해
  접근(이미 존재, line 235). nil-guard: `chainconfig.Anzeon`,
  `chainconfig.Anzeon.SystemContracts`, 각 contract 필드. test fixture
  (`params.TestChainConfig`) 가 Anzeon 미설정인 경우 면제 set 은 빈 set → 기존
  거동 보존.
- **`-race` scope (RI-21)**: `core/txpool/legacypool`, `core/txpool/blobpool`,
  `core/types` 의 `-race` 테스트. 추가로 `concurrency_impact` 가 가리키는
  `pool.mu` 동기화 점만 추가됨 — 새 mutex 없음.

## Historical bug hotspots
- (참고) DIAG-20260617_074336 에 따르면 이 영역은 이전에도 `anzeonTipCap` 캐시
  무효화 부재가 stable-state-drift 의 단골 후보. PR-77 의 walked example 도
  여기서 부분진단(첫 캐시에서 멈춤)을 경고함.

## Open Questions / Resolved Decisions
- **(Resolved)** blobpool 의 Layer 1 면제: blob tx 는 시스템 컨트랙트로 향하지
  않는 것이 일반적이고, governance 제안 tx 는 legacy/dynamic-fee type 이므로
  blobpool 의 Layer 1 면제는 **하지 않는다**. Layer 2(SetGasTip 동적 반영)만 적용.
- **(Resolved)** `validation.go` 인터페이스 변경 여부: 변경하지 않는다(RI-09
  cherry-pick 안전). 호출자가 `opts.MinTip=0` 으로 보내는 방식.
- **(Resolved)** Layer 3 (eth/gasprice/anzeon.go same-Root skip): 본 스코프에서
  제외 (ticket 의 명시 요구). 동반 결함이지만 별도 PR 로 처리.

## Acceptance Criteria Mapping
- AC1 (gasTip 변경 후 부합하지 않는 tx pending pool 잔존 없음) ← Layer 2
  (`SetGasTip` raising drop 은 이미 작동; lowering 시 캐시 invalidation +
  queue→pending 재평가 추가) + 회귀 unit test `TestRepricingDynamicReflection`.
- AC2 (시스템 컨트랙트 복원 제안 tx 가 floor 와 무관하게 풀 진입) ← Layer 1
  (`validateTxBasics` 면제) + 회귀 unit test
  `TestSystemContractTxExemptFromMinTip`.
- AC3 (면제는 chainconfig 파생, 하드코딩 금지) ← design Step 1 의
  `systemContractExempt(tx)` 헬퍼는 `pool.chainconfig.Anzeon.SystemContracts`
  에서만 주소 집합을 빌드.
- AC4 (수정 전 코드에서 실패하는 회귀 유닛테스트) ←
  `TestRepricingDynamicReflection` 는 "27600→30000→27600 후 27600 tip tx 가
  pending 됨" 을 확정 — 현재 코드는 Add-time 거절로 실패해야 한다.
  `TestSystemContractTxExemptFromMinTip` 도 동일.
- AC5 (multiengine_test.go 패턴 시뮬레이션) ← 새 파일
  `core/txpool/legacypool/gastip_restore_sim_test.go` (legacypool 패키지 내
  for 의존성을 줄이고, multiengine 의 chain-creation 패턴(testutils.Genesis +
  rawdb + core.NewBlockChain)을 참조 채택) — design Step 5 에서 위치 확정.
- AC6 (`go test -race` 통과) ← design 의 race scope 와 lock-경계 분석을 따른다.
- AC7 (브랜치명 `test7` 포함) ← Implementer 단계의 책임 (planner 는 명시만).

## Notes for Implementer
- 작업 브랜치 명에 반드시 `test7` 토큰 포함.
- 새 helper 함수는 cherry-pick 안전을 위해 `legacypool.go` 내에 두되, 가능하면
  분리 가능한 위치(예: `legacypool.go` 의 helper region 또는 신규
  `gas_tip_exempt.go`)에 둘 것 — Implementer 가 판단. 단, `validation.go` 본문에
  StableNet 분기를 추가하지는 말 것 (RI-09).
