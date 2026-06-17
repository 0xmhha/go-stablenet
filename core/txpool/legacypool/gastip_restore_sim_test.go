// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Chain-level simulation for the governance gasTip restore scenario (PR-77).
// It models the lifecycle the wbft miner drives in production — reading the
// GovValidator gasTip slot after each imported block and calling
// TxPool.SetGasTip — by interleaving SetGasTip with pool.reset across a
// sequence of simulated blocks. This complements the unit tests in
// legacypool_test.go (TestRepricingDynamicReflection /
// TestSystemContractTxExemptFromMinTip) by exercising the reset/promote/demote
// path between governance changes, rather than isolated SetGasTip calls.
//
// A full consensus E2E (consensus/wbft/backend/multiengine_test.go) would have
// to deploy GovValidator, run proposals/votes and seal blocks through the WBFT
// engine; pulling that stack into a txpool package test risks an import cycle
// and adds little over driving the pool's own reset path directly, which is the
// established chain-simulation mechanism in this package (testBlockChain).

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
	"github.com/holiman/uint256"
)

// TestGasTipRestoreChainSimulation walks the full governance gasTip lifecycle
//
//	block 1: floor = 27600 (initial)            — a user tx at floor lands pending
//	block 2: governance RAISES floor to 30000   — AC1: the 27600 tx must not stay pending
//	block 3: restore proposal submitted          — AC2: a low-tip tx to GovValidator is
//	                                                admitted despite the 30000 floor and
//	                                                survives the Pending tip filter
//	block 4: governance RESTORES floor to 27600  — normal txs at 27600 flow again and the
//	                                                anzeonTipCap cache holds no stale values
func TestGasTipRestoreChainSimulation(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig

	// Build a pool over the package's chain-simulation mock, keeping the chain
	// handle so we can advance the simulated head between governance changes.
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := newTestBlockChain(config, 10000000, statedb, new(event.Feed))
	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("pool init: %v", err)
	}
	<-pool.initDoneCh
	defer pool.Close()

	// advance simulates importing the next block: it drives a full reorg cycle
	// (reset → demote → promote) through scheduleReorgLoop and waits for it to
	// finish, exactly as the txpool does on a ChainHeadEvent in production.
	// (Calling the low-level pool.reset directly would skip demote/promote and
	// leave pendingNonces unrebuilt.)
	advance := func() {
		<-pool.requestReset(nil, nil)
	}

	bigFund := new(big.Int).Mul(big.NewInt(1e9), big.NewInt(params.Ether))
	minBaseFee := new(big.Int).SetUint64(params.MinBaseFee)
	floor := gwei(27600)
	high := gwei(30000)
	dest := common.HexToAddress("0xC0FFEE")

	fundedTx := func(tip, feeCap *big.Int, to common.Address) *types.Transaction {
		key, _ := crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), bigFund)
		return wbftDynamicFeeTx(0, 100000, feeCap, tip, to, key)
	}

	// ---- block 1: initial floor 27600, a user tx at floor lands in pending ----
	pool.SetGasTip(floor)
	advance()

	userTx := fundedTx(floor, new(big.Int).Add(minBaseFee, floor), dest)
	if err := pool.addRemoteSync(userTx); err != nil {
		t.Fatalf("block 1: user tx at floor rejected: %v", err)
	}
	if pending, _ := pool.Stats(); pending != 1 {
		t.Fatalf("block 1: want 1 pending, got %d", pending)
	}

	// ---- block 2: governance raises the floor to 30000 ----
	// AC1: a tx that no longer meets the new floor must not remain pending.
	pool.SetGasTip(high)
	advance()
	if pending, _ := pool.Stats(); pending != 0 {
		t.Fatalf("block 2 (AC1): user tx below raised floor still pending, got %d", pending)
	}

	// ---- block 3: a restore proposal tx to GovValidator with a low (market) tip ----
	// AC2: it must be admitted despite the 30000 floor (system-contract exemption)
	// and must survive the Pending tip-truncation filter.
	govAddr := config.Anzeon.SystemContracts.GovValidator.Address
	propTx := fundedTx(gwei(1), minBaseFee, govAddr) // tip 1 gwei << 30000 floor
	if err := pool.addRemoteSync(propTx); err != nil {
		t.Fatalf("block 3 (AC2): governance restore proposal rejected: %v", err)
	}
	filter := txpool.PendingFilter{MinTip: uint256.MustFromBig(high)}
	if !pendingContains(pool.Pending(filter), propTx.Hash()) {
		t.Fatalf("block 3 (AC2): governance proposal missing from Pending under high MinTip filter")
	}
	advance()

	// ---- block 4: governance restores the floor to 27600 ----
	pool.SetGasTip(floor)
	advance()

	restoredTx := fundedTx(floor, new(big.Int).Add(minBaseFee, floor), dest)
	if err := pool.addRemoteSync(restoredTx); err != nil {
		t.Fatalf("block 4: user tx at restored floor rejected: %v", err)
	}
	if !pendingContains(pool.Pending(txpool.PendingFilter{MinTip: uint256.MustFromBig(floor)}), restoredTx.Hash()) {
		t.Fatalf("block 4: restored-floor tx not pending after governance restore")
	}

	// No stale per-tx anzeonTipCap caches survived the floor changes.
	assertNoStaleAnzeonTipCache(t, pool)
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// pendingContains reports whether hash appears anywhere in a Pending() result.
func pendingContains(pending map[common.Address][]*txpool.LazyTransaction, hash common.Hash) bool {
	for _, lazies := range pending {
		for _, lz := range lazies {
			if lz.Hash == hash {
				return true
			}
		}
	}
	return false
}
