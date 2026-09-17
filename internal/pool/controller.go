// Package pool matures found blocks and submits idempotent miner payouts.
package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/petoshi/qday-pool/internal/amount"
	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/store"
)

type nodeClient interface {
	Status(context.Context) (nodeapi.Status, error)
	BlockStatus(context.Context, string) (nodeapi.BlockStatus, error)
	Payout(context.Context, nodeapi.PayoutRequest) (nodeapi.PayoutResponse, error)
	Defend(context.Context) (nodeapi.DefendResponse, error)
}

type Config struct {
	BlockPoll      time.Duration
	PayoutInterval time.Duration
	MinimumPayout  string
	PayoutFee      string
	MaxOutputs     int
	Logger         *slog.Logger
}

type Snapshot struct {
	Node          nodeapi.Status `json:"node"`
	MinimumPayout string         `json:"minimumPayoutAtomic"`
	PayoutFee     string         `json:"payoutFeeAtomic"`
	LastChecked   time.Time      `json:"lastChecked"`
	LastPayout    time.Time      `json:"lastPayout,omitempty"`
	LastError     string         `json:"lastError,omitempty"`
}

type Controller struct {
	node  nodeClient
	store *store.Store
	cfg   Config

	mu       sync.Mutex
	snapshot Snapshot
}

func New(node nodeClient, database *store.Store, cfg Config) (*Controller, error) {
	if node == nil || database == nil || cfg.MinimumPayout == "" || cfg.PayoutFee == "" {
		return nil, errors.New("node, store and positive payout policy are required")
	}
	if cfg.BlockPoll == 0 {
		cfg.BlockPoll = 10 * time.Second
	}
	if cfg.PayoutInterval == 0 {
		cfg.PayoutInterval = time.Minute
	}
	if cfg.MaxOutputs == 0 {
		cfg.MaxOutputs = 100
	}
	if cfg.MaxOutputs < 1 || cfg.MaxOutputs > 100 {
		return nil, errors.New("max payout outputs must be 1..100")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Controller{node: node, store: database, cfg: cfg}, nil
}

func (c *Controller) setStatus(status nodeapi.Status, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot.Node = status
	c.snapshot.LastChecked = time.Now().UTC()
	c.snapshot.LastError = ""
	if err != nil {
		c.snapshot.LastError = err.Error()
	}
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

func (c *Controller) Run(ctx context.Context) {
	blockTicker := time.NewTicker(c.cfg.BlockPoll)
	payoutTicker := time.NewTicker(c.cfg.PayoutInterval)
	defer blockTicker.Stop()
	defer payoutTicker.Stop()
	c.syncBlocks(ctx)
	c.processPayouts(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-blockTicker.C:
			c.syncBlocks(ctx)
		case <-payoutTicker.C:
			c.processPayouts(ctx)
		}
	}
}

func (c *Controller) syncBlocks(ctx context.Context) {
	status, err := c.node.Status(ctx)
	c.setStatus(status, err)
	if err != nil {
		c.cfg.Logger.Warn("QDAY node status unavailable", "error", err)
		return
	}
	blocks, err := c.store.PendingBlocks(1000)
	if err != nil {
		c.setStatus(status, err)
		return
	}
	for _, block := range blocks {
		blockStatus, err := c.node.BlockStatus(ctx, block.ID)
		if err != nil {
			_ = c.store.SetBlockError(block.ID, err.Error())
			continue
		}
		state, err := c.store.SetBlockState(block.ID, blockStatus.Known, blockStatus.Canonical, blockStatus.Confirmations, blockStatus.TipHeight, "")
		if err != nil {
			c.cfg.Logger.Error("could not update pool block", "block", block.ID, "error", err)
		} else if state == "mature" {
			c.cfg.Logger.Info("pool block matured", "block", block.ID, "height", block.Height)
		} else if state == "orphan" {
			c.cfg.Logger.Warn("pool block orphaned", "block", block.ID, "height", block.Height)
		}
	}
}

func (c *Controller) processPayouts(ctx context.Context) {
	status, err := c.node.Status(ctx)
	c.setStatus(status, err)
	if err != nil || !status.Synced || !status.Unlocked || !status.BalanceReady || status.Address == "" || status.Unit == "" {
		return
	}
	if status.Qday {
		defend, err := c.node.Defend(ctx)
		if err != nil {
			c.setStatus(status, err)
			c.cfg.Logger.Warn("pool wallet DEFEND unavailable", "error", err)
			return
		}
		if defend.Status == "queued" {
			c.cfg.Logger.Info("pool wallet DEFEND queued", "transaction", defend.Transaction)
		}
	}
	minimum, err := amount.Parse(c.cfg.MinimumPayout, status.Unit)
	if err != nil || minimum.Sign() <= 0 {
		c.setStatus(status, errors.New("invalid minimum payout for the current QDAY unit"))
		return
	}
	fee, err := amount.Parse(c.cfg.PayoutFee, status.Unit)
	if err != nil {
		c.setStatus(status, errors.New("invalid payout fee for the current QDAY unit"))
		return
	}
	c.mu.Lock()
	c.snapshot.MinimumPayout = minimum.String()
	c.snapshot.PayoutFee = fee.String()
	c.mu.Unlock()
	prepared, err := c.store.ActivePayouts(10)
	if err != nil {
		c.setStatus(status, err)
		return
	}
	if len(prepared) == 0 {
		payout, err := c.store.PreparePayouts(minimum, fee, c.cfg.MaxOutputs)
		if err != nil {
			c.setStatus(status, err)
			return
		}
		if payout != nil {
			prepared = append(prepared, *payout)
		}
	}
	for _, payout := range prepared {
		request := nodeapi.PayoutRequest{RequestID: payout.RequestID, FromAddress: status.Address, ExpectedUnitAtomic: status.Unit, FeeAtomic: payout.FeeAtomic, Outputs: make([]nodeapi.PayoutOutput, len(payout.Items))}
		for i, item := range payout.Items {
			request.Outputs[i] = nodeapi.PayoutOutput{Address: item.Address, AmountAtomic: item.AmountAtomic}
		}
		response, err := c.node.Payout(ctx, request)
		if err != nil {
			_ = c.store.PayoutError(payout.ID, err.Error())
			c.cfg.Logger.Warn("pool payout remains queued", "request", payout.RequestID, "error", err)
			continue
		}
		state := "submitted"
		if response.Status == "confirmed" {
			state = "confirmed"
		}
		if err := c.store.SetPayoutState(payout.ID, response.Transaction, state); err != nil {
			c.cfg.Logger.Error("idempotent payout succeeded but accounting update failed", "request", payout.RequestID, "transaction", response.Transaction, "error", err)
			continue
		}
		c.mu.Lock()
		c.snapshot.LastPayout = time.Now().UTC()
		c.mu.Unlock()
		c.cfg.Logger.Info("pool payout updated", "request", payout.RequestID, "transaction", response.Transaction, "status", state, "outputs", len(payout.Items), "amountAtomic", payout.AmountAtomic)
	}
}
