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

// txProposeGasTipSuggested submits a proposeGasTip tx WITHOUT an explicit gas tip,
// so the binding falls back to SuggestGasTipCap — i.e. exactly what a real wallet does.
func (g *GovWBFT) txProposeGasTipSuggested(t *testing.T, sender *EOA, newTip *big.Int) (*big.Int, *types.Transaction, error) {
	var result []interface{}
	if err := g.govValidator.Call(&bind.CallOpts{From: sender.Address}, &result, "currentProposalId"); err != nil {
		return nil, nil, err
	}
	nextID := new(big.Int).Add(result[0].(*big.Int), big.NewInt(1))
	tx, err := g.govValidator.Transact(NewTxOptsWithValue(t, sender, nil), "proposeGasTip", newTip)
	return nextID, tx, err
}

// TestGasTipGovernanceRealistic mirrors docs/pr-77 but uses SuggestGasTipCap (no forced tip),
// reproducing the reported symptom: the restore proposal stays in the pending pool and is never mined.
func TestGasTipGovernanceRealistic(t *testing.T) {
	initialTip := gwei(27600)
	raisedTip := gwei(30000)

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

	settle := func() {
		for i := 0; i < 2; i++ {
			g.backend.Commit()
			time.Sleep(150 * time.Millisecond)
		}
	}

	g.backend.Commit()
	requireBigEq(t, initialTip, g.headerGasTip(t, 1), "initial header gasTip")

	// raise 27600 -> 30000
	raiseID, tx, err := g.txProposeGasTipSuggested(t, proposer, raisedTip)
	_, err = g.ExpectedOk(tx, err)
	require.NoError(t, err, "raise proposal accepted")
	tx, err = g.govValidator.Transact(NewTxOptsWithValue(t, approver, nil), "approveProposal", raiseID)
	raiseReceipt, err := g.ExpectedOk(tx, err)
	require.NoError(t, err, "raise approval accepted")
	requireBigEq(t, raisedTip, g.contractGasTip(t), "contract gasTip after raise")

	blockN := raiseReceipt.BlockNumber.Uint64()
	requireBigEq(t, initialTip, g.headerGasTip(t, blockN), "Block N keeps old gasTip")
	g.backend.Commit()
	requireBigEq(t, raisedTip, g.headerGasTip(t, blockN+1), "Block N+1 applies new gasTip")

	settle()

	suggested, serr := g.backend.Client().SuggestGasTipCap(context.TODO())
	require.NoError(t, serr)
	t.Logf("after raise+settle: SuggestGasTipCap=%s, contractGasTip=%s", suggested, g.contractGasTip(t))

	// restore 30000 -> 27600 using suggested tip (realistic wallet behavior)
	restoreID, tx, err := g.txProposeGasTipSuggested(t, proposer, initialTip)
	_, err = g.ExpectedOk(tx, err)
	require.NoError(t, err, "restore proposal accepted")
	tx, err = g.govValidator.Transact(NewTxOptsWithValue(t, approver, nil), "approveProposal", restoreID)
	restoreReceipt, err := g.ExpectedOk(tx, err)
	require.NoError(t, err, "restore approval accepted")
	requireBigEq(t, initialTip, g.contractGasTip(t), "contract gasTip after restore")

	blockM := restoreReceipt.BlockNumber.Uint64()
	requireBigEq(t, raisedTip, g.headerGasTip(t, blockM), "Block M keeps raised gasTip")
	g.backend.Commit()
	requireBigEq(t, initialTip, g.headerGasTip(t, blockM+1), "Block M+1 restores original gasTip")
}
