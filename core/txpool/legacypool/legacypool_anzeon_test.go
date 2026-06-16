// Copyright 2025 The go-stablenet Authors
// This file is part of the go-stablenet library.
//
// The go-stablenet library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-stablenet library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-stablenet library. If not, see <http://www.gnu.org/licenses/>.

package legacypool

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// makeWBFTHeaderAnzeon returns a Header with WBFTExtra encoding the given
// gasTip, root, and number. Difficulty is set to WBFTDefaultDifficulty.
func makeWBFTHeaderAnzeon(number int64, root common.Hash, gasTip *big.Int) *types.Header {
	extra := &types.WBFTExtra{GasTip: new(big.Int).Set(gasTip)}
	extraBytes, err := rlp.EncodeToBytes(extra)
	if err != nil {
		panic("makeWBFTHeaderAnzeon: " + err.Error())
	}
	return &types.Header{
		Number:     big.NewInt(number),
		Root:       root,
		Extra:      extraBytes,
		Difficulty: types.WBFTDefaultDifficulty,
		BaseFee:    new(big.Int).SetUint64(params.MinBaseFee),
		GasLimit:   10_000_000,
		Time:       uint64(number),
	}
}

// anzeonTestBlockChain extends testBlockChain with a swappable current head
// so we can simulate chain advances with different WBFT headers.
type anzeonTestBlockChain struct {
	testBlockChain
	head *types.Header
}

func (bc *anzeonTestBlockChain) CurrentBlock() *types.Header {
	return bc.head
}

// newAnzeonPool creates a LegacyPool backed by a chain with the WBFT config and
// an in-memory state where no address is authorized (IsAuthorized == false).
// The initial head has a WBFTExtra.GasTip equal to initialGasTip.
func newAnzeonPool(t *testing.T, initialGasTip *big.Int) (*LegacyPool, *state.StateDB, *anzeonTestBlockChain) {
	t.Helper()

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	genesisRoot := types.EmptyRootHash
	head := makeWBFTHeaderAnzeon(0, genesisRoot, initialGasTip)

	bc := &anzeonTestBlockChain{
		testBlockChain: testBlockChain{
			config:        params.TestWBFTChainConfig,
			statedb:       statedb,
			chainHeadFeed: new(event.Feed),
		},
		head: head,
	}
	bc.testBlockChain.gasLimit.Store(10_000_000)
	bc.testBlockChain.baseFee = new(big.Int).SetUint64(params.MinBaseFee)

	pool := New(testTxPoolConfig, bc)
	if err := pool.Init(uint64(initialGasTip.Int64()), head, newReserver()); err != nil {
		t.Fatalf("pool.Init: %v", err)
	}
	<-pool.initDoneCh
	return pool, statedb, bc
}

// assertRemoteTipCapsCleared is the §5.2b invariant assertion: after any
// SetGasTip call, no remote tx in the pool must carry a cached Anzeon tip.
func assertRemoteTipCapsCleared(t *testing.T, pool *LegacyPool) {
	t.Helper()
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	pool.all.Range(func(hash common.Hash, tx *types.Transaction, _ bool) bool {
		if got := tx.GetAnzeonTipCap(); got != nil {
			t.Errorf("remote tx %s still carries cached Anzeon tip %v after SetGasTip",
				hash.Hex(), got)
		}
		return true
	}, false, true)
}

