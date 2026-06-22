#!/usr/bin/env bash
# ---chainbench-meta---
# id: RT-C-08
# name: 거버넌스 gasTip 변경 직후 idle(빈 블록) 구간에서 비-validator 정상 tx가 부당 거부됨 (PR-77 e2e 재현)
# category: regression/c-anzeon
# tags: [anzeon, gas, governance, pr77, staleness]
# estimated_seconds: 60
# preconditions:
#   chain_running: true
#   python_packages: [eth-account, requests, eth-utils]
# depends_on: []
# ---end-meta---
# Test: regression/c-anzeon/c-08-gastip-change-idle-stale-reject
# RT-C-08 — PR-77 end-to-end reproduction.
#
# Symptom (ticket STABLE-0005): right after governance changes the gas priority-tip
# policy, a non-validator account's NORMAL transaction is unfairly rejected for a
# while during the idle (empty-block) window, then self-heals.
#
# Mechanism under test: AnzeonTipEnv.SetCurrentBlock gates the header swap on STATE
# ROOT change. After a governance gasTip change at block N, empty blocks N+1, N+2 …
# share N's state root, so env.currentBlock stays pinned to block N whose header
# carries the PRE-change (stale, higher) GasTip. GetAnzeonTipCap then forces the
# unauthorized sender's effective tip to that stale-high value; a normal tx whose
# maxFeePerGas only covers the NEW (lower) policy cannot afford the stale tip and is
# excluded from blocks (never mined) until a state-changing block clears the staleness.
#
# RED (buggy parent 0bf2f4d1b): the tx is NOT mined within the window -> FAIL.
# GREEN (fixed): env.currentBlock tracks every head -> effective tip = new policy ->
#                the tx is mined -> PASS.
set -euo pipefail

source "$(dirname "$0")/../lib/common.sh"

test_start "regression/c-anzeon/c-08-gastip-change-idle-stale-reject"
check_env || { test_result; exit 1; }

unlock_all_validators

RPC="http://127.0.0.1:8501"

# --- 1. baseline policy -------------------------------------------------------
old_tip=$(get_header_gas_tip "1")
printf '[INFO]  baseline header.GasTip = %s wei\n' "$old_tip" >&2
assert_gt "$old_tip" "0" "baseline gasTip is set"

# New (LOWER) policy: 1000 Gwei = 1000000000000 wei. Must be < old_tip so that the
# stale value (old_tip) is the HIGHER one — the direction that REJECTS normal txs.
NEW_TIP="1000000000000"
assert_true "$( [[ $old_tip -gt $NEW_TIP ]] && echo true || echo false )" \
  "baseline gasTip ($old_tip) > new policy ($NEW_TIP) — lowering direction (stale=high=reject)"

# --- 2. governance LOWERS the gasTip policy -----------------------------------
propose_sel=$(selector "proposeGasTip(uint256)")
amt_padded=$(pad_uint256 "$NEW_TIP" | sed 's/^0x//')
propose_data="${propose_sel}${amt_padded}"

receipt=$(gov_full_flow "$GOV_VALIDATOR" "$propose_data" "$VALIDATOR_1_ADDR" "$VALIDATOR_2_ADDR") || {
  _assert_fail "governance proposeGasTip flow failed"; test_result; exit 1
}
exec_status=$(printf '%s' "$receipt" | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")
assert_eq "$exec_status" "0x1" "governance executeProposal(proposeGasTip) succeeded"

# --- 3. enter the idle (empty-block) window -----------------------------------
# After the gov-change block N, send NO transactions so blocks N+1, N+2 … are empty
# and share N's state root. The buggy SetCurrentBlock leaves env.currentBlock pinned
# to N (header GasTip = old, stale-high). Wait a few empty blocks to be safely inside
# the window.
printf '[INFO]  idling for empty blocks so env.currentBlock goes stale ...\n' >&2
start_blk=$(block_number "1")
wait_for_block "1" "$(( start_blk + 4 ))" 30 >/dev/null || true
mid_blk=$(block_number "1")
printf '[INFO]  produced empty blocks %s -> %s (no txs, same state root)\n' "$start_blk" "$mid_blk" >&2

# --- 4. non-validator NORMAL tx, affordable under the NEW policy only ----------
# maxFeePerGas covers baseFee + a few thousand Gwei (>> new 1000 Gwei policy) but is
# far below baseFee + old 27600 Gwei. Under the NEW policy the unauthorized sender's
# forced effective tip is 1000 Gwei -> affordable -> minable. Under the STALE policy
# the forced effective tip is old_tip (27600 Gwei) -> NOT affordable -> excluded.
tx_hash=$(python3 <<PYEOF
import json, requests
from eth_account import Account
pk  = "${TEST_ACC_A_PK}"
url = "${RPC}"
acct = Account.from_key(pk)
nonce = int(requests.post(url, json={"jsonrpc":"2.0","method":"eth_getTransactionCount","params":[acct.address,"pending"],"id":1}).json()["result"], 16)
chain_id = int(requests.post(url, json={"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}).json()["result"], 16)
base_fee = int(requests.post(url, json={"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",False],"id":1}).json()["result"]["baseFeePerGas"], 16)
new_tip   = ${NEW_TIP}            # 1000 Gwei
prio      = 3000000000000         # 3000 Gwei  (>> new policy, << old 27600)
# maxFee covers baseFee + ~3000 Gwei: affordable for new(1000) policy, NOT for stale(27600).
max_fee   = base_fee * 2 + prio
tx = {"nonce": nonce, "to": "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "value": 1, "gas": 21000,
      "chainId": chain_id, "maxFeePerGas": max_fee, "maxPriorityFeePerGas": prio, "type": 2}
signed = acct.sign_transaction(tx)
resp = requests.post(url, json={"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":[signed.raw_transaction.to_0x_hex()],"id":1}).json()
print(resp.get("result", "ERR:" + json.dumps(resp.get("error", {}))))
PYEOF
)
printf '[INFO]  submit result: %s\n' "$tx_hash" >&2
assert_contains "$tx_hash" "0x" "non-validator normal tx accepted into pool (admission MinTip uses fresh pool.gasTip)"

# --- 5. the acceptance oracle: it MUST be mined under the new (lower) policy ----
# On the buggy parent the stale-high effective tip keeps it out of every block during
# the idle window -> no receipt -> RED. On the fixed code it is mined -> GREEN.
printf '[INFO]  waiting up to 40s for the tx to be MINED (no extra txs sent) ...\n' >&2
receipt2=$(wait_tx_receipt_full "1" "$tx_hash" 40 2>/dev/null || echo "")
mined=$( [[ -n "$receipt2" ]] && printf '%s' "$receipt2" | grep -q "blockNumber" && echo true || echo false )

if [[ "$mined" == "true" ]]; then
  eff=$(printf '%s' "$receipt2" | python3 -c "import sys,json;print(int(json.load(sys.stdin).get('effectiveGasPrice','0x0'),16))" 2>/dev/null || echo 0)
  printf '[INFO]  MINED. effectiveGasPrice=%s wei\n' "$eff" >&2
fi
assert_true "$mined" "non-validator normal tx is MINED under the new gasTip policy during the idle window (PR-77 acceptance oracle)"

test_result
