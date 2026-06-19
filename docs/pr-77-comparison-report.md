# PR-77 GasTip 복원 정체 — 내 수정 vs 실제 정답(#77) 비교 분석 레포트

> 작성 기준: 모든 주장은 실제 코드 diff·테스트 실행 로그에 근거한다. 추측은 "추정"으로 명시한다.

## 0. 비교 대상

| 항목 | 내 작업 (`pr-77-test9`) | 실제 정답 (`pr-77-origin`) |
|---|---|---|
| 베이스 커밋 | `0bf2f4d1b` | `0bf2f4d1b` (동일) |
| 수정 커밋 | (미커밋, 작업트리) | `98f05c2a0` "fix: refresh AnzeonTipEnv current block when GasTip changes (#77)" |
| 변경 소스 파일 | `core/types/transaction.go`, `core/txpool/validation.go`, `eth/gasprice/anzeon.go` | `eth/gasprice/anzeon.go`, `core/txpool/legacypool/legacypool.go` |
| 테스트 | `systemcontracts/test/gov_validator_gastip_test.go` | `systemcontracts/test/gov_gastip_lifecycle_test.go` (untracked) |

---

## 1. 작업 개요 (입력 → 진행)

- 입력: `docs/pr-77.md` (거버넌스로 gasTip을 27600→30000 Gwei로 올린 뒤 27600으로 **복원**하는 제안 트랜잭션이 pending pool에 정체되는 문제).
- 진행 방식: 통과기준 유닛테스트(헤더 extradata `WBFTExtra.GasTip` 기준, N/N+1·M/M+1 검증)를 **먼저 구현해 재현**한 뒤 원인을 추적.
- 재현 환경: `systemcontracts/test`의 `simulated.WBFTBackend` (실제 txpool + miner + WBFT 엔진 + 실제 거버넌스 propose→approve 흐름). `consensus/wbft/backend/multiengine_test.go`는 txpool이 없어 정체 자체를 재현할 수 없어 채택하지 않음.

---

## 2. 내가 인식한 문제 (근거 로그 포함)

재현 테스트에 txpool/엔진 디버그 로그를 넣어 다음을 **실측**했다.

```
pending-consider nonce=1 cachedAnzeonTip=27,600  rawGasTipCap=30,000  minTip=30,000  effective=27,600  → filtered
GetAnzeonTipCap  block=2  headerTip=27,600  authorized=false        ← 채굴 시점인데 env가 block 2에 고정
```

두 개의 결함을 식별:

