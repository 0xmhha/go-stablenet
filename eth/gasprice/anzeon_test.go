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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// headerWithGasTip builds a WBFT header carrying the given governance GasTip (Wei) in its
// extra data, sharing the supplied state root (empty-block scenario keeps root constant).
func headerWithGasTip(t *testing.T, root common.Hash, number int64, gasTip *big.Int) *types.Header {
	t.Helper()
	extra, err := rlp.EncodeToBytes(&types.WBFTExtra{GasTip: gasTip})
	if err != nil {
		t.Fatalf("encode WBFT extra: %v", err)
	}
	return &types.Header{
		Number:  big.NewInt(number),
		Time:    uint64(number),
		Root:    root,
		Extra:   extra,
		BaseFee: big.NewInt(params.GWei),
	}
}

// TestAnzeonTipEnvRefreshesGasTipOnSameRoot reproduces the governance-restore stall at
// unit tier: after the header GasTip advances while the state root is unchanged (empty
// blocks), the effective tip enforced for a non-authorized sender must track the new
// GasTip. On the buggy Root-only guard it stays pinned at the pre-change value.
func TestAnzeonTipEnvRefreshesGasTipOnSameRoot(t *testing.T) {
	// Anzeon-enabled config; presence of Anzeon toggles AnzeonEnabled().
	cfg := *params.TestChainConfig
	cfg.Anzeon = &params.AnzeonConfig{}

	// In-memory state: a fresh sender account is non-authorized by default, so the
	// override returns the header GasTip (the enforced minimum path).
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	root := statedb.IntermediateRoot(false)
	stateAt := func(common.Hash) (*state.StateDB, error) { return statedb, nil }

	env := NewAnzeonTipEnv(&cfg, stateAt)

	// A remote (non-authorized) sender's tx priced at the raised floor (30000 Gwei).
	key, _ := crypto.GenerateKey()
	signer := types.LatestSigner(&cfg)
	tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(30000 * params.GWei),
		GasFeeCap: big.NewInt(60000 * params.GWei),
		Gas:       21000,
		To:        &common.Address{},
		Value:     big.NewInt(0),
	})
	if err != nil {
		t.Fatalf("sign tx: %v", err)
	}

	// Block N: pre-restore floor 27600 Gwei.
	oldTip := big.NewInt(27600 * params.GWei)
	env.SetCurrentBlock(headerWithGasTip(t, root, 49, oldTip))
	if got := env.GetAnzeonTipCap(tx); got.Cmp(oldTip) != 0 {
		t.Fatalf("baseline effective tip = %v, want %v", got, oldTip)
	}

	// Block N+1..: empty blocks preserve the state root, but governance raised the
	// GasTip to 30000 Gwei — the header reflects it. The env MUST refresh.
	newTip := big.NewInt(30000 * params.GWei)
	env.SetCurrentBlock(headerWithGasTip(t, root, 50, newTip))

	if got := env.GetAnzeonTipCap(tx); got.Cmp(newTip) != 0 {
		t.Fatalf("stale GasTip: effective tip = %v after same-root advance, want %v "+
			"(SetCurrentBlock skipped the refresh because only Root is checked)", got, newTip)
	}
}
