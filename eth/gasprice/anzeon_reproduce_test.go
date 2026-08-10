// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2025 The go-stablenet Authors
// This file is part of the go-stablenet library.

package gasprice

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// Ticket LOCAL-20260810_040854: governance gasTip restore proposal stuck in pending.
//
// Reproduces the ROOT-CAUSE broken edge: AnzeonTipEnv.SetCurrentBlock
// (eth/gasprice/anzeon.go) gates its update on `currentBlock.Root != header.Root`.
//
// The header GasTip (WBFT extra) lags the world state by one block, because the
// consensus engine derives it from the PARENT state (getGasTip reads the parent
// state, consensus/wbft/engine/engine.go). So a governance gasTip change produces:
//   - the change/apply block: header carries the OLD gasTip but the NEW state root;
//   - the next idle (empty) block: header carries the CORRECTED gasTip but leaves
//     the world state unchanged, hence the SAME state root as the change block.
//
// The root guard therefore latches onto the change block's STALE header GasTip and
// never picks up the corrected value from the later same-root blocks. For an
// unauthorized (normal) sender, GetAnzeonTipCap returns this stale header GasTip as
// the effective tip cap. After a RAISE (27600 -> 30000 Gwei) the env is stuck at the
// stale-low 27600 while the miner's required tip (w.tip / pool.gasTip, driven by
// updateGasTipFromContract) correctly tracks 30000. Every unauthorized tx -- including
// the subsequent restore proposal -- then has an effective tip below the required tip
// and is filtered out of the pending set (legacypool Pending), so it is never mined.
//
// This is the ticket's asymmetry: raising works (env and required tip agree at the low
// value at submission time), restoring gets stuck (env latched below the required tip).

const (
	gweiPreChange = 27600 // gasTip before the governance change (also the restore target)
	gweiPostRaise = 30000 // gasTip after the governance raise (currently enforced on-chain)
)

// newGasTipHeader builds a header whose WBFT extra carries the given gasTip (in Gwei).
func newGasTipHeader(t *testing.T, number uint64, root common.Hash, gasTipGwei int64) *types.Header {
	t.Helper()
	extra := &types.WBFTExtra{
		VanityData:   make([]byte, types.IstanbulExtraVanity),
		RandaoReveal: []byte{},
		GasTip:       new(big.Int).Mul(big.NewInt(gasTipGwei), big.NewInt(params.GWei)),
	}
	enc, err := rlp.EncodeToBytes(extra)
	if err != nil {
		t.Fatalf("failed to encode WBFTExtra: %v", err)
	}
	return &types.Header{
		Number:  new(big.Int).SetUint64(number),
		Time:    uint64(time.Now().Unix()),
		Root:    root,
		BaseFee: big.NewInt(params.GWei),
		Extra:   enc,
	}
}

func TestReproduce_GasTipRestoreStuckByRootGuard(t *testing.T) {
	config := params.AllDevChainProtocolChanges // Anzeon enabled, all forks at genesis
	baseFee := big.NewInt(params.GWei)

	// A normal, UNAUTHORIZED sender (fresh account, never SetAuthorized).
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	signer := types.LatestSignerForChainID(config.ChainID)

	// Empty world state: the sender is not authorized, so GetAnzeonTipCap must use the
	// header's gasTip for it (the enforced network tip).
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	if err != nil {
		t.Fatalf("failed to create state: %v", err)
	}
	stateAt := func(common.Hash) (*state.StateDB, error) { return stateDB, nil }

	env := NewAnzeonTipEnv(config, stateAt)
	env.SetBaseFee(baseFee)

	// Both blocks share the SAME (non-empty) state root: the idle empty block after the
	// governance change does not alter world state, so its state root equals the change
	// block's. Only the header gasTip differs (it lags one block).
	sharedRoot := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000aa")

	// Block M: the governance RAISE-apply block. Its header gasTip still carries the OLD
	// pre-change value (27600) because it is read from the parent state.
	changeBlock := newGasTipHeader(t, 100, sharedRoot, gweiPreChange)
	// Block M+1: idle empty block. Its header gasTip now reflects the applied value
	// (30000), but the empty block leaves the world state (root) unchanged.
	idleBlock := newGasTipHeader(t, 101, sharedRoot, gweiPostRaise)

	env.SetCurrentBlock(changeBlock)
	env.SetCurrentBlock(idleBlock) // same root -> root guard skips this update

	// The value the network actually enforces now is the latest block's header gasTip.
	requiredTip := idleBlock.GasTip() // 30000 Gwei in Wei
	if requiredTip == nil {
		t.Fatalf("idle block header gasTip is nil (WBFT extra round-trip failed)")
	}

	// An unauthorized user submits a tx paying the FULL current required tip (30000).
	tipWei := new(big.Int).Mul(big.NewInt(gweiPostRaise), big.NewInt(params.GWei))
	feeCap := new(big.Int).Add(baseFee, tipWei)
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   config.ChainID,
		Nonce:     0,
		To:        &common.Address{},
		Gas:       21000,
		GasTipCap: tipWei,
		GasFeeCap: feeCap,
	}), signer, key)
	if err != nil {
		t.Fatalf("failed to sign tx: %v", err)
	}

	enforcedTipCap := env.GetAnzeonTipCap(tx)
	effectiveTip := tx.EffectiveGasTipValue(env)

	// SYMPTOM assertion (symptom_assertion): a validly-priced tx must be includable in a
	// block -- i.e. its effective tip must be >= the currently required min tip. The miner's
	// pending filter (legacypool Pending) drops a tx when EffectiveGasTipIntCmp(requiredTip,
	// env) < 0, which is exactly "stuck in pending, never mined".
	if tx.EffectiveGasTipIntCmp(requiredTip, env) < 0 {
		t.Fatalf("SYMPTOM REPRODUCED: restore-era tx stuck in pending -- effective tip %s < required %s "+
			"(enforced tip cap latched at stale %s instead of current %s). "+
			"Root cause: AnzeonTipEnv.SetCurrentBlock root guard skipped the same-root idle block, "+
			"so GetAnzeonTipCap returns the stale change-block header gasTip.",
			effectiveTip, requiredTip, enforcedTipCap, requiredTip)
	}
}
