# DIAG-20260617_074336 — Governance gasTip 복원 제안 시 트랜잭션 정체

## 1. Root cause

값 = **유효 minimum tip (effective floor)**.
복사본 = ① `GovValidator` contract slot (`gasTip`) — source of truth
        ② `worker.tip` (`miner/worker.go`, line 271·382)
        ③ `pool.gasTip` (`atomic.Pointer[uint256.Int]`, `core/txpool/legacypool/legacypool.go:237`)
        ④ `Transaction.anzeonTipCap` (`core/types/transaction.go:77`) — per-tx 캐시
        ⑤ `AnzeonTipEnv.currentBlock` / `currentState` (`eth/gasprice/anzeon.go:34-35`).

**깨진 lifecycle edge: produce → consume *at Add-time entry gate*** —
`core/txpool/validation.go:119-120` 에서 `tx.GasTipCapIntCmp(opts.MinTip) < 0` 이면
`ErrUnderpriced` 로 단순 거절한다. `opts.MinTip` 은 `pool.gasTip.Load().ToBig()`
(`core/txpool/legacypool/legacypool.go:660`), 즉 **현재 ‘올라간’ 30000 Gwei**.

따라서 시나리오에서 "27600 으로 복원" 제안 트랜잭션이 `gasTipCap = 27600` 으로
서명되어 들어오면 → `27600 < 30000` → **Add 단계에서 즉시 거절** → pending pool 에
실제로는 진입조차 하지 못한 채 사용자/지갑/RPC 관점에서만 "pending" 으로 남아 반복
재전송한다. 그리고 이를 풀려면 governance 가 27600 으로 복원되어야 하는데,
복원하려면 바로 이 제안 트랜잭션이 포함되어야 하는 **순환 데드락(circular deadlock)** 이다.

`pool.SetGasTip` (legacypool.go:485-494) 의 비대칭성 — `newTip > old` 일 때만
이미-풀에 있는 underpriced 트랜잭션을 drop 하고, `newTip < old` 일 때는 아무 행동도
하지 않는 것 — 은 이 deadlock 의 *결과* 일관성에는 기여하지만 *원인* 은 아니다.
원인은 **시스템 거버넌스 트랜잭션 자체가 자기가 풀려고 하는 floor 에 의해 게이트** 된다는
점이며, 풀이 동적 변경을 사후에 반영하지 않는다는 사실은 사용자가 한 번 거절된 트랜잭션을
**재전송할 수단을 갖지 못한다**는 것을 의미한다. (참고: 거버넌스 제안이 다른 경로로
포함되어 27600 으로 내려가도, 그 직전에 31000 같은 더 큰 tip 으로 풀에 들어와 살아남은
다른 사용자의 트랜잭션들은 그대로 두면 되니까 lowering-side drop 이 없는 것은 정상이지만,
**queue 에서 underpriced 로 거절되어 사라진 트랜잭션을 다시 받아들이는 메커니즘이 없다**.)

**경쟁 가설을 비대칭(올림 정상 / 내림 정체)으로 반증:**

- **(A) `Transaction.anzeonTipCap` 캐시가 stale (PR77 walked example)** — `validation.go:337-340`
  은 `tx.GetAnzeonTipCap() == nil` 일 때만 채워지고 어디에서도 초기화되지 않는다.
  *하지만* 시나리오의 막힌 트랜잭션은 **새로 들어오는** 복원 제안이라 캐시가 비어
  있는 상태에서 처음 채워질 뿐이다. "old tx becomes stale" 시나리오에는 맞지만
  본 증상("새 tx 가 들어가지 않음")의 비대칭(올림은 정상, 내림만 정체)을 *설명하지
  못한다*. 이미 풀에 들어가 있어야 캐시 stale 이 문제 되는데, 본 사건은 풀에
  들어가지조차 못한다.
- **(B) `AnzeonTipEnv.currentBlock` 의 same-Root skip (`anzeon.go:54`)** — `header.Root`
  가 같으면 갱신을 건너뛴다. 빈 블록 시퀀스에서 stale 이 될 수 있는 캐시 결함이지만,
  본 증상에서 영향 받는 것은 *consume* 측(Pending 필터)이고, Add 단계의 거절은
  `pool.gasTip` 만 본다(env 와 무관). 마찬가지로 올림/내림 비대칭을 *설명하지 못함*.
