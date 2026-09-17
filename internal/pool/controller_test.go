package pool

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/store"
)

type controllerNode struct {
	status   nodeapi.Status
	response string
	request  nodeapi.PayoutRequest
	defends  int
}

func (n *controllerNode) Status(context.Context) (nodeapi.Status, error) { return n.status, nil }
func (n *controllerNode) BlockStatus(context.Context, string) (nodeapi.BlockStatus, error) {
	return nodeapi.BlockStatus{Known: true, Canonical: true, Height: 1, TipHeight: 61, Confirmations: 61, MaturityHeight: 61}, nil
}
func (n *controllerNode) Payout(_ context.Context, request nodeapi.PayoutRequest) (nodeapi.PayoutResponse, error) {
	n.request = request
	return nodeapi.PayoutResponse{RequestID: request.RequestID, Transaction: "transaction", Outputs: len(request.Outputs), Status: n.response}, nil
}
func (n *controllerNode) Defend(context.Context) (nodeapi.DefendResponse, error) {
	n.defends++
	return nodeapi.DefendResponse{Status: "idle"}, nil
}

func TestControllerMaturesAndConfirmsPayout(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	cutoff, err := database.RecordShare(store.Share{CreatedAt: now, Height: 1, ParentID: "parent", JobID: "job", Address: "miner", Worker: "rig", Difficulty: 1, Work: big.NewInt(100), Hash: "share"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AllocateBlock(store.FoundBlock{ID: "block", Height: 1, FoundAt: now.Add(time.Second), FoundByAddress: "miner", FoundByWorker: "rig", RewardAtomic: "10000000", FeesAtomic: "2000000", MaturityHeight: 61, CutoffShareID: cutoff}, big.NewInt(100), 100); err != nil {
		t.Fatal(err)
	}
	node := &controllerNode{status: nodeapi.Status{Network: "qday-mainnet", Height: 61, Synced: true, Unlocked: true, HasWallet: true, Address: "pool", Unit: "1000000", BalanceReady: true}, response: "queued"}
	controller, err := New(node, database, Config{MinimumPayout: "1", PayoutFee: "0.001"})
	if err != nil {
		t.Fatal(err)
	}
	controller.syncBlocks(context.Background())
	controller.processPayouts(context.Background())
	if node.request.FeeAtomic != "1000" || len(node.request.Outputs) != 1 || node.request.Outputs[0].AmountAtomic != "9900000" {
		t.Fatalf("wrong payout request: %+v", node.request)
	}
	payouts, err := database.Payouts(10, 0)
	if err != nil || len(payouts) != 1 || payouts[0].Status != "submitted" {
		t.Fatalf("wrong submitted payout: %+v, %v", payouts, err)
	}
	node.response = "confirmed"
	controller.processPayouts(context.Background())
	payouts, err = database.Payouts(10, 0)
	if err != nil || len(payouts) != 1 || payouts[0].Status != "confirmed" {
		t.Fatalf("wrong confirmed payout: %+v, %v", payouts, err)
	}
	account, err := database.Account("miner")
	if err != nil || account.BalanceAtomic != "0" || account.PaidAtomic != "9900000" {
		t.Fatalf("wrong final account: %+v, %v", account, err)
	}
}

func TestControllerRunsDefendAfterPQDay(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node := &controllerNode{status: nodeapi.Status{Network: "qday-mainnet", Height: 100, Synced: true, Unlocked: true, HasWallet: true, Address: "pool", Unit: "1000000", BalanceReady: true, Qday: true}}
	controller, err := New(node, database, Config{MinimumPayout: "1", PayoutFee: "0.001"})
	if err != nil {
		t.Fatal(err)
	}
	controller.processPayouts(context.Background())
	if node.defends != 1 {
		t.Fatalf("DEFEND calls = %d, want 1", node.defends)
	}
}
