# c-09 RemotesBelowTip reproduction — observed evidence (buggy parent 0bf2f4d1b)

## Scenario (chainbench e2e, regression profile, 4 validators)
1. governance LOWERED gasTip floor 27600 -> 1000 Gwei (settled).
2. UNAUTHORIZED (non-validator) account submitted a tx (raw maxPriorityFeePerGas = 50000 Gwei, well ABOVE later floor) that stays resident in the txpool.
3. governance RAISED gasTip floor 1000 -> 30000 Gwei.

## Observed (RED)
- After the raise, the tx is STILL present in the pool (eth_getTransactionByHash returns it), even though under the new floor its EFFECTIVE tip is below the floor (unminable).
- For an unauthorized account the effective execution tip is the block GasTip (1000 Gwei), NOT the raw maxPriorityFeePerGas (50000).

## Node log (node1) gasTip timeline
INFO [06-22|20:42:36.162] Legacy pool tip threshold updated        tip=27,600,000,000,000
DEBUG[06-22|20:42:36.162] Updated gasTip from GovValidator contract newTip=27,600,000,000,000
INFO [06-22|20:43:08.028] Legacy pool tip threshold updated        tip=1,000,000,000,000
DEBUG[06-22|20:43:08.032] Updated gasTip from GovValidator contract newTip=1,000,000,000,000
INFO [06-22|20:43:15.084] Legacy pool tip threshold updated        tip=30,000,000,000,000
DEBUG[06-22|20:43:15.088] Updated gasTip from GovValidator contract newTip=30,000,000,000,000

## Test + result
- test: tests/regression/c-anzeon/c-09-remotesbelowtip-anzeon-eviction.sh
- result: FAIL on buggy parent (tx not evicted).
