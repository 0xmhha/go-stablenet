// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2025 The go-stablenet Authors
// This file is part of the go-stablenet library.

package test

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// BaseTxProposeGasTip creates a GasTip governance proposal (proposeGasTip) and
// returns the predicted proposal id together with the (already-sent) transaction.
func (g *GovWBFT) BaseTxProposeGasTip(t *testing.T, contract *bind.BoundContract, sender *EOA, newTip *big.Int) (*big.Int, *types.Transaction, error) {
	var result []interface{}
	err := contract.Call(&bind.CallOpts{From: sender.Address, Context: context.TODO()}, &result, "currentProposalId")
	if err != nil {
		return nil, nil, err
	}
	currentId := result[0].(*big.Int)
	nextId := new(big.Int).Add(currentId, big.NewInt(1))

	tx, err := contract.Transact(NewTxOptsWithValue(t, sender, nil), "proposeGasTip", newTip)
	return nextId, tx, err
}

// TestGovValidator_GasTipRestorePendingPool is the PR-77 regression test.
//
// Scenario (all gas tips read from block header extradata WBFTExtra.GasTip,
// never from the governance contract):
//
//  1. initial GasTip = 27600 Gwei
//  2. governance raises GasTip -> 30000 Gwei (executed in block N)
//     - block N still reports 27600, block N+1 reports 30000 (applied at N+1)
//  3. governance restores GasTip -> 27600 Gwei (executed in block M)
//     - block M still reports 30000, block M+1 reports 27600 (applied at M+1)
//
// Before the fix, the restore proposal transaction got stuck in the pending pool:
// its Anzeon effective tip was cached at submission time (to the then-current
// network tip) and never refreshed, so after the raise it was permanently below
// the new tip floor and never included in a block.
func TestGovValidator_GasTipRestorePendingPool(t *testing.T) {
	initGov(t)
	defer g.backend.Close()

	ctx := context.Background()
	client := g.backend.Client()

	gwei := big.NewInt(1_000_000_000)
	initialTip := new(big.Int).Mul(big.NewInt(27600), gwei) // 27600 Gwei (params.InitialGasTip)
	raisedTip := new(big.Int).Mul(big.NewInt(30000), gwei)  // 30000 Gwei

	readGasTip := func(num uint64) *big.Int {
		h, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(num))
		require.NoError(t, err, "HeaderByNumber(%d)", num)
		extra, err := types.ExtractWBFTExtra(h)
		require.NoError(t, err, "ExtractWBFTExtra(block %d)", num)
		require.NotNil(t, extra.GasTip, "block %d has nil GasTip in extradata", num)
		return extra.GasTip
	}

	// mineUntilMined mines up to maxBlocks blocks waiting for tx inclusion. It fails
	// (instead of hanging forever like bind.WaitMined) if the tx never gets included,
	// which is the PR-77 "stuck in pending pool" symptom.
	mineUntilMined := func(label string, tx *types.Transaction, txErr error, maxBlocks int) *types.Receipt {
		require.NoError(t, txErr, "%s: failed to create/send tx", label)
		for i := 0; i < maxBlocks; i++ {
			g.backend.Commit()
			if rcpt, err := client.TransactionReceipt(ctx, tx.Hash()); err == nil && rcpt != nil {
				return rcpt
			}
		}
		t.Fatalf("%s tx=%s (gasTipCap=%s) was not included after %d blocks — stuck in pending pool",
			label, tx.Hash().Hex(), tx.GasTipCap(), maxBlocks)
		return nil
	}

	// proposeAndApprove submits a GasTip proposal, approves it to reach quorum (2),
	// and returns the block number in which the proposal was executed.
	proposeAndApprove := func(phase string, newTip *big.Int) uint64 {
		proposalId, proposeTx, err := g.BaseTxProposeGasTip(t, g.govValidator, customValidators[0].Operator, newTip)
		mineUntilMined(phase+":propose", proposeTx, err, 5)

		approveTx, err := g.BaseTxApproveProposal(t, g.govValidator, customValidators[1].Operator, proposalId)
		rcpt := mineUntilMined(phase+":approve(execute)", approveTx, err, 5)
		return rcpt.BlockNumber.Uint64()
	}

	head, err := client.BlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, readGasTip(head).Cmp(initialTip),
		"initial header GasTip should be 27600 Gwei, got %s", readGasTip(head))

	// === 1st proposal: raise 27600 -> 30000 ===
	n := proposeAndApprove("raise", raisedTip)
	g.backend.Commit() // mine block N+1 (empty) so we can read its header
	t.Logf("[raise] N=%d GasTip=%s ; N+1=%d GasTip=%s", n, readGasTip(n), n+1, readGasTip(n+1))
	require.Equal(t, 0, readGasTip(n).Cmp(initialTip),
		"block N (%d) must still report old tip 27600, got %s", n, readGasTip(n))
	require.Equal(t, 0, readGasTip(n+1).Cmp(raisedTip),
		"block N+1 (%d) must report new tip 30000, got %s", n+1, readGasTip(n+1))

	// === 2nd proposal: restore 30000 -> 27600 ===
	m := proposeAndApprove("restore", initialTip)
	g.backend.Commit() // mine block M+1 (empty)
	t.Logf("[restore] M=%d GasTip=%s ; M+1=%d GasTip=%s", m, readGasTip(m), m+1, readGasTip(m+1))
	require.Equal(t, 0, readGasTip(m).Cmp(raisedTip),
		"block M (%d) must still report 30000 (not yet restored), got %s", m, readGasTip(m))
	require.Equal(t, 0, readGasTip(m+1).Cmp(initialTip),
		"block M+1 (%d) must report restored tip 27600, got %s", m+1, readGasTip(m+1))
}
