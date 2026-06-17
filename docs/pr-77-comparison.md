# PR-77 비교 분석 레포트: 내 수정 vs. 실제 정답(`pr-77-origin` #77)

> 비교 대상
> - **내 수정**: `pr-77-test6` 작업 트리 (base `0bf2f4d1b`)
> - **정답**: `pr-77-origin` 커밋 `98f05c2a0` — *"fix: refresh AnzeonTipEnv current block when GasTip changes (#77)"* (동일 base `0bf2f4d1b` 위)
>
> 두 수정 모두 동일한 base 커밋 위에서 작업되어 1:1 비교가 가능하다. 본 레포트의 모든 코드 인용은
> 실제 `git diff` / `git show` 출력에서 발췌했으며 추정은 "추정"으로 명시한다.

---

## 1. 입력과 문제 인식

### 1.1 작업 입력
`docs/pr-77.md` 한 문서만을 입력으로 받았다. 핵심 요구는:

1. **대상 1** — 신규 헤더의 state root가 같아도 GasTip이 다르면 `currentBlock`이 갱신되어야 한다.
   `gasTipChanged(a, b)` 의 동작 규약(둘 다 nil→false, 한쪽만 nil→true, 값 같으면 false, 다르면 true)까지 명시.
2. **대상 2** — `RemotesBelowTip`이 캐시된 `AnzeonTipCap` 기준으로 임계값과 비교하고, nil이면 `tx.GasTipCap()`으로 폴백.
3. **통과 조건** — 위 2개 핵심 케이스는 수정 전 코드에서 실패해야 하고(회귀 검출력),
   `go test -race ./eth/gasprice/... ./core/txpool/legacypool/...` 통과.

### 1.2 내가 파악한 근본 원인 (두 곳 모두 정답과 동일하게 식별)

- **버그 A — `AnzeonTipEnv.SetCurrentBlock` 의 갱신 조건이 state root만 검사**
  헤더의 GasTip은 부모 state 기준으로 계산되므로 거버넌스 변경(block N)은 block N+1 헤더에 반영된다.
  N+1이 빈 블록이면 state root가 N과 동일 → 기존 조건 `currentBlock.Root != header.Root` 가 false →
  `currentBlock`이 갱신되지 않아 **stale GasTip**을 계속 사용. 비인가 계정의 effective tip 계산이
  옛 값을 보게 되어 minTip 미달 상태가 지속된다.

- **버그 B — `RemotesBelowTip` 이 `tx.GasTipCap()` 으로 비교**
  비인가 계정의 실제 실행 tip은 진입 시 캐시된 `AnzeonTipCap`(= `block.GasTip()`)이다.
  자기 자신의 `GasTipCap`으로 비교하면, effective tip이 임계값 아래로 떨어진 tx가 드롭되지 않아
  pending pool에 정체된다.

→ 이 인식은 정답 커밋의 메시지에 적힌 원인 분석과 **완전히 일치**한다.

---

## 2. 내 설계 및 구현

### 2.1 `eth/gasprice/anzeon.go`
- `SetCurrentBlock` 갱신 조건에 `gasTipChanged(env.currentBlock.GasTip(), header.GasTip())` OR 절 추가.
- `gasTipChanged(a, b *big.Int) bool` 헬퍼 신설(doc 규약 그대로).

### 2.2 `core/txpool/legacypool/legacypool.go`
- `RemotesBelowTip` 에서 `tx.GetAnzeonTipCap()` 우선, nil이면 `tx.GasTipCap()` 폴백 후 `Cmp(threshold) < 0` 비교.

### 2.3 테스트 (정답에는 없는 부분)
- `eth/gasprice/anzeon_test.go`
  - `TestGasTipChanged` (표 기반 5케이스: both nil / 한쪽 nil ×2 / 값 같음(다른 포인터) / 다름)
  - `TestSetCurrentBlockGasTipChange` (root 동일+GasTip 변경→갱신, root·GasTip 동일→비갱신)