- **(C) `pool.SetGasTip` 의 단방향 drop (legacypool.go:486-494)** — 사실이지만 본 사건의
  *원인* 이 아니라 *2차 증상* 이다. 단방향 drop 은 *이미 풀에 들어가 살아남은* 트랜잭션
  처리에 대한 정책이고, 본 증상의 트랜잭션은 진입조차 못한다. 비대칭은 진입-게이트
  자체(올림 후 27600 tip 제안 → 거절 / 평소 27600 floor 에서 30000 tip 제안 → 통과)로
  완전히 설명된다.

원인을 위 (A) 로 멈추는 것은 PR77 의 walked example 의 *증상* (첫 캐시)에만 머무르는
부분진단이다. 본 진단의 *value의 producer→consumer* 사슬에서 깨진 edge 는 더 상위,
**Add-time 의 정적 minimum-tip 게이트** 다.

## 2. Evidence

### 생산자(producer) 경로 — 거버넌스 값을 pool/worker 까지 전파
- `systemcontracts/solidity/v1/GovValidator.sol:178-184` — `proposeGasTip(_newTip)` 가
  거버넌스 제안을 만든다. 제안이 통과되면 contract slot `gasTip` 이 갱신된다
  (이벤트 `GasTipUpdated`, line 175).
- `miner/worker.go:1201-1206` — `updateGasTipFromContract(state)` 가 `GetGasTip(addr, state)`
  로 컨트랙트 슬롯을 읽어 `setGasTip` 으로 전달.
- `miner/worker.go:377-389` — `setGasTipUnsafe(tip)`: `w.tip` 갱신 후
  `w.eth.TxPool().SetGasTip(tip)` 호출. **방향 무관하게 갱신** (값만 다르면 store).
- `miner/worker.go:313-323` — worker 초기화 시 GovValidator 에서 1회 읽고,
  `chain.SetGasTipUpdater(updateGasTipFromContract)` 로 매 블록 import 마다 갱신
  콜백 등록.
- `miner/worker.go:870-883` — `resultLoop` 에서 직접 블록 sealing 후에도
  `updateGasTipFromContract` 호출.

→ producer 경로는 양방향(올림/내림) 모두 정상 작동한다. `pool.gasTip` 은 거버넌스
값과 같은 방향·시점으로 변한다.

### Add-time entry 게이트 — **깨진 edge 위치**
- `core/txpool/legacypool/legacypool.go:650-668` — `validateTxBasics`:
  `opts.MinTip = pool.gasTip.Load().ToBig()` → `ValidateTransaction` 호출.
- `core/txpool/validation.go:119-120` —
  ```go
  if tx.GasTipCapIntCmp(opts.MinTip) < 0 {
      return fmt.Errorf("%w: gas tip cap %v, minimum needed %v", ErrUnderpriced, ...)
  }
  ```
  **여기서 거절되면 트랜잭션은 풀에 진입하지 않는다.** 이후 거버넌스가 내려간 뒤
  자동 재시도가 없으므로, 사용자가 동일 nonce·동일 tip 으로 재전송해도 결과는 같다.
- `core/txpool/validation.go:122-131` — Anzeon 활성 시 `gasFeeCap >= MinBaseFee + MinTip`
  도 게이트한다. 이중 게이팅.

### 단방향 SetGasTip drop (2차 증상)
- `core/txpool/legacypool/legacypool.go:472-496` — `SetGasTip`:
  `if newTip.Cmp(old) > 0 { drop := pool.all.RemotesBelowTip(tip); ... }`. lowering 시
  drop 루프는 실행되지 않지만, 또한 *반대로 새로 유효해진 큐 트랜잭션을 promote
  하는 로직도 없다*.
- `core/txpool/blobpool/blobpool.go:1018-1085` — blobpool `SetGasTip` 도 동일 비대칭.

### 캐시 라이프사이클 — 경쟁 가설을 위해 명시
- `core/types/transaction.go:77` — `anzeonTipCap atomic.Value`.
- `core/types/transaction.go:412-418` — `SetAnzeonTipCap` 는 `tipCap != nil` 일 때만
  store. **clear/invalidate 메서드 없음** (전체 코드베이스 grep: `SetAnzeonTipCap`
  호출자는 `core/txpool/validation.go:337-340` 단 한 군데).
