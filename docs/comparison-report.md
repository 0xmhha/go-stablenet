# PR-77 비교 분석 — 우리 수정 vs 실제 정답(#77)

> 모든 주장은 두 브랜치의 실제 diff를 읽고 작성했다. 추론(정황 근거 기반)은 명시적으로 "추론"이라 표기한다.
> - 우리 수정: `pr-77-test7` 브랜치 `fix/pr-77-gastip-dynamic-reflection-test7` (base `0bf2f4d1b`, 7 커밋)
> - 실제 정답: `pr-77-origin` 커밋 `98f05c2a0 fix: refresh AnzeonTipEnv current block when GasTip changes (#77)` (base 동일)

---

## 0. 한 줄 결론

**우리 수정과 실제 정답은 서로 다른 지점을 고쳤다.** 실제 정답은 우리가 진단에서 가설 (B)로 **기각하고 "Layer 3 — 스코프 제외"로 명시한** `eth/gasprice/anzeon.go`의 same-root skip이 핵심이었다. 우리의 1차 수정(Layer 1 거버넌스 면제)은 실제 정답이 전혀 건드리지 않은 영역이고, 우리의 Layer 2(per-tx 캐시 clear)는 **실제 버그를 고치지 못할 가능성이 높다** — 캐시를 비워도 재계산이 여전히 stale한 env를 읽기 때문이다(§5).

---

## 1. 입력과 우리가 인식한 문제

- 입력: `docs/pr-77.md` — "거버넌스로 gasTip을 27600→30000으로 올린 뒤 27600으로 **복원**하는 제안 tx가 pending pool에 정체된다."
- 우리 진단(`DIAG-20260617_074336`)이 지목한 **1차 원인**:
  - **Add-time 게이트의 순환 데드락** — `core/txpool/validation.go:119` `tx.GasTipCapIntCmp(opts.MinTip) < 0`가 복원 제안 tx(tip 27600)를 현재 floor(30000)로 거절 → 풀 진입 실패 → floor를 낮추려면 그 tx가 포함돼야 하는 데드락.
- 우리가 **2차(동반 결함)**로 본 것: `Transaction.anzeonTipCap` invalidator 0개, `SetGasTip` 내림 시 무동작.
- 우리가 **명시적으로 기각/제외**한 것:
  - 가설 (B) `AnzeonTipEnv.currentBlock` same-root skip → "consume 측이라 add 거절과 무관, 올림/내림 비대칭 미설명"으로 기각.
  - "Layer 3 (eth/gasprice/anzeon.go same-root skip): 본 스코프에서 제외" (`analysis.md` line 221).

➡️ **실제 정답은 정확히 이 기각·제외한 (B)/Layer 3이었다.**

---

## 2. 우리가 설계·구현한 것 (Layer 1 + Layer 2)

| 변경 | 파일 | 내용 |
|---|---|---|
| Layer 1 | `legacypool.go` `validateTxBasics` / `Pending` | 시스템 컨트랙트(chainconfig 파생 주소) 대상 tx를 MinTip 게이트·Pending truncation에서 면제(`local` 면제 미러링) |
| Layer 2 | `transaction.go` | `ClearAnzeonTipCap()` (typed-nil) 신규 |
| Layer 2 | `legacypool.go` `SetGasTip` | 내림 시: `pool.all` 순회 `ClearAnzeonTipCap()` → `Reheap()` → queue 계정 `requestPromoteExecutables`(unlock 후) |
| Layer 2 | `blobpool.go` `SetGasTip` | 내림 무동작 사유 주석 |
| 테스트 | `legacypool_test.go`, `gastip_restore_sim_test.go` | 회귀 3종 (pre-fix FAIL 검증) |

규모: 프로덕션 3파일 +108/−5, 테스트 +359.

---

## 3. 실제 정답(#77)의 수정 — 정확한 diff

**프로덕션 2파일, +23/−2, 테스트 없음.**

### (a) `eth/gasprice/anzeon.go` — SetCurrentBlock의 same-root skip 보강 (핵심)
```go
- if env.currentBlock == nil || env.currentBlock.Root != header.Root {
+ if env.currentBlock == nil || env.currentBlock.Root != header.Root || gasTipChanged(env.currentBlock.GasTip(), header.GasTip()) {
```
+ `gasTipChanged(a,b)` 헬퍼 신규. 주석: *"empty blocks share the same root as the governance-change block but carry the updated GasTip in their header."*

### (b) `core/txpool/legacypool.go` — `RemotesBelowTip`가 raw가 아닌 effective tip 사용
```go
- if tx.GasTipCapIntCmp(threshold) < 0 {
+ tipcap := tx.GetAnzeonTipCap(); if tipcap == nil { tipcap = tx.GasTipCap() }
+ if tipcap.Cmp(threshold) < 0 {
```

