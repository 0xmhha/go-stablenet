// PR-77 second-defect regression test (RemotesBelowTip eviction).
package legacypool

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestRemotesBelowTip_AnzeonEffectiveTip pins PR-77's second defect.
//
// For an unauthorized (non-validator) account the EFFECTIVE tip used at
// execution is the block's GasTip floor, cached on the tx at pool entry
// (anzeonTipCap) — NOT the raw maxPriorityFeePerGas. When the gas-tip floor
// RISES above that effective tip, the tx becomes permanently unminable and the
// pool must evict it on SetGasTip -> RemotesBelowTip.
//
// The buggy RemotesBelowTip compares the RAW tip (tx.GasTipCapIntCmp): a tx with
// raw 50000 but effective 1000 "passes" 50000 < 30000 == false and lingers
// forever. The fix compares the effective Anzeon tip (EffectiveGasTipIntCmp), so
// 1000 < 30000 and the tx is evicted.
//
// RED on buggy parent 0bf2f4d1b, GREEN once RemotesBelowTip is Anzeon-aware.
func TestRemotesBelowTip_AnzeonEffectiveTip(t *testing.T) {
	t.Parallel()
	pool, key := setupPool()
	defer pool.Close()

	// baseFee must be non-nil for EffectiveGasTip to engage the Anzeon path.
	pool.anzeonTipEnv.SetBaseFee(big.NewInt(1))

	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, big.NewInt(1_000_000_000_000_000))

	// Unauthorized account tx: raw priority fee 50000 (ABOVE the future floor),
	// but its effective Anzeon tip is the old block GasTip 1000, cached at entry.
	tx := dynamicFeeTx(0, 100000, big.NewInt(1_000_000), big.NewInt(50000), key)
	tx.SetAnzeonTipCap(big.NewInt(1000)) // effective tip locked at the old (low) floor
	if errs := pool.addRemotesSync([]*types.Transaction{tx}); len(errs) > 0 && errs[0] != nil {
		t.Fatalf("failed to add tx: %v", errs[0])
	}
	if pool.Get(tx.Hash()) == nil {
		t.Fatalf("precondition: tx must be in the pool before the floor raise")
	}

	// Governance raises the floor to 30000 -> SetGasTip -> RemotesBelowTip(30000).
	pool.SetGasTip(big.NewInt(30000))

	// Oracle: effective tip 1000 < new floor 30000 => the tx MUST be evicted.
	if pool.Get(tx.Hash()) != nil {
		t.Fatalf("RED: unminable unauthorized tx (effective tip 1000 < floor 30000) was NOT evicted; " +
			"RemotesBelowTip compared the raw tip (50000) instead of the effective Anzeon tip")
	}
}
