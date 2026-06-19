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

package test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"
)

// gwei returns n * 1e9 wei.
func gwei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(params.GWei))
}

// govGasTipFeeCap is a generous maxFeePerGas so that the only fee constraint
// exercised by these tests is the gas tip (priority fee), not the fee cap.
var govGasTipFeeCap = new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(params.GWei))

func requireBigEq(t *testing.T, want, got *big.Int, msg string) {
	t.Helper()
	require.NotNil(t, got, "%s: got nil", msg)
	require.Truef(t, want.Cmp(got) == 0, "%s: want %s, got %s", msg, want, got)
}

// headerGasTip reads the WBFTExtra.GasTip embedded in the header of the given block.
func (g *GovWBFT) headerGasTip(t *testing.T, num uint64) *big.Int {
	t.Helper()
	h, err := g.backend.Client().HeaderByNumber(context.TODO(), new(big.Int).SetUint64(num))
	require.NoError(t, err)
	return h.GasTip()
}

// contractGasTip reads the current gasTip value stored in the GovValidator contract.
func (g *GovWBFT) contractGasTip(t *testing.T) *big.Int {
	t.Helper()
	var result []interface{}
	require.NoError(t, g.govValidator.Call(&bind.CallOpts{}, &result, "gasTip"))
	return result[0].(*big.Int)
}

// txProposeGasTip creates a proposeGasTip transaction with an explicit gas tip cap,
// mimicking an operator wallet that submits at a fixed network tip.
func (g *GovWBFT) txProposeGasTip(t *testing.T, sender *EOA, newTip, tipCap *big.Int) (*big.Int, *types.Transaction, error) {
	var result []interface{}
	if err := g.govValidator.Call(&bind.CallOpts{From: sender.Address}, &result, "currentProposalId"); err != nil {
		return nil, nil, err
	}
	nextID := new(big.Int).Add(result[0].(*big.Int), big.NewInt(1))

	opts := NewTxOptsWithValue(t, sender, nil)
	opts.GasTipCap = new(big.Int).Set(tipCap)
	opts.GasFeeCap = new(big.Int).Set(govGasTipFeeCap)
	opts.GasLimit = 1_000_000
	tx, err := g.govValidator.Transact(opts, "proposeGasTip", newTip)
	return nextID, tx, err
}

// txApproveProposalWithTip approves a proposal using an explicit gas tip cap.
func (g *GovWBFT) txApproveProposalWithTip(t *testing.T, sender *EOA, proposalID, tipCap *big.Int) (*types.Transaction, error) {
	opts := NewTxOptsWithValue(t, sender, nil)
	opts.GasTipCap = new(big.Int).Set(tipCap)
	opts.GasFeeCap = new(big.Int).Set(govGasTipFeeCap)
	opts.GasLimit = 1_000_000
	return g.govValidator.Transact(opts, "approveProposal", proposalID)
}

// TestGasTipGovernanceLifecycle reproduces the docs/pr-77 scenario:
//
//	raise gasTip 27600 -> 30000 (applied at N+1), then restore 30000 -> 27600
//	(must apply at M+1).
//
// Operators consistently submit governance transactions at the baseline network
// tip (27600 Gwei). Raising works, but the restore proposal carries a tip that is
// now below the raised pool minimum (30000 Gwei). Because the pool admits/keeps a
// transaction based on its raw GasTipCap rather than the Anzeon effective tip
// (which, for non-authorized members, is dictated by the block header), the
// restore proposal is stranded in the pending pool and never mined.
func TestGasTipGovernanceLifecycle(t *testing.T) {
	initialTip := gwei(27600) // 27600 Gwei
	raisedTip := gwei(30000)  // 30000 Gwei

	members := []*TestCandidate{NewTestCandidate(), NewTestCandidate()}
	g, err := NewGovWBFT(t, types.GenesisAlloc{
		members[0].Operator.Address: {Balance: towei(1_000_000)},
		members[1].Operator.Address: {Balance: towei(1_000_000)},
	}, func(govValidator *params.SystemContract) {
		govValidator.Params = map[string]string{
			"members":       members[0].Operator.Address.Hex() + "," + members[1].Operator.Address.Hex(),
			"quorum":        "2",
			"expiry":        "86400",
			"memberVersion": "1",
			"gasTip":        initialTip.String(),
		}
	}, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	defer g.backend.Close()

	proposer := members[0].Operator
	approver := members[1].Operator

	// Operators consistently submit at the baseline tip.
	opTip := new(big.Int).Set(initialTip)

	// settle lets the asynchronous gasTip updater (fired on block import) propagate
	// the contract value into the txpool/worker minimum tip before the next step.
	settle := func() {
		for i := 0; i < 2; i++ {
			g.backend.Commit()
			time.Sleep(150 * time.Millisecond)
		}
	}

	// --- Step 1: initial GasTip baseline ---
	g.backend.Commit() // block 1
	requireBigEq(t, initialTip, g.contractGasTip(t), "initial contract gasTip")
	requireBigEq(t, initialTip, g.headerGasTip(t, 1), "initial header gasTip")

	// --- Steps 2-4: raise 27600 -> 30000 ---
	raiseID, tx, err := g.txProposeGasTip(t, proposer, raisedTip, opTip)
	_, err = g.ExpectedOk(tx, err)
	require.NoError(t, err, "raise proposal must be accepted into a block")

	tx, err = g.txApproveProposalWithTip(t, approver, raiseID, opTip)
	raiseReceipt, err := g.ExpectedOk(tx, err)
	require.NoError(t, err, "raise approval/execution must be accepted into a block")
	requireBigEq(t, raisedTip, g.contractGasTip(t), "contract gasTip after raise execution")

	blockN := raiseReceipt.BlockNumber.Uint64()

	// --- Step 5: Block N keeps the old value ---
	requireBigEq(t, initialTip, g.headerGasTip(t, blockN), "Block N must keep old gasTip")

	// --- Step 6: Block N+1 applies the new value ---
	g.backend.Commit()
	requireBigEq(t, raisedTip, g.headerGasTip(t, blockN+1), "Block N+1 must apply new gasTip")

	// Let the raised tip propagate into the pool minimum.
	settle()

	// --- Steps 7-9: restore 30000 -> 27600 (the previously failing path) ---
	restoreID, tx, err := g.txProposeGasTip(t, proposer, initialTip, opTip)
	_, err = g.ExpectedOk(tx, err)
	require.NoError(t, err, "restore proposal must be accepted into a block (stuck-in-pending bug)")

	tx, err = g.txApproveProposalWithTip(t, approver, restoreID, opTip)
	restoreReceipt, err := g.ExpectedOk(tx, err)
	require.NoError(t, err, "restore approval/execution must be accepted into a block")
	requireBigEq(t, initialTip, g.contractGasTip(t), "contract gasTip after restore execution")

	blockM := restoreReceipt.BlockNumber.Uint64()

	// --- Step 10: Block M keeps the previous (raised) value ---
	requireBigEq(t, raisedTip, g.headerGasTip(t, blockM), "Block M must keep raised gasTip")

	// --- Step 11: Block M+1 restores the original value ---
	g.backend.Commit()
	requireBigEq(t, initialTip, g.headerGasTip(t, blockM+1), "Block M+1 must restore original gasTip")
}
