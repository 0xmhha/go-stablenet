# PR-77 GasTip 거버넌스 이슈 — 내 수정 vs 실제 정답 비교 분석 레포트

> 본 레포트의 모든 동작 주장은 **실제 빌드/테스트 실행으로 검증**했다(아래 "검증 매트릭스" 참조).
> 코드 인용은 두 저장소의 실제 diff에서 가져왔다.
> - 내 작업 트리: `pr-77-test8`
> - 실제 정답: `pr-77-origin` 의 커밋 `98f05c2a0 "fix: refresh AnzeonTipEnv current block when GasTip changes (#77)"`

---

## 1. 입력(docs/pr-77.md)으로부터 문제를 어떻게 인식했는가

`docs/pr-77.md` 의 핵심 증상:
- 거버넌스로 GasTip을 27600 → 30000 Gwei로 **상향**은 정상.
- 다시 27600 Gwei로 **원복**하는 제안 트랜잭션이 **pending pool에 계속 남아 블록에 포함되지 않음**.

코드 추적으로 다음 메커니즘을 파악했다(이 부분은 정답과 동일하게 옳았다):
- StableNet(Anzeon)에서 **비인가(non-authorized) 계정의 실효 gas tip은 트랜잭션 자체 tip이 아니라 "블록 헤더의 거버넌스 GasTip"** 으로 결정된다 (`eth/gasprice/AnzeonTipEnv.GetAnzeonTipCap`).
- 그런데 txpool의 여러 게이트가 헤더 기반 실효 tip이 아니라 **raw `GasTipCap`** 또는 **lagging된 env 값**으로 동작 → 상향/원복 비대칭이 생긴다.

**여기서 한 가지 결정적 선택을 했고, 그것이 정답과의 모든 차이를 만들었다.**
재현 테스트를 **결정론적으로** 만들기 위해, 원복 제안 트랜잭션의 `GasTipCap`을 27600 Gwei로 **명시적으로 고정**했다(운영자가 항상 기준 tip으로 제출한다는 가정). 그 결과 내 재현은 다음 증상을 보였다:

```
transaction underpriced: gas tip cap 27600000000000, minimum needed 30000000000000
```

→ 즉 **"admission(진입) 단계 거부"**. 트랜잭션이 풀에 들어가지조차 못함.

그러나 docs의 실제 증상은 **"pending pool에 남아 있음(=풀에는 들어갔으나 블록에 미포함)"** 이다. **이 둘은 다른 증상이다.** 내 강제-tip 재현은 진짜 버그가 아니라 "유사하지만 다른" 증상을 잡았다. (이 점은 §3·§5에서 핵심 차이로 이어진다.)

---

## 2. 내가 설계·구현한 수정 (pr-77-test8)

설계 원칙: *"비인가 계정은 자기 tip을 통제하지 못하고 헤더 tip을 내므로, raw-tip 기반 최소 tip 게이트를 적용하면 안 된다"* → **게이트마다 비인가 계정을 면제**.

3개 파일, +68 라인:

| 파일 | 변경 |
|---|---|
| `core/txpool/validation.go` | stateless 진입검사에서 Anzeon이면 raw-tip 검사를 **건너뜀**. 대신 stateful `ValidateTransactionWithState`에서 `opts.State.IsAuthorized(from)`로 **인가 계정에만** 최소 tip 강제. `ValidationOptionsWithState`에 `MinTip` 필드 추가. |
| `core/txpool/legacypool/legacypool.go` | `Pending` 필터에서 `pool.currentState.IsAuthorized(addr)`로 **비인가 계정을 최소-tip 제외에서 면제**. `validateTx`에서 `MinTip` 전달. |
| `core/txpool/blobpool/blobpool.go` | (위 validation.go 변경의 부수효과로) blob 경로에도 stateful `MinTip` 전달. |

추가로 재현 테스트 `systemcontracts/test/gov_gastip_lifecycle_test.go`(강제 tip)를 작성. 디버깅 중 `AnzeonTipEnv.currentBlock`이 **블록 3(=실행 블록, 헤더 GasTip=27600)에 멈춰 있는 것**을 로그로 관측했으나, 이를 *"신뢰할 수 없는 입력"* 으로 간주하고 **우회**하는 길을 택했다(env를 고치지 않고 비인가 계정을 필터에서 빼는 방식).

