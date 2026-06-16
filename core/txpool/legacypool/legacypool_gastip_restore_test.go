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

// Package legacypool — regression tests for anzeonTipCap cache invalidation
// on governance minTip changes (LOCAL-20260616_055857).
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

// TestSetGasTip_UpThenDown_InvalidatesAnzeonCache verifies that
// LegacyPool.SetGasTip clears the per-tx anzeonTipCap cache whenever the
// network minTip changes — whether upward or downward.
//
// This is the unit-level regression guard for the "tx stuck in pending pool
// after governance minTip restore" bug (LOCAL-20260616_055857).
//
// Scenario:
//  1. Pool starts with minTip = low (27600 Gwei unit).
//  2. A dynamic-fee tx is admitted.
//  3. The tx's anzeonTipCap is manually primed to simulate the stale value
//     that would be set during admission at the old tip.
//  4. SetGasTip(high) is called → cache must be cleared.
//  5. Cache is re-primed (simulating re-validation at high tip).
//  6. SetGasTip(low) is called → cache must be cleared again.
//  7. After clearance, EffectiveGasTip queries the env fresh.
func TestSetGasTip_UpThenDown_InvalidatesAnzeonCache(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := newTestBlockChain(params.TestChainConfig, 10000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("failed to init pool: %v", err)
	}
	defer pool.Close()
	<-pool.initDoneCh

	// Fund a key and add a dynamic-fee tx.
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.GWei)))

	// GasFeeCap must exceed baseFee (1e9) + tip so EffectiveGasTip is not capped to zero.
	// Use GasFeeCap = 2e9 so effective tip = min(30000, 2e9 - 1e9) = min(30000, 1e9) = 30000.
	tx := dynamicFeeTx(0, 100000, big.NewInt(2e9), big.NewInt(30000), key)
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("failed to add tx: %v", err)
	}

	// Prime a stale anzeonTipCap on the tx (simulates cache set at old minTip).
	staleValue := big.NewInt(30000)
	tx.SetAnzeonTipCap(staleValue)
	if got := tx.GetAnzeonTipCap(); got == nil || got.Cmp(staleValue) != 0 {
		t.Fatalf("precondition: expected stale cache %v, got %v", staleValue, got)
	}

	// Step 4: SetGasTip(high) — cache must be cleared.
	pool.SetGasTip(big.NewInt(30000))
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Errorf("after SetGasTip(high): expected nil cache, got %v", got)
	}

	// Re-prime the cache to simulate re-validation at the raised tip.
	tx.SetAnzeonTipCap(big.NewInt(30000))

	// Step 6: SetGasTip(low) — cache must be cleared again (down-direction).
	pool.SetGasTip(big.NewInt(27600))
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Errorf("after SetGasTip(down): expected nil cache, got %v (this is the bug under repair)", got)
	}
}

// TestSetGasTip_NoChangeDoesNotClear verifies that SetGasTip with the same
// value as the current tip does NOT clear the anzeonTipCap cache (early-return
// path; avoids unnecessary work).
func TestSetGasTip_NoChangeDoesNotClear(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := newTestBlockChain(params.TestChainConfig, 10000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("failed to init pool: %v", err)
	}
	defer pool.Close()
	<-pool.initDoneCh

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.GWei)))

	tx := dynamicFeeTx(0, 100000, big.NewInt(2e9), big.NewInt(1), key)
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("failed to add tx: %v", err)
	}

	// Get current tip and set the cache.
	currentTip := pool.gasTip.Load().ToBig()
	tx.SetAnzeonTipCap(big.NewInt(27600))

	// Call SetGasTip with the SAME value — early return, cache must not be touched.
	pool.SetGasTip(new(big.Int).Set(currentTip))
	if got := tx.GetAnzeonTipCap(); got == nil || got.Cmp(big.NewInt(27600)) != 0 {
		t.Errorf("SetGasTip(same) must not clear cache; got %v", got)
	}
}

// TestInvalidateAnzeonTipCache_AllTxsCleared verifies that invalidateAnzeonTipCache
// clears the cache for every tx tracked by the pool (both local and remote).
func TestInvalidateAnzeonTipCache_AllTxsCleared(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := newTestBlockChain(params.TestChainConfig, 10000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("failed to init pool: %v", err)
	}
	defer pool.Close()
	<-pool.initDoneCh

	// Create two different accounts with txs.
	keyA, _ := crypto.GenerateKey()
	keyB, _ := crypto.GenerateKey()
	addrA := crypto.PubkeyToAddress(keyA.PublicKey)
	addrB := crypto.PubkeyToAddress(keyB.PublicKey)
	testAddBalance(pool, addrA, new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.GWei)))
	testAddBalance(pool, addrB, new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.GWei)))

	// Use GasTipCap=30000 so that SetGasTip(27600) does NOT drop the tx (30000 >= 27600).
	txA := dynamicFeeTx(0, 100000, big.NewInt(2e9), big.NewInt(30000), keyA)
	txB := dynamicFeeTx(0, 100000, big.NewInt(2e9), big.NewInt(30000), keyB)
	for i, err := range pool.addRemotesSync([]*types.Transaction{txA, txB}) {
		if err != nil {
			t.Fatalf("failed to add tx[%d]: %v", i, err)
		}
	}

	// Prime caches.
	primed := big.NewInt(30000)
	txA.SetAnzeonTipCap(primed)
	txB.SetAnzeonTipCap(primed)

	// Trigger a SetGasTip change to clear all caches (tip goes DOWN — no drops).
	pool.SetGasTip(big.NewInt(27600))

	// Both must be nil.
	if got := txA.GetAnzeonTipCap(); got != nil {
		t.Errorf("txA cache not cleared: got %v", got)
	}
	if got := txB.GetAnzeonTipCap(); got != nil {
		t.Errorf("txB cache not cleared: got %v", got)
	}
}