1. **per-tx 캐시 동결 (commit #47 유래).** `Transaction.anzeonTipCap`가 제출 시점에 한 번 캐시되고 갱신되지 않아, 일반(미인가) 계정의 실효 tip이 옛 네트워크 tip(27600)에 고정 → 상향(30000) 후 floor 미달로 매 블록 필터링.
2. **`AnzeonTipEnv.SetCurrentBlock`의 state-root 기준 캐싱.** 빈 블록은 직전 블록과 **state root가 동일**하지만 헤더 extradata의 GasTip은 다르다(N+1 적용 규칙). root만으로 갱신 여부를 판단해 `currentBlock`이 변경 블록에 고정되고, 실효 tip이 stale.

→ 즉 "실효 tip < floor"가 영구화되어 복원 트랜잭션이 채굴 후보에서 계속 제외됨.

실제 정답 #77의 커밋 메시지도 동일한 핵심 원인을 적시한다: *"SetCurrentBlock only checked state root equality, so empty blocks after a governance gasTip change left currentBlock stuck ... non-validator transactions were validated using a stale GasTip."* → **1차 진단(=원인 2)은 정답과 일치.**

---

## 3. 내 설계/구현

세 곳을 수정해 "실효 tip을 항상 현재 헤더 기준으로 계산"하도록 함.

1. `eth/gasprice/anzeon.go`
   - `SetCurrentBlock`: 갱신 조건을 **헤더 해시 변경** 기준으로 변경. state(StateDB)는 root가 바뀔 때만 재로딩(비용 절감), `currentHeaderTip`(헤더 GasTip 디코딩값)·signer는 헤더가 바뀔 때마다 갱신.
   - 블록당 1회 디코딩한 `currentHeaderTip` 필드를 추가해 reheap 시 헤더 재디코딩 비용 회피.
2. `core/types/transaction.go`
   - `EffectiveGasTip`가 **항상 env에서 live 조회**하도록 변경. per-tx 캐시(`anzeonTipCap` 필드, `Set/GetAnzeonTipCap`) 제거.
3. `core/txpool/validation.go`
   - 제출 시점의 per-tx 캐시 적재 코드 제거.

설계 의도: 미인가 계정의 실효 tip은 "현재 네트워크(헤더) gasTip"이라는 불변식을 매 평가 시점에 보장 → 어떤 시점의 gasTip 변동에도 stale이 생기지 않음.

검증: 새 테스트 통과(`[raise] N=2:27600, N+1=3:30000`, `[restore] M=5:30000, M+1=6:27600`). 회귀: `core/types`, `core/txpool/...`, `eth/gasprice/...`, `miner/...`, `consensus/wbft/...`, `systemcontracts/...` 전부 통과.

---

## 4. 실제 정답(#77) 구현

1. `eth/gasprice/anzeon.go`
   - `SetCurrentBlock` 갱신 조건에 `|| gasTipChanged(env.currentBlock.GasTip(), header.GasTip())` 추가, 헬퍼 `gasTipChanged` 신설. **per-tx 캐시(#47)는 유지.** `GetAnzeonTipCap`는 그대로 `env.currentBlock.GasTip()`(매 호출 디코딩) 사용.
2. `core/txpool/legacypool/legacypool.go`
   - `RemotesBelowTip`가 raw `tx.GasTipCap()` 대신 **`tx.GetAnzeonTipCap()`(캐시된 실효 tip)** 로 드랍 판정. floor 상향 시 미인가 계정 tx를 실효 tip 기준으로 정리하기 위함.

설계 의도: per-tx 캐시는 "엔트리 시점의 실효 tip 스냅샷"으로 **유지**하되, env의 `currentBlock`만 헤더 GasTip 변경 시 올바르게 전진시켜 캐시에 **올바른 값이 적재**되도록 함. 드랍 경로(RemotesBelowTip)도 캐시값과 일관되게 맞춤.

---

## 5. 핵심 차이점

| 축 | 내 수정 | 정답(#77) |
|---|---|---|
| 1차 원인(env stale) | 동일하게 수정 (해시 기준 갱신) | 동일하게 수정 (`gasTipChanged` 추가) |
| per-tx 캐시(#47) | **제거** → 항상 live 계산 | **유지** → 엔트리 시점 스냅샷 |
| 실효 tip 의미 | 미인가 계정 = "항상 현재 네트워크 tip" | 미인가 계정 = "풀 진입 시점의 네트워크 tip(동결)" |
| reheap 성능 보전 | env에 블록당 헤더 tip 캐시 추가 | 기존 per-tx 캐시로 보전 |
| `RemotesBelowTip` | 변경 없음(raw 사용) — live 필터가 정합성 담당 | 캐시 실효 tip 기준으로 변경 |
| 수정 표면적 | 3파일(필드·메서드 제거 포함, 더 큼) | 2파일(가산적, 더 작음) |

행동(behavior) 차이의 핵심: gasTip이 **상향**된 뒤, 낮은 raw tip으로 이미 풀에 들어와 있던 미인가 계정 tx에 대해
- 내 수정: 실효 tip을 현재 네트워크 tip으로 재평가 → (gasFeeCap이 충분하면) **유지·채굴**(더 낸다).
- 정답: 엔트리 시점 스냅샷이 새 floor 미달 → `RemotesBelowTip`로 **드랍**.

---

## 6. 검증으로 드러난 결정적 사실 (교차 실행)

두 테스트를 두 수정에 교차 실행했다(모두 실제 실행 로그 기반).

| 테스트 \ 수정 | 내 수정 | 정답(#77) |
|---|---|---|
| 내 테스트 (복원 tx가 **현재** tip 30000 지불) | PASS | PASS |
| 정답 테스트 (복원 tx가 **옛** tip 27600 명시 지불) | FAIL | **FAIL** |

정답 테스트가 **정답 수정에서도 실패**하며, 실패 원인은 동일하다:

```
transaction underpriced: gas tip cap 27600000000000, minimum needed 30000000000000
"restore proposal must be accepted into a block (stuck-in-pending bug)"
```

즉 정답 테스트는 운영지갑이 **고정 baseline tip(27600)** 으로 제출하는 시나리오를 모델링하는데, 이 tx는 채굴 필터가 아니라 **제출 게이트(`core/txpool/validation.go` `ValidateTransaction` → `tx.GasTipCapIntCmp(MinTip) < 0`, raw 기준)** 에서 거부된다. #77은 이 게이트를 건드리지 않으므로 자기 테스트를 통과시키지 못한다(해당 테스트는 #77과 함께 커밋되지 않은 untracked 파일이다).

내 테스트의 복원 tx는 `SuggestGasTipCap`(=현재 네트워크 tip=30000)으로 자동 책정되어 제출 게이트는 통과하고, **오직 env-stale 경로**로만 정체된다. 그래서 두 수정 모두 통과한다.

---

## 7. 차이가 발생한 이유

1. **원인 진단은 일치, 해결 철학이 갈렸다.** 둘 다 env stale을 핵심으로 봤다. 나는 "stale을 만드는 캐시 자체"를 잠재 결함으로 보고 제거하는 쪽(근본 단순화), #77은 "캐시는 유지하되 그 입력(currentBlock)을 올바르게 전진"시키는 최소 침습 쪽을 택했다.
2. **재현 입력(테스트)의 tip 책정 전략이 달랐다.** 내 테스트는 지갑 기본 동작(`SuggestGasTipCap`)을, 정답 테스트는 "운영자가 baseline tip 고정 제출"을 모델링했다. 이 차이가 **제출 게이트라는 또 다른 결함면**을 드러냈고, 내 재현 경로에서는 그 면이 가려져 있었다.
3. **결과적으로 두 수정 모두 동일한 잔존 갭(제출 게이트의 raw-tip 검사)을 남겼다.** 내 진단은 "env stale + per-tx 캐시"에 집중하느라 제출 게이트(raw GasTipCap vs floor)가 미인가 계정 모델과 모순된다는 점까지는 도달하지 못했다.

---

## 8. 개선 제안 (실측 기반, 환각 배제)

1. **제출 게이트를 Anzeon 실효 tip과 정합화 (가장 중요).** `core/txpool/validation.go`의 `ValidateTransaction`이 미인가 계정에 대해 raw `GasTipCap` 대신 실효 tip(헤더 gasTip) 기준으로 floor를 검사하도록 해야, "운영자가 baseline tip으로 제출"하는 정답 테스트 시나리오까지 해결된다. 현재는 두 수정 모두 미해결.
2. **두 테스트를 합집합으로 채택.** "현재 tip 제출"(내 테스트)과 "옛 tip 고정 제출"(정답 테스트)은 서로 다른 결함면을 친다. 둘 다 회귀 스위트에 포함해야 재발 방지가 완전해진다.
3. **per-tx 캐시 유지 시 갱신 책임 명문화.** #77처럼 캐시를 유지한다면, 캐시는 "엔트리 시점 스냅샷"이라는 의미를 주석/이름으로 못박고, gasTip이 tx 대기 중에 또 변할 때(이중 변경)의 동작을 테스트로 고정하는 것이 안전하다. (내 방식처럼 제거하면 이 책임 자체가 사라진다.)
4. **상향 시 드랍 정책 합의.** "낮은 tip로 들어온 미인가 tx를 floor 상향 시 드랍할지(=#77) vs 현재 tip으로 재평가해 유지할지(=내 방식)"는 정책 결정 사항이다. 운영자 UX(제안 tx 유실 방지) 관점에선 유지가 유리할 수 있으나, 합의가 필요하다.
5. **`gasTipChanged`/헤더 해시 비교의 비용.** 내 방식은 `header.Hash()` 비교(저빈도 호출이라 무해)와 블록당 1회 디코딩, #77은 호출당 `currentBlock.GasTip()` 디코딩 + per-tx 캐시. 어느 쪽이든 reheap 빈도/풀 크기에서의 실측 벤치를 한 번 떠두면 선택 근거가 명확해진다.

---

## 부록 A. 검증 명령/결과 요약
- 내 수정 + 내 테스트: PASS (`systemcontracts/test`), 회귀 스위트 전부 ok.
- 내 수정 + 정답 테스트: FAIL — `transaction underpriced ... minimum needed 30000000000000`.
- 정답 수정 + 정답 테스트: FAIL — 동일 메시지.
- 정답 수정 + 내 테스트: PASS.
- (환경) `systemcontracts/test`는 solc + OZ 서브모듈 필요(현재 브랜치에서 `git submodule update --init` 수행, dev 브랜치 이동 없음).