---

## 3. 실제 정답(pr-77-origin, #77)은 무엇이며 어떻게 다른가

정답은 2개 파일, +23 라인. **`validation.go`는 전혀 건드리지 않았다.**

### (a) `eth/gasprice/anzeon.go` — 근본 원인 직접 수정
```go
// 기존: 상태 root만 비교
if env.currentBlock == nil || env.currentBlock.Root != header.Root {
// 정답: 헤더 GasTip 변화도 갱신 조건에 추가
if env.currentBlock == nil || env.currentBlock.Root != header.Root || gasTipChanged(env.currentBlock.GasTip(), header.GasTip()) {
```
**근본 원인**: `SetCurrentBlock`이 `Root`만 비교했다. 거버넌스 변경 실행 블록 N 이후의 **빈 블록(N+1 등)은 상태 변화가 없어 N과 같은 root**를 가진다. 따라서 env가 블록 N에 고정되고, N의 헤더 GasTip은 변경 *이전* 값(27600)이라 **env가 stale한 27600을 계속 반환** → 비인가 계정의 `EffectiveGasTip`이 minTip(30000) 미만 → `Pending`에서 제외 → **pending pool 정체**. 이 한 줄이 admission·inclusion·ordering 등 env를 쓰는 **모든 경로를 동시에** 정상화한다.

### (b) `core/txpool/legacypool/legacypool.go` — `RemotesBelowTip` 드롭 기준 보정
```go
// 기존: raw tip로 드롭
if tx.GasTipCapIntCmp(threshold) < 0 {
// 정답: 캐시된 실효 tip(GetAnzeonTipCap)으로 드롭
tipcap := tx.GetAnzeonTipCap(); if tipcap == nil { tipcap = tx.GasTipCap() }
if tipcap.Cmp(threshold) < 0 {
```

