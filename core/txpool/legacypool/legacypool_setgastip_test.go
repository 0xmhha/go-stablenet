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

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestSetGasTipBidirectional verifies that SetGasTip reconciles the pool in
// both directions: raising the tip drops remotes below the new threshold, and
// lowering it again does not wrongly drop transactions that still satisfy the
// (lower) threshold.
func TestSetGasTipBidirectional(t *testing.T) {
	t.Parallel()

	pool, _ := setupPool()
	defer pool.Close()

	keys := make([]*ecdsa.PrivateKey, 3)
	for i := range keys {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1_000_000_000))
	}

	// Two well-priced txs (tip 5) and one cheap tx (tip 2).
	txs := types.Transactions{
		pricedTransaction(0, 100000, big.NewInt(5), keys[0]),
		pricedTransaction(0, 100000, big.NewInt(5), keys[1]),
		pricedTransaction(0, 100000, big.NewInt(2), keys[2]),
	}
	pool.addRemotesSync(txs)

	if pending, _ := pool.Stats(); pending != 3 {
		t.Fatalf("initial pending = %d, want 3", pending)
	}

	// Raise the tip above the cheap tx: it must be dropped, the others kept.
	pool.SetGasTip(big.NewInt(4))
	if pending, _ := pool.Stats(); pending != 2 {
		t.Fatalf("after tip increase pending = %d, want 2 (cheap tx must drop)", pending)
	}

	// Lower the tip below all survivors: the bidirectional reconcile must not
	// drop any of the still-valid txs (tip 5 >= 3).
	pool.SetGasTip(big.NewInt(3))
	if pending, _ := pool.Stats(); pending != 2 {
		t.Fatalf("after tip decrease pending = %d, want 2 (valid txs must survive)", pending)
	}

	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