- `core/txpool/legacypool/remotes_below_tip_test.go`
  - `TestRemotesBelowTipUsesAnzeonTipCap` (캐시<임계값이면 GasTipCap이 높아도 드롭 / 캐시≥임계값 유지 / 캐시 nil 시 폴백 드롭·유지)
- 회귀 검출 검증: 두 신규 테스트가 **수정 전 코드에서 실패**, 수정 후 통과함을 git stash로 직접 확인.
- `go test -race ./eth/gasprice/... ./core/txpool/legacypool/...` 통과, `go vet` 클린.

---

## 3. 정답(`#77`)과의 차이

### 3.1 프로덕션 코드: **로직 동일 (functionally identical)**

| 항목 | 내 수정 | 정답 #77 | 차이 |
|---|---|---|---|
| `gasTipChanged` 본문 | nil/nil→false, 한쪽 nil→true, `a.Cmp(b)!=0` | **완전히 동일 (byte-for-byte)** | 없음 |
| `SetCurrentBlock` 조건 | `nil \|\| Root!= \|\| gasTipChanged(...)` (3줄 포맷) | `nil \|\| Root!= \|\| gasTipChanged(...)` (1줄) | 줄바꿈만 다름, 로직 동일 |
| `RemotesBelowTip` | `tip := GetAnzeonTipCap(); if nil → GasTipCap; tip.Cmp(threshold)<0` | 동일 (변수명 `tipcap`) | 변수명만 다름 |
| `gasTipChanged` 위치 | `SetCurrentBlock`과 `SetBaseFee` 사이 | `GetBaseFee` 뒤, `GetAnzeonTipCap` 앞 | 배치만 다름(cosmetic) |
| 주석 | 상세(원인·영향까지 서술) | 간결(2~3줄) | 분량만 다름 |

핵심: **두 수정은 컴파일·실행상 동작이 같다.** 같은 두 함수를, 같은 헬퍼로, 같은 조건/폴백 로직으로 고쳤다.

### 3.2 테스트: **유일한 실질적 차이**

- **정답 #77 은 테스트 파일을 추가하지 않았다.** `git show 98f05c2a0 --name-only` 결과 변경 파일은
  `core/txpool/legacypool/legacypool.go`, `eth/gasprice/anzeon.go` **2개뿐**.
- origin 레포 전체에도 `gasTipChanged` / `RemotesBelowTip` 를 검증하는 테스트는 없다
  (`grep` 결과 `SetCurrentBlock` 매치는 기존 mock 스텁 2건뿐).
- 반면 내 수정은 doc의 "통과 기준 유닛테스트"와 "회귀 검출력" 요구를 충족하는 테스트 3종을 추가했다.

> 즉 doc가 **명시적으로 요구한 테스트**를 기준으로 보면, 내 결과물이 정답 커밋보다 요구사항을 더 충족한다.
> 단, 실제 머지된 "정답"은 테스트 없이 프로덕션 코드만 반영했다는 사실도 그대로 기록한다.

---

## 4. 차이가 발생한 이유

1. **로직이 거의 동일한 이유**: `docs/pr-77.md` 자체가 수정 위치(두 함수), 헬퍼 시그니처(`gasTipChanged(a,b)`),
   동작 규약(nil 처리/포인터 무관 값 비교), 폴백 규칙(`AnzeonTipCap` nil→`GasTipCap`)까지 **사실상 구현 명세 수준**으로
   기술돼 있었다. 정답 설계가 입력에 인코딩돼 있었으므로 독립적으로 작업해도 수렴할 수밖에 없었다.
   → 이번 케이스의 "정답 일치"는 모델의 추론력보다 **입력 명세의 구체성** 덕이 크다(과대 해석 금지).

2. **테스트 유무 차이의 이유 (추정 포함)**:
   - 내 쪽: doc의 §"통과 기준 유닛테스트" / "회귀 검출력" 문구를 작업 산출물에 포함시켜야 할 명시 요구로 해석.
   - 정답 #77: 커밋 메시지·변경 파일상 테스트가 없다(사실). *왜* 없는지는 커밋에 근거가 없어 단정 불가 —
     별도 테스트 PR로 분리했거나, 내부 검증으로 갈음했을 가능성은 **추정**이며 확인된 바 없다.

