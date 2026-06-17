// SPDX-License-Identifier: GPL-3.0-or-later
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
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestRemotesBelowTipUsesAnzeonTipCap verifies that RemotesBelowTip compares
// against the cached AnzeonTipCap (the effective execution tip) rather than the
// transaction's own GasTipCap. This is the regression for the pending-pool
// stagnation seen after a governance GasTip change: an unauthorized account's
// effective tip is the (lower) header GasTip, so such a transaction must be
// dropped when the threshold rises even though its declared GasTipCap is high.
func TestRemotesBelowTipUsesAnzeonTipCap(t *testing.T) {
	key, _ := crypto.GenerateKey()

	mkTx := func(nonce uint64, gasTipCap *big.Int, anzeonTipCap *big.Int) *types.Transaction {
		tx := newTip(nonce, gasTipCap, key)
		if anzeonTipCap != nil {
			tx.SetAnzeonTipCap(anzeonTipCap)
		}
		return tx
	}

	threshold := big.NewInt(30000)

	// Cached AnzeonTipCap below threshold even though GasTipCap is above it.
	// MUST be dropped.
	dropped := mkTx(0, big.NewInt(100000), big.NewInt(27600))
	// Cached AnzeonTipCap at/above threshold: MUST be kept.
	kept := mkTx(1, big.NewInt(100000), big.NewInt(30000))
	// No cached AnzeonTipCap: falls back to GasTipCap (below threshold) -> dropped.
	fallbackDrop := mkTx(2, big.NewInt(10000), nil)
	// No cached AnzeonTipCap: falls back to GasTipCap (above threshold) -> kept.
	fallbackKeep := mkTx(3, big.NewInt(50000), nil)

	lookup := newLookup()
	for _, tx := range []*types.Transaction{dropped, kept, fallbackDrop, fallbackKeep} {
		lookup.Add(tx, false)
	}

	found := lookup.RemotesBelowTip(threshold)

	inFound := make(map[common.Hash]bool, len(found))
	for _, tx := range found {
		inFound[tx.Hash()] = true
	}

	if !inFound[dropped.Hash()] {
		t.Errorf("tx with AnzeonTipCap below threshold should be dropped, but was not")
	}
	if inFound[kept.Hash()] {
		t.Errorf("tx with AnzeonTipCap at/above threshold should be kept, but was dropped")
	}
	if !inFound[fallbackDrop.Hash()] {
		t.Errorf("tx without cache and GasTipCap below threshold should be dropped, but was not")
	}
	if inFound[fallbackKeep.Hash()] {
		t.Errorf("tx without cache and GasTipCap above threshold should be kept, but was dropped")
	}
}

// newTip builds a signed dynamic-fee transaction with the given gas tip cap.
func newTip(nonce uint64, gasTipCap *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(1)), &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: new(big.Int).Add(gasTipCap, big.NewInt(1000000)),
		Gas:       21000,
		To:        &common.Address{},
		Value:     big.NewInt(0),
	})
	return tx
}