- `core/txpool/validation.go:337-340` — `tx.GetAnzeonTipCap() == nil` 일 때만 채움.
  즉 한 번 채워진 캐시는 다음 Reset/SetGasTip/Reheap 동안 그대로 살아남는다.
- `eth/gasprice/anzeon.go:50-63` — `SetCurrentBlock`: `env.currentBlock.Root != header.Root`
  일 때만 갱신. 빈 블록 시퀀스(`root` 미변경)에서는 헤더만 새것이라도 env 가 stale.
- `core/txpool/legacypool/legacypool.go:585` — `Pending` 에서만 `SetCurrentBlock(filter.Header)`
  호출. `runReorg`(line 1402-1410) 는 `SetBaseFee` 만 호출하고 `SetCurrentBlock` 은 호출
  하지 않는다.

### Consume 경로 (참고)
- `core/txpool/legacypool/legacypool.go:569-620` — `Pending`: `tx.EffectiveGasTipIntCmp(minTipBig, pool.anzeonTipEnv) < 0` 으로 필터.
- `core/types/transaction.go:388-403` — `EffectiveGasTip`: **캐시된 `anzeonTipCap`
  우선** 사용 (line 398-401), nil 일 때만 env 에서 lazy 조회.

### 그래프 edge 요약 (cks 인용)
- `setGasTipUnsafe` 호출처: `miner/worker.go:1201-1206 (updateGasTipFromContract)`,
  `miner/miner.go:213-219`. (`find_callers SetGasTip`)
- `SetAnzeonTipCap` 호출처: `core/txpool/validation.go:241-343` 한 곳 + 테스트.
  (`find_callers SetAnzeonTipCap` → 단일 producer)
- `updateGasTipFromContract` 호출처: `miner/worker.go:260-332 (newWorker init)`,
  `miner/worker.go:819-894 (resultLoop)`, `miner/miner.go:98-111`.

## 3. Affected sites

수정이 필요한 지점들 (`find_callers`/`impact_analysis` 기반 write-site enumeration):

1. **`core/txpool/legacypool/legacypool.go:472-496` (`LegacyPool.SetGasTip`)** —
   lowering 시 drop 로직 외에, **queue 에 있던/거절된 트랜잭션을 재평가하여 promote
   할 수 있는 메커니즘이 필요**. 또한 lowering 시 `pool.priced.Reheap()` 호출 검토.
2. **`core/txpool/blobpool/blobpool.go:1018-1085` (`BlobPool.SetGasTip`)** — 동일한
   비대칭 정책. 일관성을 위해 같이 다룬다.
3. **`core/txpool/validation.go:119-131` (`ValidateTransaction` MinTip 게이트)** —
   거버넌스/시스템-컨트랙트 호출 트랜잭션(예: `to == GovValidator.address`) 에 대한
   **예외 화이트리스트** 또는 *locals 와 유사한 면제* 가 필요. 그렇지 않으면 순환
   데드락 자체를 풀 수 없다. (`validateTxBasics:662-664` 에서 `local` 인 경우
   `opts.MinTip = 0` 으로 이미 비슷한 면제가 있다.)
4. **`core/txpool/legacypool/legacypool.go:650-669` (`validateTxBasics`)** — 위 (3) 과
   짝지어 governance-bound tx 에 대해 `opts.MinTip` 을 0 또는 더 낮은 값으로 우회.
5. **`core/types/transaction.go:77,412-426` (`anzeonTipCap` 캐시)** + **`SetGasTip`
   경로** — 캐시 invalidator 추가 (`tx.ClearAnzeonTipCap()`). `SetGasTip` 이 변경되었을
   때, `pool.all`/`pool.pending`/`pool.queue` 의 모든 tx 에 대해 캐시를 클리어해야
   다음 Pending 호출에서 새 floor 가 반영된다. **invalidator 0 인 캐시 = 유력 용의자**
   원칙. 본 사건의 1차 원인은 아니지만, 향후 stale-cache 결함을 막기 위해 함께 처리해야 한다.
6. **`eth/gasprice/anzeon.go:50-63` (`SetCurrentBlock` same-Root skip)** + 호출처
   `core/txpool/legacypool/legacypool.go:585` — same-Root skip 정책을 재검토 (빈 블록에서
   header.Number 가 증가하면 비록 root 가 같아도 header 자체는 새것; `header.GasTip()` 값을
   따로 보거나 무조건 갱신).
