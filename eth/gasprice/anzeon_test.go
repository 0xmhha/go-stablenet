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

// headerWithGasTip builds a minimal block header whose WBFT extra carries the
// given governance GasTip and whose state root is set to root.
func headerWithGasTip(t *testing.T, root common.Hash, gasTip *big.Int) *types.Header {
	t.Helper()
	extra, err := rlp.EncodeToBytes(&types.WBFTExtra{GasTip: gasTip})
	if err != nil {
		t.Fatalf("failed to encode WBFT extra: %v", err)
	}
	return &types.Header{
		Number:  big.NewInt(1),
		Time:    1,
		Root:    root,
		Extra:   extra,
		BaseFee: big.NewInt(1),
	}
}

func TestGasTipChanged(t *testing.T) {
	tests := []struct {
		name string
		a, b *big.Int
		want bool
	}{
		{"both nil", nil, nil, false},
		{"a nil only", nil, big.NewInt(1), true},
		{"b nil only", big.NewInt(1), nil, true},
		{"equal same pointer reuse", big.NewInt(27600), big.NewInt(27600), false},
		{"different", big.NewInt(27600), big.NewInt(30000), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gasTipChanged(tc.a, tc.b); got != tc.want {
				t.Fatalf("gasTipChanged(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSetCurrentBlockGasTipChange(t *testing.T) {
	stateAt := func(common.Hash) (*state.StateDB, error) { return nil, nil }
	env := NewAnzeonTipEnv(params.TestChainConfig, stateAt)

	root := common.HexToHash("0xdeadbeef")

	// Initial head: GasTip = 30000.
	first := headerWithGasTip(t, root, big.NewInt(30000))
	env.SetCurrentBlock(first)
	if env.currentBlock != first {
		t.Fatalf("currentBlock not set on first call")
	}

	// New header, identical state root but GasTip restored to 27600 (e.g. an
	// empty block whose header reflects a governance GasTip change). The block
	// must be refreshed despite the matching state root.
	restored := headerWithGasTip(t, root, big.NewInt(27600))
	env.SetCurrentBlock(restored)
	if env.currentBlock != restored {
		t.Fatalf("currentBlock not refreshed when GasTip changed with identical root")
	}
	if got := env.currentBlock.GasTip(); got == nil || got.Cmp(big.NewInt(27600)) != 0 {
		t.Fatalf("currentBlock GasTip = %v, want 27600", got)
	}

	// Same root and same GasTip: must NOT refresh.
	same := headerWithGasTip(t, root, big.NewInt(27600))
	env.SetCurrentBlock(same)
	if env.currentBlock != restored {
		t.Fatalf("currentBlock refreshed when root and GasTip are both unchanged")
	}
}
