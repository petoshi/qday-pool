// Package web serves the read-only QDAY pool dashboard and API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petoshi/qday-pool/internal/pool"
	"github.com/petoshi/qday-pool/internal/store"
	"github.com/petoshi/qday-pool/internal/stratum"
	"go.sia.tech/core/types"
)

//go:embed site
var embedded embed.FS

type snapshotter interface {
	Snapshot() stratum.Snapshot
}

type controllerSnapshotter interface {
	Snapshot() pool.Snapshot
}

type Config struct {
	StratumAddress string
	PoolFeeBPS     uint64
	PPLNSWindow    uint64
}

type Server struct {
	store      *store.Store
	stratum    snapshotter
	controller controllerSnapshotter
	cfg        Config
	static     http.Handler
	index      []byte
}

type Amount struct {
	Atomic string `json:"atomic"`
	QDAY   string `json:"qday"`
}

type BlockView struct {
	store.FoundBlock
	Reward      Amount `json:"reward"`
	Fees        Amount `json:"fees"`
	PoolFee     Amount `json:"poolFee"`
	Distributed Amount `json:"distributed"`
}

type PayoutView struct {
	store.Payout
	Amount Amount `json:"amount"`
	Fee    Amount `json:"fee"`
}

type MinerView struct {
	store.MinerStat
	Hashrate float64 `json:"hashrate"`
}