### 실제 버그의 메커니즘 (위 두 변경의 주석·코드 기반)
StableNet의 핵심 도메인 사실: **미인가(일반) 계정의 effective tip은 `tx.GasTipCap()`이 아니라 블록 헤더의 `block.GasTip()`(거버넌스 값)이다** (`anzeon.go` `GetAnzeonTipCap` 미인가 분기, line 125-128). 즉 일반 tx의 실효 tip == 현재 거버넌스 GasTip.
- 거버넌스 변경값은 **블록 헤더의 GasTip 필드**로 전파된다.
- 변경 적용 블록 다음의 **빈 블록**은 state root가 동일(상태 변화 없음)하지만 헤더에는 새 GasTip을 담는다.
- 기존 `SetCurrentBlock`은 `root` 동일이면 갱신 skip → env가 옛 GasTip 유지 → `GetAnzeonTipCap`가 stale 값 반환 → Pending 필터의 effective-tip 평가가 어긋나 tx가 부당하게 처리됨.
- (b)는 floor 인상 시 drop 판정을 raw GasTipCap이 아닌 실효 tip(캐시된 block.GasTip)으로 일치시킨다.

---

## 4. 핵심 차이 비교

| 항목 | 우리 수정 | 실제 정답(#77) |
|---|---|---|
| 근본 원인 지목 | Add-게이트 데드락(1차) + 캐시/SetGasTip(2차) | **AnzeonTipEnv same-root skip + RemotesBelowTip effective-tip 불일치** |
| `eth/gasprice/anzeon.go` | **건드리지 않음** (스코프 제외) | **핵심 수정** (SetCurrentBlock + gasTipChanged) |
| `RemotesBelowTip` | 미변경(raw GasTipCap 그대로) | effective(캐시) tip 사용으로 수정 |
| 시스템 컨트랙트 면제(validateTxBasics) | 신규 추가 | **없음** |
| `ClearAnzeonTipCap` / SetGasTip 내림 반응 | 신규 추가 | **없음** |
| blobpool 변경 | 주석 추가 | 없음 |
| 변경 규모(프로덕션) | 3파일 +108/−5 | 2파일 +23/−2 |
| 테스트 | 회귀 3종 추가 | 없음 |
| 접근 성격 | 광범위·가설 주도·방어적 | 최소·외과적·도메인 정합 |

---

## 5. 결정적 차이: 우리 수정은 실제 버그를 고치는가? — **아니오일 가능성이 높음**

근거(코드로 검증):
1. 실제 버그 = `env.currentBlock`이 same-root skip으로 stale → `GetAnzeonTipCap`가 **stale `env.currentBlock.GasTip()`** 반환(anzeon.go:125-128).
2. 우리 Layer 2 `ClearAnzeonTipCap`는 **per-tx 캐시만** 비운다. 다음 `EffectiveGasTip` 재계산은 캐시가 nil이므로 `GetAnzeonTipCap(env)`를 호출 → **여전히 stale한 env**를 읽는다(`transaction.go:398-401`). 즉 캐시를 비워도 stale 소스가 그대로라 효과 없음.
3. 우리는 `SetCurrentBlock`/`anzeon.go`를 건드리지 않았다(§3 확인). 따라서 빈 블록 same-root 시나리오에서 env는 계속 stale.

➡️ **우리 수정은 실제 재현 경로(거버넌스 변경 후 빈 블록의 same-root)에서 버그를 해소하지 못한다.** 우리 테스트가 통과한 이유는 `testBlockChain` 목업 + 직접 `SetGasTip` 호출로 **우리 가설의 메커니즘만 검증**했을 뿐, 실제 버그 조건(same-root 빈 블록)을 한 번도 재현하지 않았기 때문이다(자기 가설 확증 테스트).

추가로 우리 Layer 1(시스템 컨트랙트 면제)은 실제 정답이 불필요하다고 본 영역이다. 거버넌스 제안자는 대개 **인가 계정(validator)**이고, 인가 계정은 `GetAnzeonTipCap`에서 `tx.GasTipCap()`를 그대로 쓰며 add-게이트도 raw GasTipCap을 본다 → 충분한 tip로 서명하면 데드락이 성립하지 않는다(추론, 단 실제 정답이 면제를 전혀 넣지 않은 점이 강한 정황). 즉 우리의 "순환 데드락"은 **실재하지 않는 실패 모드**를 겨냥했을 가능성이 있다.

---

## 6. 왜 이런 차이가 발생했나 (근본 원인 분석)

1. **도메인 모델 오해 (가장 결정적).** "미인가 계정의 effective tip = block.GasTip()"라는 Anzeon 핵심 규칙을 진단에 충분히 반영하지 못했다. 이 규칙을 알았다면 "값이 헤더로 전파되고 env가 그 헤더를 stale하게 잡는다"는 실제 경로가 1순위였을 것이다. 우리는 effective tip을 `tx.GasTipCap()` 중심으로 사고해 add-게이트(raw 비교)에 앵커링됐다.
2. **입력 프레이밍에 앵커링.** `docs/pr-77.md`의 "복원 *제안 트랜잭션*이 정체"를 "그 거버넌스 tx가 add-게이트에서 거절"로 좁게 해석. 실제는 거버넌스 변경 *이후 일반 tx들*의 effective-tip 평가가 stale해지는 더 넓은 문제였다.
3. **올바른 가설을 잘못된 기준으로 기각.** 가설 (B)를 "올림/내림 비대칭을 설명 못 함"으로 기각했는데, 비대칭은 **빈 블록 same-root skip**으로 설명된다(우리가 그 메커니즘을 끝까지 전개하지 않음). 진단의 "FALSIFY competing hypotheses" 단계에서 (B)를 충분히 깊게 추적하지 않았다.
4. **자기 가설에 맞춘 테스트.** 목업 기반으로 우리 메커니즘(캐시 clear, 면제)만 검증 → pre-fix FAIL/post-fix PASS는 얻었지만 "실제 버그를 잡는 회귀"가 아니라 "내 변경을 잡는 회귀"였다. 실 환경 재현(체인에서 빈 블록 생성)을 하지 못한 시뮬레이션 한계가 이를 가렸다.
5. **스코프 조기 확정.** Layer 1+2로 사용자 승인 후, 정답이 있던 Layer 3를 "동반 결함, 별도 PR"로 밀어냈다. 확신도 Medium-High에서 멈추지 말고 "실제 로그에 ErrUnderpriced가 찍히는가"(진단서 §4가 스스로 제시한 확증 단계)를 실행했어야 했다.

---

## 7. 개선 제안

1. **진단 단계에서 도메인 불변식을 먼저 확정.** "이 체인에서 tx의 effective tip은 무엇으로 결정되는가?"를 코드(`GetAnzeonTipCap`)로 못박은 뒤 가설을 세운다. cks/도메인 스킬이 이 규칙을 surfacing하도록 진단 체크리스트에 "effective-value 정의 확인"을 강제.
2. **기각한 가설도 메커니즘 끝까지 전개.** 특히 "비대칭"처럼 강한 변별 신호는, 기각 전에 각 후보로 비대칭을 *설명 시도*한 결과를 적게 한다(가설 (B)를 빈 블록 시나리오로 전개했다면 채택됐을 것).
3. **확증 단계를 스킵하지 말 것.** 진단서가 스스로 적은 "확신을 올리는 방법"(로그의 ErrUnderpriced 유무, 실제 stuck tx의 gasTipCap 값)을 **구현 전에** 1개라도 실행. 이는 add-게이트 vs consume-side를 바로 갈랐을 것이다.
4. **테스트는 "내 수정"이 아니라 "실제 결함 조건"을 재현.** 목업이 재현 못 하는 조건(빈 블록 same-root)이 있으면, 그 한계를 명시하고 통합 레벨(실 BlockChain/빈 블록 생성)로 끌어올린다. "pre-fix FAIL"만으로 회귀력을 신뢰하지 말 것 — 무엇에 대한 FAIL인지 확인.
5. **스코프 제외 항목을 리스크로 승격.** "Layer 3 제외"처럼 정답 후보를 제외할 때는, 제외 근거를 사용자 승인 질문에 **명시적 리스크로** 노출(우리는 조용히 별도 PR로 미뤘다).
6. **최소 수정 우선.** 실제 정답은 23줄이었다. 광범위·방어적 변경(면제, 캐시 무효화 인프라)은 byzantine-fairness 인접 리스크와 표면적을 키운다 — 근본 edge를 못 박기 전의 확장은 지양.

---

## 8. 부록 — 검증 근거 (인용)

- 실제 정답 커밋/규모: `git diff --stat 0bf2f4d1b..98f05c2a0` → `legacypool.go(+9)`, `anzeon.go(+16)`, 테스트 없음.
- effective-tip 도메인 규칙: `pr-77-origin/eth/gasprice/anzeon.go` `GetAnzeonTipCap` 미인가 분기 `return env.currentBlock.GasTip()`.
- 우리 브랜치 스코프: `git diff --stat 0bf2f4d1b..HEAD` → `blobpool.go/legacypool.go/transaction.go`만, `anzeon.go` **미포함**.
- 우리 재계산 경로: `pr-77-test7/core/types/transaction.go:398-401` `EffectiveGasTip` — 캐시 nil이면 `anzeonTipEnv.GetAnzeonTipCap(tx)` 호출(=stale env 소스).