3. **주석·포맷·변수명 차이의 이유**: 순수 스타일 선택. 내 주석이 더 장황(원인·영향까지 서술)하고,
   정답은 핵심만 2~3줄로 간결. 기능 영향 없음.

---

## 5. 개선 고민 (할루시네이션 배제, 검증된 것만)

### 5.1 내 결과물에서 개선할 점
- **주석 분량 과다**: 내 `SetCurrentBlock`/`RemotesBelowTip` 주석은 정답 대비 길다. 코드 자체가 자명하므로
  정답 수준(2~3줄, "왜"만)으로 줄이는 편이 리뷰·유지보수에 유리하다.
- **테스트 파일 네이밍 일관성**: 정답 레포 컨벤션은 `<파일>_test.go`(예: `anzeon_test.go`). 나도 `anzeon_test.go`는
  맞췄으나 legacypool 쪽은 `remotes_below_tip_test.go` 라는 신규 파일을 만들었다. 기존 `list_test.go`/`legacypool_test.go`에
  케이스를 추가하는 편이 레포 관행과 더 일치한다(신규 파일 최소화).

### 5.2 정답·내 수정 **공통**으로 남아있는, 추가 검토 가치가 있는 지점
- **캐시 신선도 가정**: 두 수정 모두 `tx.GetAnzeonTipCap()`이 진입 시 올바르게 채워져 있다는 전제에 의존한다.
  거버넌스로 GasTip이 *내려간(복원)* 경우, 풀에 이미 있던 비인가 tx의 캐시값이 옛 헤더 GasTip이라면
  `RemotesBelowTip(threshold)`의 threshold가 그 캐시보다 높아질 때만 드롭된다. 캐시 재계산 시점/주체
  (validation의 `SetAnzeonTipCap` 1회 캐싱, `core/txpool/validation.go:337` 부근)에 대한 별도 회귀 테스트가 있으면
  정체 시나리오 전 구간을 덮을 수 있다. — 이는 두 수정 모두에 해당하는 *기존 설계 경계*이며, 본 PR 범위 밖일 수 있음.
- **`SetGasTip` 의 드롭 경로**: `RemotesBelowTip`은 `SetGasTip`에서 tip이 *상승*할 때만 호출된다(`legacypool.go:487`).
  doc의 "GasTip 복원(하향)" 시나리오에서 pending 정체가 풀리는 실제 트리거가 무엇인지(헤더 GasTip 상향 재조정 vs.
  재검증 경로)에 대한 end-to-end 통합 테스트(ChainBench)가 단위 테스트를 보완할 수 있다. — **추정 영역이므로
  단정하지 않음**; 코드만으로는 복원 후 드롭 트리거의 전 경로를 확증하지 못했다.

### 5.3 프로세스 관점
- 입력 명세가 구체적일수록 산출 코드는 정답에 수렴하지만, **"명세에 없는 부분"**(이 케이스의 테스트 동반 여부,
  캐시 재계산 경계)이 진짜 변별점이 된다. 명세 구체성에 안주하지 말고, 명세가 *침묵하는* 인접 불변식
  (캐시 무효화 타이밍 등)을 능동적으로 테스트로 못 박는 것이 품질 우위를 만든다.

---

## 6. 결론

- **원인 분석·근본 수정 로직**: 내 수정 == 정답 #77 (functionally identical). 두 곳 모두 정확히 동일하게 식별·수정.
- **유일한 실질 차이**: 나는 doc가 요구한 회귀 테스트 3종을 추가했고, 정답 커밋은 테스트를 포함하지 않았다(사실 확인됨).
- **나머지 차이**: 주석 분량·포맷·변수명·헬퍼 배치 — 모두 cosmetic, 동작 영향 없음.
- **개선 여지**: (a) 주석 간결화, (b) 테스트를 기존 파일에 통합, (c) 캐시 재계산 경계·복원 시나리오 통합 테스트는
  두 수정 공통의 추가 검증 가치 — 단, 일부는 추정이며 코드로 확증되지 않았음을 명시한다.