// TestAnzeonGasTipRestoration reproduces PR-77 (LOCAL-20260616_031104):
// a governance gasTip restoration (30000 → 27600 Gwei equivalent) must not
// leave the proposing tx stuck in the pending pool.
//
// The test covers three acceptance criteria:
//
//	AC1: restoration tx is in Pending() after the tip cycle.
//	AC2: a tx with GasTipCap < new threshold is NOT in Pending() after tip increase.
//	AC3: empty block (same root, different header.GasTip) drives SetCurrentBlock
//	     refresh and GetAnzeonTipCap returns the new tip.
func TestAnzeonGasTipRestoration(t *testing.T) {
	t.Parallel()

	// We use small numbers (in units matching pool tip threshold).
	// The WBFT config uses InitialGasTip (27600 Gwei), which is large; to keep
	// the test independent of those production constants we drive the pool with
	// numeric gasTip values that satisfy:
	//   tip27600 < tip30000  (restoration cycle: 27600 → 30000 → 27600)
	//
	// For simplicity, use GWei-scale values that match a realistic deployment.
	var (
		tip27600 = new(big.Int).SetUint64(27600) // "27600" unit in this test
		tip30000 = new(big.Int).SetUint64(30000)
	)

	// ------------------------------------------------------------------ AC1
	t.Run("AC1_restoration_tx_in_pending_after_tip_cycle", func(t *testing.T) {
		t.Parallel()

		pool, statedb, bc := newAnzeonPool(t, tip27600)
		defer pool.Close()

		// Remote sender (will be "unauthorized" — state has no authorized accounts).
		remoteKey, _ := crypto.GenerateKey()
		remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)
		statedb.AddBalance(remoteAddr, uint256.MustFromBig(new(big.Int).SetUint64(1e18)))

		// Build a dynamic fee tx whose GasTipCap is tip30000 (≥ new threshold
		// after the increase) and GasFeeCap is large enough for any baseFee.
		signer := types.LatestSignerForChainID(params.TestWBFTChainConfig.ChainID)
		gasFeeCap := new(big.Int).Add(
			new(big.Int).SetUint64(params.MinBaseFee),
			tip30000,
		)
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   params.TestWBFTChainConfig.ChainID,
			Nonce:     0,
			GasTipCap: tip30000,
			GasFeeCap: gasFeeCap,
			Gas:       21000,
			To:        &common.Address{},
			Value:     big.NewInt(0),
		}), signer, remoteKey)
		if err != nil {
			t.Fatalf("SignTx: %v", err)
		}

		// Pool gasTip is tip27600; tx.GasTipCap=tip30000 so it clears the threshold.
		// During validation, anzeonTipEnv.GetAnzeonTipCap(tx) returns
		// env.currentBlock.GasTip() = tip27600 for this unauthorized sender.
		// The tx's EffectiveGasTip (tip27600) >= pool.gasTip (tip27600). OK to add.
		if err := pool.addRemoteSync(tx); err != nil {
			t.Fatalf("addRemoteSync: %v", err)
		}

		// Step 1: governance increases tip to 30000.
		// Under the old bug, the tx's EffectiveGasTip (from stale cache = tip27600)
		// would be < tip30000, and the tx would be dropped by RemotesBelowTip
		// (but tx.GasTipCap=30000 >= 30000, so it survives that check).
		// The restoration step is what causes the bug, not the increase.
		pool.SetGasTip(tip30000)
		// §5.2b invariant: cache must be cleared.
		assertRemoteTipCapsCleared(t, pool)
		// tx survives increase (tx.GasTipCap=30000 is not below 30000).
		if pool.Get(tx.Hash()) == nil {
			t.Fatal("tx should still be in pool after tip increase (GasTipCap >= new threshold)")
		}

		// Step 2: governance restores tip to 27600.
		// Pool now transitions: head has new root (state changed due to gov tx).
		newRoot := common.HexToHash("0xbeef")
		newHead := makeWBFTHeaderAnzeon(1, newRoot, tip27600)
		bc.head = newHead

		pool.SetGasTip(tip27600)
		// §5.2b invariant: cache cleared again.
		assertRemoteTipCapsCleared(t, pool)

		// Now drive pool.reset so anzeonTipEnv gets the new head.
		<-pool.requestReset(nil, newHead)

		// Build the Pending filter with the new header and minTip=tip27600.
		filter := txpool.PendingFilter{
			MinTip:  uint256.MustFromBig(tip27600),
			BaseFee: uint256.MustFromBig(new(big.Int).SetUint64(params.MinBaseFee)),
			Header:  newHead,
		}
		pending := pool.Pending(filter)
		found := false
		for _, txs := range pending {
			for _, lazy := range txs {
				if lazy.Hash == tx.Hash() {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("AC1 FAIL: restoration tx is NOT in Pending() after gasTip restoration; bug reproduced")
		}
	})

	// ------------------------------------------------------------------ AC2
	t.Run("AC2_underpriced_tx_not_in_pending_after_tip_increase", func(t *testing.T) {
		t.Parallel()

		pool, statedb, _ := newAnzeonPool(t, tip27600)
		defer pool.Close()

		remoteKey, _ := crypto.GenerateKey()
		remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)
		statedb.AddBalance(remoteAddr, uint256.MustFromBig(new(big.Int).SetUint64(1e18)))

		signer := types.LatestSignerForChainID(params.TestWBFTChainConfig.ChainID)
		// tx with GasTipCap = tip27600 (exactly at current threshold).
		gasFeeCap := new(big.Int).Add(
			new(big.Int).SetUint64(params.MinBaseFee),
			tip27600,
		)
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   params.TestWBFTChainConfig.ChainID,
			Nonce:     0,
			GasTipCap: tip27600,
			GasFeeCap: gasFeeCap,
			Gas:       21000,
			To:        &common.Address{},
			Value:     big.NewInt(0),
		}), signer, remoteKey)
		if err != nil {
			t.Fatalf("SignTx: %v", err)
		}

		if err := pool.addRemoteSync(tx); err != nil {
			t.Fatalf("addRemoteSync: %v", err)
		}

		// Increase tip to 30000; tx.GasTipCap(=27600) < 30000 → evicted by RemotesBelowTip.
		pool.SetGasTip(tip30000)

		if pool.Get(tx.Hash()) != nil {
			t.Fatal("AC2 FAIL: underpriced tx still in pool after tip increase; it should have been evicted")
		}

		// Confirm it is not in Pending either.
		filter := txpool.PendingFilter{
			MinTip:  uint256.MustFromBig(tip30000),
			BaseFee: uint256.MustFromBig(new(big.Int).SetUint64(params.MinBaseFee)),
		}
		pending := pool.Pending(filter)
		for _, txs := range pending {
			for _, lazy := range txs {
				if lazy.Hash == tx.Hash() {
					t.Fatal("AC2 FAIL: evicted tx still visible in Pending()")
				}
			}
		}
	})

	// ------------------------------------------------------------------ AC3
	t.Run("AC3_empty_block_same_root_different_gasTip_refreshes_env", func(t *testing.T) {
		t.Parallel()

		pool, statedb, _ := newAnzeonPool(t, tip30000)
		defer pool.Close()

		// Unauthorized sender key.
		remoteKey, _ := crypto.GenerateKey()
		remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)
		statedb.AddBalance(remoteAddr, uint256.MustFromBig(new(big.Int).SetUint64(1e18)))

		// Create a DynamicFeeTx signed for the WBFT chain.
		signer := types.LatestSignerForChainID(params.TestWBFTChainConfig.ChainID)
		gasFeeCap := new(big.Int).Add(new(big.Int).SetUint64(params.MinBaseFee), tip30000)
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   params.TestWBFTChainConfig.ChainID,
			Nonce:     0,
			GasTipCap: tip30000,
			GasFeeCap: gasFeeCap,
			Gas:       21000,
			To:        &common.Address{},
			Value:     big.NewInt(0),
		}), signer, remoteKey)
		if err != nil {
			t.Fatalf("SignTx: %v", err)
		}

		// Construct two headers with the SAME root (empty block scenario) but
		// different GasTip fields.
		sharedRoot := common.HexToHash("0xabcd1234")
		headerA := makeWBFTHeaderAnzeon(1, sharedRoot, tip30000)
		headerB := makeWBFTHeaderAnzeon(2, sharedRoot, tip27600) // same root, different GasTip

		// Seed env with headerA.
		pool.mu.Lock()
		pool.anzeonTipEnv.SetCurrentBlock(headerA)
		pool.mu.Unlock()

		// Verify env returns tip30000 for our unauthorized tx.
		pool.mu.RLock()
		got := pool.anzeonTipEnv.GetAnzeonTipCap(tx)
		pool.mu.RUnlock()
		if got == nil || got.Cmp(tip30000) != 0 {
			t.Fatalf("AC3 precondition: expected GetAnzeonTipCap = tip30000 after headerA; got %v", got)
		}

		// Now SetCurrentBlock with headerB (empty block: same root, lower GasTip).
		// Step 1 fix must accept it and update currentBlock.
		pool.mu.Lock()
		pool.anzeonTipEnv.SetCurrentBlock(headerB)
		pool.mu.Unlock()

		// After the fix, GetAnzeonTipCap must return tip27600.
		pool.mu.RLock()
		got2 := pool.anzeonTipEnv.GetAnzeonTipCap(tx)
		pool.mu.RUnlock()
		if got2 == nil || got2.Cmp(tip27600) != 0 {
			t.Fatalf("AC3 FAIL: GetAnzeonTipCap after empty-block headerB = %v; want %v (restoration)", got2, tip27600)
		}
	})
}