7. **`miner/worker.go:1201-1225` (`updateGasTipFromContract`)** — 콜백 등록 (`SetGasTipUpdater`)
   이 `worker.chain` 으로 향하는데, `chain.SetGasTipUpdater` 가 실제로 어디서 fire 되는지
   확인 필요 (검색 결과 `consensus/wbft/engine/engine.go:622-645` 부근 호출). 변경이 매
   블록 import 마다 호출되도록 보장.
8. **테스트**: `core/txpool/legacypool/legacypool_test.go:1458-1818` 의 `SetGasTip`
   관련 테스트 시리즈 (TestPoolUnderpricing 등) — lowering-then-restore 시나리오와
   governance-tx 면제 정책에 대한 새 테스트 추가.

(범위 외이지만 영향을 받음: `core/types/receipt.go:339-373` 의 `anzeonTipEnv` 도
동일한 패턴을 갖고 있어, 만약 type#2 와 같은 fix 라면 receipt 측 env 도 정합해야
한다.)

## 4. Confidence

**Medium-High**.

근거가 강한 부분:
- 코드 인용으로 Add-time 게이트(`validation.go:119-120`) 와 단방향
  `SetGasTip`(legacypool.go:486-494) 가 정확히 발견됨.
- 시나리오의 **올림/내림 비대칭** 이 Add-time 거절 가설로만 깔끔하게 설명됨
  (경쟁 가설들은 이 비대칭을 설명 못함).
- 거버넌스 → worker → pool 의 producer 사슬은 양방향 정상 작동함을 코드로 확인.

확신을 더 올리려면 (raise the confidence):
- 실제 재현 시 노드 로그에서 `ErrUnderpriced` 와
  `"gas tip cap %v, minimum needed %v"` 메시지가 찍히는지 확인 (validation.go:120).
  찍히면 root cause 확정. 안 찍히고 트랜잭션이 풀에는 들어갔는데 mining 만 안 된다면
  consume-side (anzeonTipCap stale or Pending filter) 로 가설 이동.
- 실제 제출되는 restore 제안 tx 의 `gasTipCap` 값을 확인 — 27600 으로 서명되어 들어오는지
  (사용자가 "복원할 값" 으로 서명) vs 30000 이상으로 서명되어 들어오는지 (현재 풀 floor 에
  맞춰 서명). 전자라면 본 진단 확정, 후자라면 가설 (A) 의 anzeonTipCap stale 로 이동.
- `pool.gasTip.Load()` 의 실시간 값을 RPC 또는 metric (`pooltipGauge`) 으로 확인하여
  contract 값과 동기화되는 시점을 확정.

## 5. Suggested direction

핵심 fix 는 **두 층** 으로 가야 한다. ① **Add-time entry gate 의 governance 면제** —
governance 시스템 컨트랙트(`GovValidator`, `GovCouncil`, `GovMinter` 등) 로 향하는
트랜잭션은 `validateTxBasics` / `ValidateTransaction` 에서 `MinTip` 게이트와
`MinBaseFee + MinTip` 게이트를 우회해야 한다 (현재 `local` 트랜잭션 면제와 같은 메커니즘을
시스템-컨트랙트 호출에도 적용). 이렇게 해야 "tip floor 을 풀려고 하는 제안 자체가
floor 때문에 막히는" 순환 데드락이 깨진다. ② **풀 측 동적 변경 반영** —
`SetGasTip` 이 lowering 일 때도 `pool.all`/`pool.queue` 를 순회하며 `Transaction.anzeonTipCap`
캐시를 invalidate 하고, 새 floor 기준으로 queue→pending re-promotion 을 트리거해야 한다
(현재는 invalidator 가 0 인 캐시이고, queue→pending demote/promote 도 reset 경로에만
달려 있어 동적 변경에 반응하지 않음). `AnzeonTipEnv.SetCurrentBlock` 의 same-Root skip
정책도 같이 검토 (빈 블록·governance-only 블록 시퀀스에서 stale 위험). 단, 위 (5)/(6) 의
캐시 invalidator 와 env 갱신 정책은 본 증상의 **1차 원인이 아니라 동적-변경 반영
인프라의 동반 결함** 이라는 점은 분명히 하고 진행한다 — `/plan` 단계에서 우선순위는
(1) governance 면제(데드락 해소) → (2) lowering 시 invalidator/promotion 인프라.