### 핵심 차이 요약
| 항목 | 내 수정 | 정답(#77) |
|---|---|---|
| 접근 | 증상 회피 — 게이트마다 비인가 계정 **면제** | 근본 원인 제거 — env의 **stale 갱신 버그** 수정 |
| 변경 규모 | 3파일 / +68 | 2파일 / +23 |
| `validation.go` | 진입검사 의미 변경 | **무수정** |
| stale env 자체 | **그대로 남김**(우회만) | **수정됨** |
| 동작 계약 | 비인가 계정의 raw-tip 하한 게이트 **제거**(더 관대) | 표준 underpriced 의미 **유지** |
| blobpool | 부수 변경 발생 | 변경 없음 |

---

## 4. 검증 매트릭스 (모두 실제 실행으로 확인)

같은 시나리오를 두 가지 테스트로 3가지 코드 상태에서 실행:
- **강제-tip 테스트**(`gov_gastip_lifecycle_test.go`): 원복 tx의 tip을 27600으로 고정 (내가 작성한 것).
- **현실-tip 테스트**(`gov_gastip_realistic_test.go`): tip 미지정 → 지갑처럼 `SuggestGasTipCap` 사용 (docs의 실제 시나리오에 충실).

| 테스트 \ 코드 | Pristine(무수정) | 정답(#77) | 내 수정 |
|---|---|---|---|
| 강제-tip (raw 27600) | admission 거부 | **FAIL** (admission 거부) | PASS |
| 현실-tip (suggested) | **HANG**(pending 정체=실제 증상) | PASS | PASS |

근거가 된 관측:
- Pristine + 현실-tip → **타임아웃 행(hang)**. 로그: `SuggestGasTipCap=30000000000000` → tx는 30000으로 정상 진입했으나(=admission 통과) 블록 미포함으로 `WaitMined`가 무한 대기. **이것이 docs의 진짜 증상.**
- 정답 + 현실-tip → PASS (`SuggestGasTipCap=30000`, env 갱신으로 포함됨).
- **정답 + 내 강제-tip 테스트 → FAIL** (`transaction underpriced ... minimum needed 30000000000000`). 즉 **내 테스트는 정답 코드에서 통과하지 못한다.**

→ **두 핵심 결론**:
1. 두 수정 모두 *실제* 버그(현실-tip 시나리오)는 해결한다.
2. 그러나 **내 강제-tip 테스트는 정답과 양립하지 않는 "다른 계약"을 검증**한다. 즉 내 테스트는 docs 요구사항이 아니라 *내 설계 의견*("비인가 sub-minimum tip도 받아줘야 한다")을 박제한 것이다.

---

## 5. 차이가 왜 발생했는가 (정직한 원인 분석)

1. **재현 방식의 분기점.** 결정론을 위해 raw tip을 강제한 순간, 버그의 *발현 지점*이 inclusion(실제) → admission(내 재현)으로 바뀌었다. 진짜 시나리오는 지갑이 `SuggestGasTipCap`(=현재 헤더값)을 쓰므로 raw tip이 부족할 일이 없고, 문제는 오직 "풀에 들어간 뒤 포함되지 않음"이다. 현실-tip 테스트를 먼저 만들었다면 같은 근본 원인(stale env)으로 수렴했을 가능성이 높다.

2. **관측을 해석한 방향.** 나는 `currentBlock`이 블록 3에 멈춘 것을 **보고도**, env를 *고칠 대상*이 아니라 *피할 대상*으로 판단했다(필터에서 비인가 면제). 정답 저자는 동일 현상을 **"root-only 비교로 인한 stale" 라는 근본 원인**으로 규정하고 한 줄로 고쳤다.

3. **동시성 제약에 대한 과한 회피.** `validateTxBasics`가 lock 밖에서 도므로 env 접근이 race가 된다고 보고 admission을 stateful로 옮기는 큰 우회를 했다. 정답은 env를 admission에서 직접 안 쓰고, env 자체를 올바르게 유지함으로써 기존 `EffectiveGasTip` 경로를 그대로 신뢰했다.

---

## 6. 개선점 (할루시네이션 배제, 코드 근거 기반)

내 수정의 실제 약점들:

1. **근본 원인(stale env)을 남겼다.** 내 패치는 admission/Pending만 비인가 면제로 우회했을 뿐, `pool.anzeonTipEnv`의 stale 자체는 그대로다. 따라서 **env를 쓰는 다른 경로**(예: `pricedList`의 `EffectiveGasTipCmp` 기반 정렬, txpool content/inspect API의 실효 tip 표시)는 여전히 stale 값을 본다. 정답은 이를 한 곳에서 모두 고친다. → **정답의 env-fix(또는 `gasTipChanged` 갱신 조건)를 채택**하는 것이 옳다.

2. **내 패치 내부의 비일관성.** admission에서는 비인가 sub-minimum-tip tx를 *받아주지만*, `RemotesBelowTip`(상향 시 드롭, 미수정으로 raw-tip 사용)은 그 tx를 **다음 상향 때 드롭**할 수 있다. "받아놓고 나중에 버림"이라는 모순. 정답은 `RemotesBelowTip`도 실효 tip 기준으로 맞춰 일관적이다.

3. **불필요한 blast radius.** `validation.go`의 진입검사 의미를 바꾼 탓에 blobpool까지 손대야 했다(+표준 underpriced 계약 변경). 정답은 0줄로 끝낸 영역이다.

4. **테스트가 요구사항이 아닌 의견을 검증.** 강제-tip 테스트는 docs 시나리오(정상 제안 lifecycle)가 아니라 인위적 입력을 검증한다. **현실-tip 테스트(`gov_gastip_realistic_test.go`)가 docs에 충실**하며, pristine에서 hang→정답/내수정 모두 PASS로 회귀를 정확히 가둔다. 향후엔 이쪽을 정식 회귀 테스트로 채택해야 한다.

### 권고
- **코드는 정답(#77) 방식**(env의 `gasTipChanged` 갱신 + `RemotesBelowTip` 실효 tip)으로 교체하는 것이 근본적·소규모·저위험.
- **테스트는 현실-tip 버전**을 채택(+ 원하면 강제-tip 케이스는 "비인가 계정 정책"을 명시적으로 결정한 뒤 별도 정책 테스트로).
- 교훈: **재현 테스트는 결정론을 위해 입력을 인위적으로 비틀기 전에, 보고된 "증상 그 자체"(여기선 admission 거부가 아니라 inclusion 정체)를 먼저 재현**해야 근본 원인으로 수렴한다.