// assertAnzeonCacheCoherent is a debug/test helper that verifies no pooled tx
// has a stale anzeonTipCap relative to the current header.GasTip and the tx's
// authorization state. MUST be called with pool.mu held (read lock sufficient).
//
// For unauthorized senders, the cached value must equal header.GasTip().
// For authorized senders, the cached value must equal tx.GasTipCap().
// A nil cache is always valid (will be recomputed on demand).
func assertAnzeonCacheCoherent(t *testing.T, pool *LegacyPool) {
	t.Helper()
	header := pool.currentHead.Load()
	pool.all.Range(func(_ common.Hash, tx *types.Transaction, _ bool) bool {
		cached := tx.GetAnzeonTipCap()
		if cached == nil {
			return true // nil is always ok — will be recomputed
		}
		from, err := types.Sender(pool.signer, tx)
		if err != nil {
			t.Errorf("assertAnzeonCacheCoherent: sender error for tx %s: %v", tx.Hash().Hex(), err)
			return true
		}
		if pool.currentState.IsAuthorized(from) {
			if cached.Cmp(tx.GasTipCap()) != 0 {
				t.Errorf("authorized tx %s: cached %v != GasTipCap %v",
					tx.Hash().Hex(), cached, tx.GasTipCap())
			}
		} else {
			if hTip := header.GasTip(); hTip != nil && cached.Cmp(hTip) != 0 {
				t.Errorf("unauthorized tx %s: cached %v != header.GasTip %v",
					tx.Hash().Hex(), cached, hTip)
			}
		}
		return true
	}, true, true)
}

// TestSetGasTip_UpThenDown_RestoresEffectiveTip is the end-to-end regression
// test for the "stuck in pending pool after governance minTip restore" bug.
//
// It uses TestChainConfig (non-Anzeon) to verify the fundamental cache-clearing
// mechanic. For the Anzeon-specific EffectiveGasTip assertion with header.GasTip,
// see TestTransaction_EffectiveGasTip_AfterClear in core/types.
func TestSetGasTip_UpThenDown_RestoresEffectiveTip(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := newTestBlockChain(params.TestChainConfig, 10000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver()); err != nil {
		t.Fatalf("failed to init pool: %v", err)
	}
	defer pool.Close()
	<-pool.initDoneCh

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, new(big.Int).Mul(big.NewInt(1e6), big.NewInt(params.GWei)))

	// Admit a dynamic-fee tx with GasTipCap=30000.
	// GasFeeCap must exceed baseFee(1e9)+tip so effective tip = tip, not zero.
	tx := dynamicFeeTx(0, 100000, big.NewInt(2e9), big.NewInt(30000), key)
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("failed to add tx: %v", err)
	}

	// Simulate admission at old minTip=27600 by priming the stale cache.
	tx.SetAnzeonTipCap(big.NewInt(30000))

	// 3. Governance raises minTip to 30000.
	pool.SetGasTip(big.NewInt(30000))

	// Cache must be cleared.
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Errorf("after SetGasTip(30000): expected nil cache, got %v", got)
	}

	// 4. Pending with MinTip=30000 — tx should be in pending (raw GasTipCap=30000).
	pending := pool.Pending(txpool.PendingFilter{
		MinTip:  uint256.NewInt(30000),
		Header:  pool.currentHead.Load(),
		BaseFee: uint256.NewInt(params.InitialBaseFee),
	})
	if len(pending[addr]) == 0 {
		t.Fatalf("tx should be in pending with MinTip=30000 (GasTipCap=30000)")
	}

	// Re-prime the cache (simulates re-validation under the raised tip).
	tx.SetAnzeonTipCap(big.NewInt(30000))

	// 5. Governance restores minTip to 27600.
	pool.SetGasTip(big.NewInt(27600))

	// Cache must be cleared again.
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Errorf("after SetGasTip(27600): expected nil cache, got %v (this is the bug under repair)", got)
	}

	// 6. Pending with MinTip=27600 — tx must still be included (the bug: it was stuck).
	pending2 := pool.Pending(txpool.PendingFilter{
		MinTip:  uint256.NewInt(27600),
		Header:  pool.currentHead.Load(),
		BaseFee: uint256.NewInt(params.InitialBaseFee),
	})
	if len(pending2[addr]) == 0 {
		t.Fatalf("tx stuck in pending pool after minTip restore — this is the bug under repair")
	}

	// 7. After Pending() call (which re-caches via EffectiveGasTipIntCmp),
	//    the cached value must be nil or a valid effective tip (not the stale 30000).
	cached := tx.GetAnzeonTipCap()
	if cached != nil && cached.Cmp(big.NewInt(30000)) == 0 {
		t.Errorf("stale anzeonTipCap 30000 persisted after minTip restore: got %v", cached)
	}

	// Run self-check invariant.
	pool.mu.RLock()
	assertAnzeonCacheCoherent(t, pool)
	pool.mu.RUnlock()
}
