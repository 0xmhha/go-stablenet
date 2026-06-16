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

// makeWBFTHeader returns a minimal Header with a WBFTExtra blob encoding the
// given gasTip, root, and number. The difficulty is set to the WBFT default so
// that Header.Hash() uses the Istanbul-filtered hash (real blocks do this).
func makeWBFTHeader(number int64, root common.Hash, gasTip *big.Int) *types.Header {
	extra := &types.WBFTExtra{
		GasTip: gasTip,
	}
	extraBytes, err := rlp.EncodeToBytes(extra)
	if err != nil {
		panic("makeWBFTHeader: rlp encode: " + err.Error())
	}
	return &types.Header{
		Number:     big.NewInt(number),
		Root:       root,
		Extra:      extraBytes,
		Difficulty: types.WBFTDefaultDifficulty,
		Time:       uint64(number),
	}
}

// makeAnzeonEnv returns a new AnzeonTipEnv backed by an in-memory state DB
// where addr is NOT authorized (simulating an unauthorized governance-proposal
// sender). stateAtCount is incremented each time stateAt is called.
func makeAnzeonEnv(t *testing.T, config *params.ChainConfig, addr common.Address, stateAtCount *int) *AnzeonTipEnv {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	// addr is not authorized — IsAuthorized returns false by default.

	stateAt := func(root common.Hash) (*state.StateDB, error) {
		if stateAtCount != nil {
			*stateAtCount++
		}
		return statedb, nil
	}
	return NewAnzeonTipEnv(config, stateAt)
}

// TestSetCurrentBlock_RefreshOnGasTipChangeSameRoot verifies that when two
// headers share the same state root but carry different WBFTExtra.GasTip
// values, SetCurrentBlock accepts the new header and GetAnzeonTipCap returns
// the updated tip for an unauthorized sender.
func TestSetCurrentBlock_RefreshOnGasTipChangeSameRoot(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	env := makeAnzeonEnv(t, config, addr, nil)

	root := common.HexToHash("0xaaaa")
	tip30000 := big.NewInt(30000)
	tip27600 := big.NewInt(27600)

	headerA := makeWBFTHeader(1, root, tip30000)
	headerB := makeWBFTHeader(1, root, tip27600) // same root, same number, different GasTip

	// Seed with headerA.
	env.SetCurrentBlock(headerA)
	// Confirm env saw headerA.
	if env.currentBlock == nil {
		t.Fatal("currentBlock should be set after SetCurrentBlock(headerA)")
	}

	// Now call with headerB — same root but different GasTip. Must refresh.
	env.SetCurrentBlock(headerB)
	if env.currentBlock != headerB {
		t.Fatal("currentBlock should have been updated to headerB (different GasTip, same root)")
	}

	// Build a minimal tx signed by addr to exercise GetAnzeonTipCap.
	signer := types.MakeSigner(config, headerB.Number, headerB.Time)
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   config.ChainID,
		Nonce:     0,
		GasFeeCap: big.NewInt(1e18),
		GasTipCap: big.NewInt(1e18),
		Gas:       21000,
	}), signer, key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}

	got := env.GetAnzeonTipCap(tx)
	if got == nil {
		t.Fatal("GetAnzeonTipCap returned nil; want 27600")
	}
	if got.Cmp(tip27600) != 0 {
		t.Fatalf("GetAnzeonTipCap = %v; want %v (new GasTip after restoration)", got, tip27600)
	}
}

// TestSetCurrentBlock_RefreshOnNumberAdvanceSameRoot verifies that when a
// header with the same root but an advanced block number is passed,
// SetCurrentBlock re-stamps the signer (visible via env.signer != nil).
func TestSetCurrentBlock_RefreshOnNumberAdvanceSameRoot(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig
	env := makeAnzeonEnv(t, config, common.Address{}, nil)

	root := common.HexToHash("0xbbbb")
	tip := big.NewInt(27600)

	headerA := makeWBFTHeader(5, root, tip)
	headerB := makeWBFTHeader(6, root, tip) // same root+tip, advanced number

	env.SetCurrentBlock(headerA)
	if env.currentBlock == nil {
		t.Fatal("expected currentBlock set after headerA")
	}

	env.SetCurrentBlock(headerB)
	if env.currentBlock != headerB {
		t.Fatal("currentBlock should have been updated to headerB (advanced number, same root)")
	}
	if env.signer == nil {
		t.Fatal("signer must be re-stamped after number advance")
	}
}

// TestSetCurrentBlock_NoStateAtWhenRootUnchanged verifies that stateAt is NOT
// invoked on a same-root refresh (only the header/signer are updated).
func TestSetCurrentBlock_NoStateAtWhenRootUnchanged(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig
	var callCount int
	env := makeAnzeonEnv(t, config, common.Address{}, &callCount)

	root := common.HexToHash("0xcccc")
	tip := big.NewInt(27600)

	// First call — root is new so stateAt is invoked once.
	headerA := makeWBFTHeader(1, root, tip)
	env.SetCurrentBlock(headerA)
	if callCount != 1 {
		t.Fatalf("expected 1 stateAt call after first SetCurrentBlock; got %d", callCount)
	}

	// Second call — same root, different number. stateAt must NOT be called again.
	headerB := makeWBFTHeader(2, root, tip)
	env.SetCurrentBlock(headerB)
	if callCount != 1 {
		t.Fatalf("stateAt was called again on same-root refresh; got %d total calls (want 1)", callCount)
	}
}

// TestSetCurrentBlock_NilHeader_NoOp verifies the nil-header guard is preserved.
func TestSetCurrentBlock_NilHeader_NoOp(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig
	env := makeAnzeonEnv(t, config, common.Address{}, nil)

	env.SetCurrentBlock(nil) // must not panic or mutate state
	if env.currentBlock != nil {
		t.Fatal("currentBlock should remain nil after SetCurrentBlock(nil)")
	}

	// Also verify that calling nil after a real header leaves currentBlock intact.
	root := common.HexToHash("0xdddd")
	h := makeWBFTHeader(1, root, big.NewInt(27600))
	env.SetCurrentBlock(h)
	prev := env.currentBlock

	env.SetCurrentBlock(nil)
	if env.currentBlock != prev {
		t.Fatal("SetCurrentBlock(nil) must not clear an already-set currentBlock")
	}
}

// TestSetCurrentBlock_ZeroRoot_ClearsState verifies that when a header with a
// zero root is passed, the state is cleared (currentState = nil) even when the
// block is otherwise a valid new head.
func TestSetCurrentBlock_ZeroRoot_ClearsState(t *testing.T) {
	t.Parallel()

	config := params.TestWBFTChainConfig
	var callCount int
	env := makeAnzeonEnv(t, config, common.Address{}, &callCount)

	// Seed with a non-zero root so state is populated.
	realRoot := common.HexToHash("0xeeee")
	h1 := makeWBFTHeader(1, realRoot, big.NewInt(27600))
	env.SetCurrentBlock(h1)
	if env.currentState == nil {
		t.Fatal("expected currentState to be set after non-zero root header")
	}

	// Now a header with the zero root.
	h2 := makeWBFTHeader(2, common.Hash{}, big.NewInt(27600))
	env.SetCurrentBlock(h2)
	if env.currentState != nil {
		t.Fatal("currentState should be nil after zero-root header")
	}
}
