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

package types

import (
	"math/big"
	"sync"
	"testing"
)

// mockAnzeonEnv is a minimal AnzeonGasTipEnv for testing EffectiveGasTip.
type mockAnzeonEnv struct {
	baseFee   *big.Int
	anzeonTip *big.Int // value returned by GetAnzeonTipCap
	isAnzeon  bool
}

func (m *mockAnzeonEnv) IsAnzeon() bool                          { return m.isAnzeon }
func (m *mockAnzeonEnv) GetBaseFee() *big.Int                    { return m.baseFee }
func (m *mockAnzeonEnv) GetAnzeonTipCap(_ *Transaction) *big.Int { return m.anzeonTip }
func (m *mockAnzeonEnv) SetCurrentBlock(_ *Header)               {}

// TestTransaction_AnzeonTipCap_ClearAndRefresh verifies Set/Get/Clear semantics
// on the atomic.Pointer[big.Int]-backed anzeonTipCap field.
func TestTransaction_AnzeonTipCap_ClearAndRefresh(t *testing.T) {
	tx := NewTx(&DynamicFeeTx{
		GasTipCap: big.NewInt(30000),
		GasFeeCap: big.NewInt(50000),
	})

	// 1. nil by default.
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Fatalf("expected nil initially, got %v", got)
	}

	// 2. Set.
	tx.SetAnzeonTipCap(big.NewInt(30000))
	got := tx.GetAnzeonTipCap()
	if got == nil || got.Cmp(big.NewInt(30000)) != 0 {
		t.Fatalf("expected 30000 after Set, got %v", got)
	}

	// 3. Set stores a copy — mutating caller's value must not corrupt the cache.
	original := big.NewInt(99999)
	tx.SetAnzeonTipCap(original)
	original.SetInt64(0) // mutate
	if cached := tx.GetAnzeonTipCap(); cached.Sign() == 0 {
		t.Fatalf("cache was mutated through caller's pointer — Set must store a copy")
	}

	// 4. Clear.
	tx.ClearAnzeonTipCap()
	if got := tx.GetAnzeonTipCap(); got != nil {
		t.Fatalf("expected nil after Clear, got %v", got)
	}

	// 5. Re-set with a different value.
	tx.SetAnzeonTipCap(big.NewInt(27600))
	got = tx.GetAnzeonTipCap()
	if got == nil || got.Cmp(big.NewInt(27600)) != 0 {
		t.Fatalf("expected 27600 after re-Set, got %v", got)
	}

	// 6. SetAnzeonTipCap(nil) is a no-op.
	tx.SetAnzeonTipCap(nil)
	got = tx.GetAnzeonTipCap()
	if got == nil || got.Cmp(big.NewInt(27600)) != 0 {
		t.Fatalf("SetAnzeonTipCap(nil) must be a no-op; got %v", got)
	}
}

// TestTransaction_EffectiveGasTip_AfterClear verifies that EffectiveGasTip()
// honours the stale cache before Clear, then uses the fresh env value after Clear.
func TestTransaction_EffectiveGasTip_AfterClear(t *testing.T) {
	// Unauthorized-account scenario: env.GetAnzeonTipCap returns headerTip.
	env := &mockAnzeonEnv{
		baseFee:   big.NewInt(1000),
		anzeonTip: big.NewInt(27600),
		isAnzeon:  true,
	}

	tx := NewTx(&DynamicFeeTx{
		GasTipCap: big.NewInt(30000),
		GasFeeCap: big.NewInt(50000),
	})

	// Before caching: EffectiveGasTip should query env (returns 27600).
	tip, err := tx.EffectiveGasTip(env)
	if err != nil {
		t.Fatalf("EffectiveGasTip error: %v", err)
	}
	if tip.Cmp(big.NewInt(27600)) != 0 {
		t.Fatalf("expected env value 27600 (uncached), got %v", tip)
	}

	// Manually prime the cache with stale value (simulates admission at old minTip=30000).
	tx.SetAnzeonTipCap(big.NewInt(30000))

	// With stale cache: EffectiveGasTip returns cached 30000, not env's 27600.
	env.anzeonTip = big.NewInt(27600) // env already says 27600
	tip, err = tx.EffectiveGasTip(env)
	if err != nil {
		t.Fatalf("EffectiveGasTip error: %v", err)
	}
	if tip.Cmp(big.NewInt(30000)) != 0 {
		t.Fatalf("expected stale cached 30000, got %v", tip)
	}

	// After Clear: EffectiveGasTip must re-query env and return 27600.
	tx.ClearAnzeonTipCap()
	tip, err = tx.EffectiveGasTip(env)
	if err != nil {
		t.Fatalf("EffectiveGasTip error: %v", err)
	}
	if tip.Cmp(big.NewInt(27600)) != 0 {
		t.Fatalf("expected fresh env value 27600 after Clear, got %v", tip)
	}
}

// TestTransaction_AnzeonTipCap_RaceConcurrency runs concurrent Set/Clear/Get
// to confirm no data race (run with -race).
func TestTransaction_AnzeonTipCap_RaceConcurrency(t *testing.T) {
	tx := NewTx(&DynamicFeeTx{
		GasTipCap: big.NewInt(30000),
		GasFeeCap: big.NewInt(50000),
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			tx.SetAnzeonTipCap(big.NewInt(30000))
		}()
		go func() {
			defer wg.Done()
			tx.ClearAnzeonTipCap()
		}()
		go func() {
			defer wg.Done()
			_ = tx.GetAnzeonTipCap()
		}()
	}
	wg.Wait()
}
