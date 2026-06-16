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

package types

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestInvalidateAnzeonTipCap verifies the per-tx Anzeon tip cap cache can be
// set, ignores nil writes, is cleared by InvalidateAnzeonTipCap, and can be
// repopulated afterwards (the lifecycle relied on by LegacyPool.SetGasTip).
func TestInvalidateAnzeonTipCap(t *testing.T) {
	tx := NewTransaction(0, common.Address{}, big.NewInt(0), 21000, big.NewInt(1), nil)

	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Fatalf("fresh tx cache = %v, want nil", got)
	}

	tx.SetAnzeonTipCap(big.NewInt(100))
	if got := tx.GetAnzeonTipCap(); got == nil || got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("after Set(100) cache = %v, want 100", got)
	}

	// nil input must be ignored, not clear the cache.
	tx.SetAnzeonTipCap(nil)
	if got := tx.GetAnzeonTipCap(); got == nil || got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("after Set(nil) cache = %v, want unchanged 100", got)
	}

	tx.InvalidateAnzeonTipCap()
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Fatalf("after Invalidate cache = %v, want nil", got)
	}

	// The cache must be repopulatable after an invalidation so the next
	// validation pass can refresh it against the current header.
	tx.SetAnzeonTipCap(big.NewInt(200))
	if got := tx.GetAnzeonTipCap(); got == nil || got.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("after re-Set(200) cache = %v, want 200", got)
	}

	// A second invalidation must remain a no-op-safe nil.
	tx.InvalidateAnzeonTipCap()
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Fatalf("after second Invalidate cache = %v, want nil", got)
	}
}
