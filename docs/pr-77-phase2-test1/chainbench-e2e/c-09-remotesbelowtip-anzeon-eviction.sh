#!/usr/bin/env bash
# ---chainbench-meta---
# id: RT-C-09
# name: gasTip floor 인상 시 RemotesBelowTip이 unauthorized 계정의 유효 tip(AnzeonTipCap) 대신 raw tipCap으로 판정해 채굴 불가 tx를 evict하지 못함 (PR-77 2차 결함)
# category: regression/c-anzeon
# tags: [anzeon, gas, txpool, pr77, remotesbelowtip, eviction]
# estimated_seconds: 90
# preconditions:
#   chain_running: true
#   python_packages: [eth-account, requests, eth-utils]
# depends_on: []
# ---end-meta---
# Test: regression/c-anzeon/c-09-remotesbelowtip-anzeon-eviction
# RT-C-09 — PR-77 SECOND defect (legacypool RemotesBelowTip) end-to-end reproduction.
#
# Expert fix commit 98f05c2a0 changed lookup.RemotesBelowTip to compare each remote
# tx's EFFECTIVE Anzeon tip (tx.GetAnzeonTipCap(), cached at pool entry as the block
# GasTip for unauthorized accounts) against the new tip floor — instead of the raw
# tx.GasTipCap(). Reason: for an unauthorized account the effective tip actually used
# during execution is the block GasTip, not the raw maxPriorityFeePerGas. When the
# governance gas-tip floor is RAISED above that effective tip, the tx becomes unminable
# and must be evicted; the buggy code keeps it (raw cap is still above the floor),
# leaving a permanently-unminable tx clogging the pool / the account's nonce queue.
#
# Reproduction:
#   1. Lower the floor to L (1000 Gwei) and let it settle.
#   2. From an UNAUTHORIZED account submit a FUTURE-nonce tx (queued, never mined) with
#      a HIGH raw tip (50000 Gwei). Its cached effective AnzeonTipCap = block GasTip = L.
#   3. RAISE the floor to H (30000 Gwei) via governance -> SetGasTip(H) -> RemotesBelowTip(H).
#         buggy : raw 50000 >= 30000  -> NOT evicted (tx lingers)   <-- the defect
#         fixed : effective 1000 < 30000 -> evicted
#   4. Oracle: the tx MUST be evicted (effective tip is below the new floor).
#         buggy -> still in pool  -> assertion FAILS -> RED
#         fixed -> gone           -> PASS (GREEN)
set -euo pipefail

source "$(dirname "$0")/../lib/common.sh"

test_start "regression/c-anzeon/c-09-remotesbelowtip-anzeon-eviction"
check_env || { test_result; exit 1; }

unlock_all_validators
RPC="http://127.0.0.1:8501"

L_TIP="1000000000000"    # 1000 Gwei  — low floor (effective tip for unauthorized)
H_TIP="30000000000000"   # 30000 Gwei — high floor (raised above effective)
RAW_TIP="50000000000000" # 50000 Gwei — tx1 raw maxPriorityFeePerGas (ABOVE H -> buggy keeps it)

gov_set_gastip() {  # $1 = new wei value
  local newv="$1"
  local sel; sel=$(selector "proposeGasTip(uint256)")
  local amt; amt=$(pad_uint256 "$newv" | sed 's/^0x//')
  local rcpt
  rcpt=$(gov_full_flow "$GOV_VALIDATOR" "${sel}${amt}" "$VALIDATOR_1_ADDR" "$VALIDATOR_2_ADDR") || return 1
  printf '%s' "$rcpt" | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))"
}

# --- 1. lower the floor to L and settle ---------------------------------------
st=$(gov_set_gastip "$L_TIP"); assert_eq "$st" "0x1" "governance LOWER gasTip -> $L_TIP succeeded"
blk=$(block_number "1"); wait_for_block "1" "$(( blk + 3 ))" 30 >/dev/null || true
ht=$(get_header_gas_tip "1"); printf '[INFO]  header.GasTip after lower = %s wei (want %s)\n' "$ht" "$L_TIP" >&2
assert_eq "$ht" "$L_TIP" "floor lowered and reflected in header (effective tip baseline for unauthorized)"

# --- 2. unauthorized account submits a FUTURE-nonce (queued) tx ---------------
# Future nonce => queued, never mined => it stays in the pool so we can observe whether
# the floor-raise evicts it. Cached effective AnzeonTipCap = block GasTip (L) at entry.
tx_hash=$(python3 <<PYEOF
import json, requests
from eth_account import Account
pk="${TEST_ACC_A_PK}"; url="${RPC}"
acct=Account.from_key(pk)
pend=int(requests.post(url,json={"jsonrpc":"2.0","method":"eth_getTransactionCount","params":[acct.address,"pending"],"id":1}).json()["result"],16)
cid=int(requests.post(url,json={"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}).json()["result"],16)
bf=int(requests.post(url,json={"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",False],"id":1}).json()["result"]["baseFeePerGas"],16)
future_nonce=pend+5   # deliberate gap => queued
raw=${RAW_TIP}
tx={"nonce":future_nonce,"to":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8","value":1,"gas":21000,
    "chainId":cid,"maxFeePerGas":bf*2+raw,"maxPriorityFeePerGas":raw,"type":2}
s=acct.sign_transaction(tx)
r=requests.post(url,json={"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":[s.raw_transaction.to_0x_hex()],"id":1}).json()
print(r.get("result","ERR:"+json.dumps(r.get("error",{}))))
PYEOF
)
printf '[INFO]  queued tx1 (high raw tip, unauthorized): %s\n' "$tx_hash" >&2
assert_contains "$tx_hash" "0x" "future-nonce tx1 accepted into pool (queued)"

# confirm it is actually in the pool before the raise
present_before=$(python3 -c "
import requests,json
r=requests.post('${RPC}',json={'jsonrpc':'2.0','method':'eth_getTransactionByHash','params':['${tx_hash}'],'id':1}).json().get('result')
print('true' if r is not None else 'false')
")
assert_eq "$present_before" "true" "tx1 present in pool before floor raise"

# --- 3. RAISE the floor to H -> SetGasTip(H) -> RemotesBelowTip(H) -------------
st=$(gov_set_gastip "$H_TIP"); assert_eq "$st" "0x1" "governance RAISE gasTip -> $H_TIP succeeded"
ht=$(get_header_gas_tip "1"); printf '[INFO]  header.GasTip after raise = %s wei (want %s)\n' "$ht" "$H_TIP" >&2
# give SetGasTip/RemotesBelowTip a few blocks to apply
blk=$(block_number "1"); wait_for_block "1" "$(( blk + 4 ))" 30 >/dev/null || true

# --- 4. ORACLE: tx1 must be evicted (effective tip L < new floor H) -----------
present_after=$(python3 -c "
import requests,json
r=requests.post('${RPC}',json={'jsonrpc':'2.0','method':'eth_getTransactionByHash','params':['${tx_hash}'],'id':1}).json().get('result')
print('true' if r is not None else 'false')
")
printf '[INFO]  tx1 still in pool after raise? %s  (effective tip %s < new floor %s => should be EVICTED)\n' \
  "$present_after" "$L_TIP" "$H_TIP" >&2

evicted=$( [[ "$present_after" == "false" ]] && echo true || echo false )
assert_true "$evicted" "RemotesBelowTip evicts the unminable unauthorized tx using its effective AnzeonTipCap ($L_TIP < $H_TIP) — PR-77 2nd-defect oracle"

test_result
