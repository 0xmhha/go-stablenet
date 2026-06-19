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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// AnzeonTipEnv manages Anzeon-specific gas tip calculation environment for transaction pool.
// It maintains state for dynamic gas tip calculations based on block headers and account authorization.
type AnzeonTipEnv struct {
	config       *params.ChainConfig
	stateAt      func(common.Hash) (*state.StateDB, error)
	currentBlock *types.Header
	currentState *state.StateDB
	// currentHeaderTip caches the decoded WBFTExtra.GasTip of currentBlock so that
	// GetAnzeonTipCap does not re-decode the header extradata on every call (e.g.
	// during priced-list reheaps). It is refreshed whenever currentBlock changes.
	currentHeaderTip *big.Int
	baseFee          *big.Int
	signer           types.Signer
}

// NewAnzeonTipEnv creates a new AnzeonTipEnv instance for use in transaction pool.
func NewAnzeonTipEnv(config *params.ChainConfig, stateAt func(common.Hash) (*state.StateDB, error)) *AnzeonTipEnv {
	return &AnzeonTipEnv{
		config:  config,
		stateAt: stateAt,
	}
}

// SetCurrentBlock updates the current block header and signer.
// This should be called when the blockchain head changes.
func (env *AnzeonTipEnv) SetCurrentBlock(header *types.Header) {
	if header == nil {
		return
	}
	// Already pointed at this exact header; nothing to refresh.
	if env.currentBlock != nil && env.currentBlock.Hash() == header.Hash() {
		return
	}
	// Reload the (expensive) state snapshot only when the state root actually changes.
	// Consecutive blocks frequently share a state root (e.g. empty blocks), so this
	// avoids redundant state lookups.
	if env.currentBlock == nil || env.currentBlock.Root != header.Root {
		if header.Root != (common.Hash{}) {
			env.currentState, _ = env.stateAt(header.Root)
		} else {
			env.currentState = nil
		}
	}
	env.currentBlock = header
	// The governance gas tip lives in the header extradata (WBFTExtra.GasTip), not in
	// the state trie, so it must be refreshed for every new header even when the state
	// root is unchanged. Empty blocks following a gasTip change share the previous
	// block's state root while carrying the new header tip; keying purely on Root would
	// otherwise leave a stale tip and wrongly filter normal-account txs (PR-77).
	env.currentHeaderTip = header.GasTip()
	env.signer = types.MakeSigner(env.config, header.Number, header.Time)
}

// SetBaseFee updates the base fee for gas price calculations.
func (env *AnzeonTipEnv) SetBaseFee(baseFee *big.Int) {
	env.baseFee = baseFee
}

// IsValid returns true if Anzeon is enabled and the environment is properly initialized.
func (env *AnzeonTipEnv) IsAnzeon() bool {
	return env.config != nil && env.config.AnzeonEnabled()
}

// GetBaseFee returns the current base fee.
// It returns the explicitly set base fee, or falls back to the current block's base fee.
func (env *AnzeonTipEnv) GetBaseFee() *big.Int {
	if env.baseFee != nil {
		return env.baseFee
	}
	if env.currentBlock != nil && env.currentBlock.BaseFee != nil {
		return env.currentBlock.BaseFee
	}
	return nil
}

// GetAnzeonTipCap returns the effective gas tip cap for a transaction in the Anzeon network.
// For unauthorized accounts, it returns the block header's gas tip.
// For authorized accounts (validators/minters), it returns the transaction's original gas tip cap.
func (env *AnzeonTipEnv) GetAnzeonTipCap(tx *types.Transaction) *big.Int {
	// If environment is not fully initialized, return transaction's gas tip cap
	if !env.IsAnzeon() || env.signer == nil || env.currentBlock == nil {
		return tx.GasTipCap()
	}

	// Get sender address
	from, err := types.Sender(env.signer, tx)
	if err != nil {
		return tx.GasTipCap()
	}

	// Check if sender is authorized by querying state
	// First try to use cached state, then fall back to stateAt
	// Lazy load state if not cached
	if env.currentState == nil &&
		env.stateAt != nil &&
		env.currentBlock.Root != (common.Hash{}) {
		env.currentState, _ = env.stateAt(env.currentBlock.Root)
	}

	// For unauthorized accounts, use block header's gas tip (cached per block)
	if env.currentState != nil &&
		!env.currentState.IsAuthorized(from) &&
		env.currentHeaderTip != nil {
		return env.currentHeaderTip
	}

	// For authorized accounts or if state is unavailable, use transaction's gas tip cap
	return tx.GasTipCap()
}