type Status struct {
	Ready     bool      `json:"ready"`
	Stratum   string    `json:"stratum"`
	UpdatedAt time.Time `json:"updatedAt"`
	Pool      struct {
		Hashrate    float64 `json:"hashrate"`
		Connected   int     `json:"connected"`
		Workers     int     `json:"workers"`
		Miners      int     `json:"miners"`
		Shares      uint64  `json:"shares"`
		Blocks      uint64  `json:"blocks"`
		RoundEffort float64 `json:"roundEffort"`
	} `json:"pool"`
	Network struct {
		Height       uint64  `json:"height"`
		Hashrate     float64 `json:"hashrate"`
		Difficulty   float64 `json:"difficulty"`
		Transactions int     `json:"templateTransactions"`
		Mempool      int     `json:"mempoolTransactions"`
		Peers        int     `json:"peers"`
		Synced       bool    `json:"synced"`
	} `json:"network"`
	Policy struct {
		Method         string  `json:"method"`
		Window         uint64  `json:"window"`
		FeePercent     float64 `json:"feePercent"`
		MaturityBlocks uint64  `json:"maturityBlocks"`
		MinimumPayout  Amount  `json:"minimumPayout"`
		PayoutFee      Amount  `json:"payoutFee"`
	} `json:"policy"`
	LastBlock *BlockView `json:"lastBlock,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func New(database *store.Store, mining snapshotter, controller controllerSnapshotter, cfg Config) (*Server, error) {
	if database == nil || mining == nil || controller == nil || strings.TrimSpace(cfg.StratumAddress) == "" {
		return nil, errors.New("store, mining server, controller and public Stratum address are required")
	}
	site, err := fs.Sub(embedded, "site")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(site, "index.html")
	if err != nil {
		return nil, err
	}
	return &Server{store: database, stratum: mining, controller: controller, cfg: cfg, static: http.FileServer(http.FS(site)), index: index}, nil
}

func parsePage(r *http.Request) (limit, offset int, err error) {
	limit, offset = 20, 0
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, errors.New("limit must be 1..100")
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 10_000_000 {
			return 0, 0, errors.New("offset is invalid")
		}
	}
	return
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func formatAmount(value string, unit string) Amount {
	result := Amount{Atomic: value, QDAY: value}
	n, ok := new(big.Int).SetString(value, 10)
	u, okUnit := new(big.Int).SetString(unit, 10)
	if !ok || !okUnit || u.Sign() <= 0 {
		return result
	}
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, u, r)
	if r.Sign() == 0 {
		result.QDAY = q.String()
		return result
	}
	decimals := len(unit) - 1
	fraction := strings.Repeat("0", max(0, decimals-len(r.String()))) + r.String()
	result.QDAY = q.String() + "." + strings.TrimRight(fraction, "0")
	return result
}

func floatFromBig(value *big.Int) float64 {
	result, _ := new(big.Float).SetInt(value).Float64()
	return result
}

func (s *Server) blockViews(blocks []store.FoundBlock, unit string) []BlockView {
	result := make([]BlockView, len(blocks))
	for i, block := range blocks {
		reward, _ := new(big.Int).SetString(block.RewardAtomic, 10)
		fee, _ := new(big.Int).SetString(block.PoolFeeAtomic, 10)
		distributed := new(big.Int)
		if reward != nil && fee != nil && reward.Cmp(fee) >= 0 {
			distributed.Sub(reward, fee)
		}
		result[i] = BlockView{FoundBlock: block, Reward: formatAmount(block.RewardAtomic, unit), Fees: formatAmount(block.FeesAtomic, unit), PoolFee: formatAmount(block.PoolFeeAtomic, unit), Distributed: formatAmount(distributed.String(), unit)}
	}
	return result
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	mining := s.stratum.Snapshot()
	controller := s.controller.Snapshot()
	work, shares, workers, miners, err := s.store.ShareSummary(time.Now().Add(-10 * time.Minute))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	round, err := s.store.RoundWork()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	blocks, err := s.store.Blocks("", 1, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	blockCount, err := s.store.BlockCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status := Status{Ready: mining.Ready && controller.Node.Synced, Stratum: s.cfg.StratumAddress, UpdatedAt: time.Now().UTC(), Error: controller.LastError}
	status.Pool.Hashrate = floatFromBig(work) / 600
	status.Pool.Connected, status.Pool.Workers, status.Pool.Miners = mining.Authorized, workers, miners
	status.Pool.Shares, status.Pool.Blocks = shares, uint64(blockCount)
	if networkWork, ok := new(big.Int).SetString(mining.NetworkWork, 10); ok && networkWork.Sign() > 0 {
		effort, _ := new(big.Float).Mul(new(big.Float).Quo(new(big.Float).SetInt(round), new(big.Float).SetInt(networkWork)), big.NewFloat(100)).Float64()
		status.Pool.RoundEffort = effort
	}
	status.Network.Height, status.Network.Hashrate, status.Network.Difficulty = controller.Node.Height, mining.NetworkHashrate, mining.NetworkDifficulty
	status.Network.Transactions, status.Network.Mempool = mining.Transactions, controller.Node.Mempool
	status.Network.Peers, status.Network.Synced = controller.Node.Peers, controller.Node.Synced
	status.Policy.Method, status.Policy.Window, status.Policy.FeePercent, status.Policy.MaturityBlocks = "PPLNS", s.cfg.PPLNSWindow, float64(s.cfg.PoolFeeBPS)/100, 60
	status.Policy.MinimumPayout = formatAmount(controller.MinimumPayout, controller.Node.Unit)
	status.Policy.PayoutFee = formatAmount(controller.PayoutFee, controller.Node.Unit)
	if len(blocks) != 0 {
		view := s.blockViews(blocks, controller.Node.Unit)[0]
		status.LastBlock = &view
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) blocks(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	blocks, err := s.store.Blocks("", limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := s.store.BlockCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocks": s.blockViews(blocks, s.controller.Snapshot().Node.Unit), "total": total, "limit": limit, "offset": offset})
}

func (s *Server) miners(w http.ResponseWriter, r *http.Request) {
	limit, _, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	miners, err := s.store.TopMiners(time.Now().Add(-10*time.Minute), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result := make([]MinerView, len(miners))
	for i, miner := range miners {
		work, _ := new(big.Int).SetString(miner.Work, 10)
		result[i] = MinerView{MinerStat: miner, Hashrate: floatFromBig(work) / 600}
	}
	writeJSON(w, http.StatusOK, map[string]any{"miners": result, "windowSeconds": 600})
}

func (s *Server) payouts(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	payouts, err := s.store.Payouts(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := s.store.PayoutCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	unit := s.controller.Snapshot().Node.Unit
	result := make([]PayoutView, len(payouts))
	for i, payout := range payouts {
		result[i] = PayoutView{Payout: payout, Amount: formatAmount(payout.AmountAtomic, unit), Fee: formatAmount(payout.FeeAtomic, unit)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": result, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	address := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parsed, err := types.ParseQdayAddress(address)
	if err != nil || parsed.String() != address {
		writeError(w, http.StatusBadRequest, errors.New("invalid canonical QDAY address"))
		return
	}
	account, err := s.store.Account(address)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	unit := s.controller.Snapshot().Node.Unit
	writeJSON(w, http.StatusOK, map[string]any{
		"address": account.Address, "mature": formatAmount(account.MatureAtomic, unit), "pending": formatAmount(account.PendingAtomic, unit),
		"paid": formatAmount(account.PaidAtomic, unit), "balance": formatAmount(account.BalanceAtomic, unit), "shares": account.Shares, "workers": account.Workers,
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := s.stratum.Snapshot().Ready && s.controller.Snapshot().Node.Synced
		if !ready {
			writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ready": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ready": true})
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/blocks", s.blocks)
	mux.HandleFunc("GET /api/miners", s.miners)
	mux.HandleFunc("GET /api/payouts", s.payouts)
	mux.HandleFunc("GET /api/accounts/", s.account)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, ".") {
			s.static.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.index)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		mux.ServeHTTP(w, r)
	})
}
