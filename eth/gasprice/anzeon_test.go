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
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// makeHeaderWithGasTip builds a *types.Header whose WBFTExtra.GasTip == gasTip
// and Root == root. It round-trips through Header.GasTip() so the test fails
// loudly if the WBFTExtra encode/decode paths ever diverge.
func makeHeaderWithGasTip(t *testing.T, num *big.Int, root common.Hash, gasTip *big.Int) *types.Header {
	t.Helper()
	extra := &types.WBFTExtra{GasTip: gasTip}
	var buf bytes.Buffer
	if err := extra.EncodeRLP(&buf); err != nil {
		t.Fatalf("encode WBFTExtra: %v", err)
	}
	h := &types.Header{
		Number: num,
		Root:   root,
		Extra:  buf.Bytes(),
	}
	got := h.GasTip()
	switch {
	case gasTip == nil && got != nil:
		t.Fatalf("round-trip GasTip = %v, want nil", got)
	case gasTip != nil && (got == nil || got.Cmp(gasTip) != 0):
		t.Fatalf("round-trip GasTip = %v, want %v", got, gasTip)
	}
	return h
}

// stateAtSpy counts how many times the stateAt callback is invoked so a test
// can assert that a no-op SetCurrentBlock does not re-fetch state.
type stateAtSpy struct {
	calls int
}

func (s *stateAtSpy) fn() func(common.Hash) (*state.StateDB, error) {
	return func(common.Hash) (*state.StateDB, error) {
		s.calls++
		return nil, nil
	}
}

func anzeonEnabledChainConfig() *params.ChainConfig {
	cfg := *params.TestChainConfig
	cfg.Anzeon = &params.AnzeonConfig{} // AnzeonEnabled() == (Anzeon != nil)
	return &cfg
}

// TestSetCurrentBlock_EmptyBlockGasTipChangeUpdatesCurrent reproduces the
// empty-block governance GasTip bug: a second header with the SAME state Root
// but a CHANGED GasTip must still refresh currentBlock. Before the fix the
// Root-equality short-circuit kept the stale header.
func TestSetCurrentBlock_EmptyBlockGasTipChangeUpdatesCurrent(t *testing.T) {
	spy := &stateAtSpy{}
	env := NewAnzeonTipEnv(anzeonEnabledChainConfig(), spy.fn())

	sameRoot := common.HexToHash("0x01")
	h1 := makeHeaderWithGasTip(t, big.NewInt(100), sameRoot, big.NewInt(10))
	h2 := makeHeaderWithGasTip(t, big.NewInt(101), sameRoot, big.NewInt(50)) // empty block + governance minTip change

	env.SetCurrentBlock(h1)
	env.SetCurrentBlock(h2)

	got := env.currentBlock.GasTip()
	if got == nil || got.Cmp(big.NewInt(50)) != 0 {
		t.Fatalf("currentBlock.GasTip = %v, want 50 (env not refreshed on empty-block GasTip change)", got)
	}
}

// TestSetCurrentBlock_NoChangeSkipsRefresh is a regression guard: an identical
// header (same Root, same GasTip) must not trigger a redundant stateAt fetch.
func TestSetCurrentBlock_NoChangeSkipsRefresh(t *testing.T) {
	spy := &stateAtSpy{}
	env := NewAnzeonTipEnv(anzeonEnabledChainConfig(), spy.fn())

	h1 := makeHeaderWithGasTip(t, big.NewInt(100), common.HexToHash("0x01"), big.NewInt(10))
	env.SetCurrentBlock(h1)
	before := spy.calls

	env.SetCurrentBlock(h1) // identical header: no Root change, no GasTip change
	if spy.calls != before {
		t.Fatalf("stateAt invoked on no-op SetCurrentBlock: before=%d after=%d", before, spy.calls)
	}
}

// TestSetCurrentBlock_NilHeaderNoop guards the existing nil-header early return.
func TestSetCurrentBlock_NilHeaderNoop(t *testing.T) {
	spy := &stateAtSpy{}
	env := NewAnzeonTipEnv(anzeonEnabledChainConfig(), spy.fn())

	env.SetCurrentBlock(nil)
	if env.currentBlock != nil {
		t.Fatalf("currentBlock = %v, want nil after SetCurrentBlock(nil)", env.currentBlock)
	}
	if spy.calls != 0 {
		t.Fatalf("stateAt invoked on nil header: calls=%d", spy.calls)
	}
}
