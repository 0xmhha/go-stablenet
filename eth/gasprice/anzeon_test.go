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

package gasprice

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// headerWithGasTip builds a header whose WBFT extra carries the given
// governance gas tip, so header.GasTip() returns it.
func headerWithGasTip(root common.Hash, number int64, gasTip *big.Int) *types.Header {
	extra, _ := rlp.EncodeToBytes(&types.WBFTExtra{GasTip: gasTip})
	return &types.Header{
		Root:   root,
		Number: big.NewInt(number),
		Extra:  extra,
	}
}

// TestSetCurrentBlockRefreshesHeaderOnSameRoot covers the empty-block case
// (governance gas tip changes while the post-block state root stays identical):
// the header must always be adopted so GetAnzeonTipCap sees the new tip, while
// the expensive stateAt call is skipped because the root did not change.
func TestSetCurrentBlockRefreshesHeaderOnSameRoot(t *testing.T) {
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	calls := 0
	stateAt := func(common.Hash) (*state.StateDB, error) {
		calls++
		return statedb, nil
	}
	env := NewAnzeonTipEnv(params.TestChainConfig, stateAt)

	root := common.Hash{0x01}
	env.SetCurrentBlock(headerWithGasTip(root, 1, big.NewInt(27600)))
	env.SetCurrentBlock(headerWithGasTip(root, 2, big.NewInt(30000)))

	if got := env.currentBlock.GasTip(); got == nil || got.Cmp(big.NewInt(30000)) != 0 {
		t.Fatalf("currentBlock.GasTip() = %v, want 30000 (header must refresh on same root)", got)
	}
	if calls != 1 {
		t.Fatalf("stateAt called %d times, want 1 (must skip on identical root)", calls)
	}
}

// TestSetCurrentBlockNilHeaderIsNoop verifies the nil guard is preserved.
func TestSetCurrentBlockNilHeaderIsNoop(t *testing.T) {
	env := NewAnzeonTipEnv(params.TestChainConfig, func(common.Hash) (*state.StateDB, error) {
		return nil, nil
	})
	h := headerWithGasTip(common.Hash{0x02}, 1, big.NewInt(27600))
	env.SetCurrentBlock(h)
	env.SetCurrentBlock(nil)
	if env.currentBlock != h {
		t.Fatalf("nil header must not change currentBlock")
	}
}

// TestSetCurrentBlockDifferentRootRefreshesState verifies that a changed state
// root triggers a fresh stateAt lookup (and signer rebuild).
func TestSetCurrentBlockDifferentRootRefreshesState(t *testing.T) {
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	calls := 0
	stateAt := func(common.Hash) (*state.StateDB, error) {
		calls++
		return statedb, nil
	}
	env := NewAnzeonTipEnv(params.TestChainConfig, stateAt)

	env.SetCurrentBlock(headerWithGasTip(common.Hash{0x01}, 1, big.NewInt(27600)))
	env.SetCurrentBlock(headerWithGasTip(common.Hash{0x02}, 2, big.NewInt(30000)))

	if calls != 2 {
		t.Fatalf("stateAt called %d times, want 2 (distinct roots must refresh)", calls)
	}
}
