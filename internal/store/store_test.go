package store

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPPLNSAllocationAndPayoutAccounting(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().UTC().Add(-time.Minute)
	shares := []Share{
		{CreatedAt: base.Add(time.Second), Height: 9, ParentID: "p", JobID: "1", Address: "a", Worker: "old", Difficulty: 1, Work: big.NewInt(60), Hash: "01"},
		{CreatedAt: base.Add(2 * time.Second), Height: 9, ParentID: "p", JobID: "2", Address: "b", Worker: "rig", Difficulty: 1, Work: big.NewInt(30), Hash: "02"},
		{CreatedAt: base.Add(3 * time.Second), Height: 9, ParentID: "p", JobID: "3", Address: "a", Worker: "new", Difficulty: 1, Work: big.NewInt(20), Hash: "03"},
		{CreatedAt: base.Add(4 * time.Second), Height: 9, ParentID: "p", JobID: "4", Address: "c", Worker: "rig", Difficulty: 1, Work: big.NewInt(10), Hash: "04"},
	}
	var cutoff int64
	for _, share := range shares {
		var err error
		if cutoff, err = s.RecordShare(share); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordShare(Share{CreatedAt: base.Add(4 * time.Second), Height: 9, ParentID: "p", JobID: "late", Address: "late", Worker: "rig", Difficulty: 1, Work: big.NewInt(1000), Hash: "05"}); err != nil {
		t.Fatal(err)
	}
	allocations, err := s.AllocateBlock(FoundBlock{ID: "block", Height: 10, FoundAt: base.Add(5 * time.Second), FoundByAddress: "c", FoundByWorker: "rig", RewardAtomic: "1001", FeesAtomic: "101", MaturityHeight: 70, CutoffShareID: cutoff}, big.NewInt(50), 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "396", "b": "396", "c": "198"}
	if len(allocations) != len(want) {
		t.Fatalf("got %d allocations, want %d", len(allocations), len(want))
	}
	for _, allocation := range allocations {
		if want[allocation.Address] != allocation.AmountAtomic {
			t.Fatalf("allocation for %s = %s, want %s", allocation.Address, allocation.AmountAtomic, want[allocation.Address])
		}
	}
	blocks, err := s.Blocks("", 10, 0)
	if err != nil || len(blocks) != 1 || blocks[0].PoolFeeAtomic != "11" || blocks[0].SelectedWork != "50" {
		t.Fatalf("wrong stored block: %+v, %v", blocks, err)
	}
	if state, err := s.SetBlockState("block", true, true, 61, 70, ""); err != nil || state != "mature" {
		t.Fatalf("maturity = %q, %v", state, err)
	}
	payout, err := s.PreparePayouts(big.NewInt(200), big.NewInt(1), 100)
	if err != nil || payout == nil || len(payout.Items) != 2 || payout.AmountAtomic != "792" {
		t.Fatalf("wrong payout: %+v, %v", payout, err)
	}
	if err := s.SetPayoutState(payout.ID, "transaction", "submitted"); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActivePayouts(10)
	if err != nil || len(active) != 1 || len(active[0].Items) != 2 {
		t.Fatalf("wrong active payouts: %+v, %v", active, err)
	}
	if err := s.SetPayoutState(payout.ID, "transaction", "confirmed"); err != nil {
		t.Fatal(err)
	}
	account, err := s.Account("a")
	if err != nil || account.BalanceAtomic != "0" || account.PaidAtomic != "396" {
		t.Fatalf("wrong paid account: %+v, %v", account, err)
	}
}

func TestPruneSharesKeepsRequiredWork(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if _, err := s.RecordShare(Share{CreatedAt: now.Add(time.Duration(i) * time.Second), Height: 1, ParentID: "p", JobID: "j", Address: "a", Worker: "w", Difficulty: 1, Work: big.NewInt(10), Hash: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneShares(big.NewInt(25)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM shares`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("retained %d shares, want 3: %v", count, err)
	}
}

func TestBlockObservationErrorDoesNotCreateOrphan(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	cutoff, err := s.RecordShare(Share{CreatedAt: now, Height: 10, ParentID: "p", JobID: "j", Address: "a", Worker: "w", Difficulty: 1, Work: big.NewInt(10), Hash: "observation-share"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllocateBlock(FoundBlock{ID: "observed", Height: 10, FoundAt: now, FoundByAddress: "a", FoundByWorker: "w", RewardAtomic: "100", FeesAtomic: "0", MaturityHeight: 70, CutoffShareID: cutoff}, big.NewInt(10), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBlockError("observed", "temporary node timeout"); err != nil {
		t.Fatal(err)
	}
	blocks, err := s.PendingBlocks(10)
	if err != nil || len(blocks) != 1 || blocks[0].Status != "pending" || blocks[0].LastError != "temporary node timeout" {
		t.Fatalf("temporary observation changed block state: %+v, %v", blocks, err)
	}
}
