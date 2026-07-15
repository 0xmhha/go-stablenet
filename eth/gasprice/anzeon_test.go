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
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// anzeonTestChainConfig returns a minimal ChainConfig with Anzeon enabled,
// sufficient to construct an AnzeonTipEnv in unit tests.
func anzeonTestChainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID: big.NewInt(1),
		Anzeon:  &params.AnzeonConfig{},
	}
}

// headerWithRootAndGasTip builds a *types.Header whose WBFTExtra.GasTip equals
// gasTip and whose state root equals root. All other WBFTExtra fields are zero.
func headerWithRootAndGasTip(root common.Hash, gasTip *big.Int) *types.Header {
	extra := &types.WBFTExtra{GasTip: new(big.Int).Set(gasTip)}
	enc, err := rlp.EncodeToBytes(extra)
	if err != nil {
		panic("headerWithRootAndGasTip: rlp encode failed: " + err.Error())
	}
	return &types.Header{
		Root:   root,
		Number: big.NewInt(1),
		Time:   1,
		Extra:  enc,
	}
}

// TestSetCurrentBlockRefreshesGasTipOnSameRoot pins the fix introduced by
// LOCAL-20260715_050120: two consecutive calls to SetCurrentBlock with headers
// that share the same state root but carry different WBFTExtra GasTip values
// must both be reflected by the env. On unfixed code the second call is skipped
// (root unchanged) and the env returns the stale pre-raise GasTip 27600 Gwei;
// after the fix it returns the fresh 30000 Gwei.
//
// This is the same same-root/different-GasTip condition the e2e chainbench
// oracle (repro/LOCAL-20260715_050120-gastip-restore-stall) fires on: an idle
// window of empty blocks after a governance GasTip raise keeps the state root
// constant while the WBFTExtra GasTip has already moved.
func TestSetCurrentBlockRefreshesGasTipOnSameRoot(t *testing.T) {
	env := NewAnzeonTipEnv(anzeonTestChainConfig(), func(_ common.Hash) (*state.StateDB, error) {
		return nil, nil // state not exercised in this header-GasTip assertion
	})

	root := common.HexToHash("0x0101010101010101010101010101010101010101010101010101010101010101")

	// h1 carries the pre-raise tip (27 600 Gwei); h2 carries the post-raise tip
	// (30 000 Gwei) with the SAME state root — exactly the idle-window condition.
	h1 := headerWithRootAndGasTip(root, big.NewInt(27_600_000_000_000))
	h2 := headerWithRootAndGasTip(root, big.NewInt(30_000_000_000_000))

	env.SetCurrentBlock(h1)
	// Same root, new GasTip. On unfixed code this call is a no-op (root guard
	// skips the whole block). After the fix only the stateAt call is skipped;
	// currentBlock and signer always advance.
	env.SetCurrentBlock(h2)

	got := env.currentBlock.GasTip()
	want := big.NewInt(30_000_000_000_000)
	if got == nil || got.Cmp(want) != 0 {
		t.Fatalf("env pinned to stale header GasTip: got %v, want %v (30000 Gwei)", got, want)
	}
}
