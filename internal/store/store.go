// Package store persists pool shares, PPLNS allocations and payouts.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=10000;

CREATE TABLE IF NOT EXISTS shares (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at INTEGER NOT NULL,
  height INTEGER NOT NULL,
  parent_id TEXT NOT NULL,
  job_id TEXT NOT NULL,
  address TEXT NOT NULL,
  worker TEXT NOT NULL,
  difficulty REAL NOT NULL,
  work TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS shares_created_idx ON shares(created_at DESC);
CREATE INDEX IF NOT EXISTS shares_address_idx ON shares(address, created_at DESC);

CREATE TABLE IF NOT EXISTS blocks (
  id TEXT PRIMARY KEY,
  height INTEGER NOT NULL,
  found_at INTEGER NOT NULL,
  found_by_address TEXT NOT NULL,
  found_by_worker TEXT NOT NULL,
  reward_atomic TEXT NOT NULL,
  fees_atomic TEXT NOT NULL,
  pool_fee_atomic TEXT NOT NULL,
  window_work TEXT NOT NULL,
  selected_work TEXT NOT NULL,
  maturity_height INTEGER NOT NULL,
  confirmations INTEGER NOT NULL DEFAULT 0,
  canonical INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS blocks_height_idx ON blocks(height DESC);

CREATE TABLE IF NOT EXISTS allocations (
  block_id TEXT NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  work TEXT NOT NULL,
  amount_atomic TEXT NOT NULL,
  PRIMARY KEY(block_id, address)
);
CREATE INDEX IF NOT EXISTS allocations_address_idx ON allocations(address);

CREATE TABLE IF NOT EXISTS payouts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  submitted_at INTEGER,
  transaction_id TEXT NOT NULL DEFAULT '',
  amount_atomic TEXT NOT NULL,
  fee_atomic TEXT NOT NULL,
  status TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS payouts_created_idx ON payouts(created_at DESC);

CREATE TABLE IF NOT EXISTS payout_items (
  payout_id INTEGER NOT NULL REFERENCES payouts(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  amount_atomic TEXT NOT NULL,
  PRIMARY KEY(payout_id, address)
);
CREATE INDEX IF NOT EXISTS payout_items_address_idx ON payout_items(address);
`

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type Share struct {
	CreatedAt  time.Time
	Height     uint64
	ParentID   string
	JobID      string
	Address    string
	Worker     string
	Difficulty float64
	Work       *big.Int
	Hash       string
}

type FoundBlock struct {
	ID             string    `json:"id"`
	Height         uint64    `json:"height"`
	FoundAt        time.Time `json:"foundAt"`
	FoundByAddress string    `json:"foundByAddress"`
	FoundByWorker  string    `json:"foundByWorker"`
	RewardAtomic   string    `json:"rewardAtomic"`
	FeesAtomic     string    `json:"feesAtomic"`
	PoolFeeAtomic  string    `json:"poolFeeAtomic"`
	WindowWork     string    `json:"windowWork"`
	SelectedWork   string    `json:"selectedWork"`
	MaturityHeight uint64    `json:"maturityHeight"`
	Confirmations  uint64    `json:"confirmations"`
	Canonical      bool      `json:"canonical"`
	Status         string    `json:"status"`
	LastError      string    `json:"lastError,omitempty"`
	CutoffShareID  int64     `json:"-"`
}

type Allocation struct {
	Address      string `json:"address"`
	Work         string `json:"work"`
	AmountAtomic string `json:"amountAtomic"`
}

type Payout struct {
	ID            int64        `json:"id"`
	RequestID     string       `json:"requestID"`
	CreatedAt     time.Time    `json:"createdAt"`
	SubmittedAt   *time.Time   `json:"submittedAt,omitempty"`
	TransactionID string       `json:"transactionID,omitempty"`
	AmountAtomic  string       `json:"amountAtomic"`
	FeeAtomic     string       `json:"feeAtomic"`
	Status        string       `json:"status"`
	LastError     string       `json:"lastError,omitempty"`
	Items         []PayoutItem `json:"items,omitempty"`
	Outputs       int          `json:"outputs"`
}

type PayoutItem struct {
	Address      string `json:"address"`
	AmountAtomic string `json:"amountAtomic"`
}

type AddressAccount struct {
	Address       string `json:"address"`
	MatureAtomic  string `json:"matureAtomic"`
	PendingAtomic string `json:"pendingAtomic"`
	PaidAtomic    string `json:"paidAtomic"`
	BalanceAtomic string `json:"balanceAtomic"`
	Shares        uint64 `json:"shares"`
	Workers       int    `json:"workers"`
}

type MinerStat struct {
	Address string    `json:"address"`
	Worker  string    `json:"worker"`
	Work    string    `json:"work"`
	Shares  uint64    `json:"shares"`
	LastAt  time.Time `json:"lastAt"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=10000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func parseBig(name, value string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(value, 10)
	if !ok || n.Sign() < 0 || n.String() != value {
		return nil, fmt.Errorf("invalid %s integer", name)
	}
	return n, nil
}

func (s *Store) RecordShare(share Share) (int64, error) {
	if share.CreatedAt.IsZero() || share.Height == 0 || share.Address == "" || share.Work == nil || share.Work.Sign() <= 0 || share.Hash == "" {
		return 0, errors.New("incomplete accepted share")
	}
	result, err := s.db.Exec(`INSERT INTO shares(created_at,height,parent_id,job_id,address,worker,difficulty,work,hash) VALUES(?,?,?,?,?,?,?,?,?)`,
		share.CreatedAt.UnixMilli(), share.Height, share.ParentID, share.JobID, share.Address, share.Worker, share.Difficulty, share.Work.String(), share.Hash)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) DeleteShare(hash string) error {
	if hash == "" {
		return errors.New("share hash is empty")
	}
	_, err := s.db.Exec(`DELETE FROM shares WHERE hash=?`, hash)
	return err
}

// PruneShares keeps at least retainWork of the newest accepted work. PPLNS
// needs recent work, not an eternal archive of every low-difficulty share.
func (s *Store) PruneShares(retainWork *big.Int) error {
	if retainWork == nil || retainWork.Sign() <= 0 {
		return errors.New("share retention work must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id,work FROM shares ORDER BY id DESC`)
	if err != nil {
		return err
	}
	total := new(big.Int)
	var cutoff int64
	for rows.Next() {
		var id int64
		var encoded string
		if err := rows.Scan(&id, &encoded); err != nil {
			rows.Close()
			return err
		}
		work, err := parseBig("share work", encoded)
		if err != nil {
			rows.Close()
			return err
		}
		total.Add(total, work)
		if total.Cmp(retainWork) >= 0 {
			cutoff = id
			break
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if cutoff == 0 {
		return nil
	}
	_, err = s.db.Exec(`DELETE FROM shares WHERE id < ?`, cutoff)
	return err
}

type weightedAddress struct {
	address   string
	work      *big.Int
	amount    *big.Int
	remainder *big.Int
}

// AllocateBlock freezes one PPLNS window and assigns the exact atomic reward.
// pplnsWindow is measured in expected hashes and poolFeeBPS in basis points.
func (s *Store) AllocateBlock(block FoundBlock, pplnsWindow *big.Int, poolFeeBPS uint64) ([]Allocation, error) {
	if block.ID == "" || block.Height == 0 || block.CutoffShareID <= 0 || pplnsWindow == nil || pplnsWindow.Sign() <= 0 || poolFeeBPS > 10_000 {
		return nil, errors.New("invalid found block")
	}
	reward, err := parseBig("block reward", block.RewardAtomic)
	if err != nil || reward.Sign() <= 0 {
		return nil, errors.New("invalid positive block reward")
	}
	fees, err := parseBig("block fees", block.FeesAtomic)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM blocks WHERE id=?`, block.ID).Scan(&exists); err != nil {
		return nil, err
	} else if exists != 0 {
		return nil, errors.New("block was already allocated")
	}
	rows, err := tx.Query(`SELECT address,work FROM shares WHERE id<=? ORDER BY id DESC`, block.CutoffShareID)
	if err != nil {
		return nil, err
	}
	grouped := make(map[string]*big.Int)
	selected := new(big.Int)
	for rows.Next() && selected.Cmp(pplnsWindow) < 0 {
		var address, encoded string
		if err := rows.Scan(&address, &encoded); err != nil {
			rows.Close()
			return nil, err
		}
		work, err := parseBig("share work", encoded)
		if err != nil || work.Sign() <= 0 {
			rows.Close()
			return nil, errors.New("invalid stored share work")
		}
		remaining := new(big.Int).Sub(pplnsWindow, selected)
		if work.Cmp(remaining) > 0 {
			work = remaining
		}
		if grouped[address] == nil {
			grouped[address] = new(big.Int)
		}
		grouped[address].Add(grouped[address], work)
		selected.Add(selected, work)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	} else if selected.Sign() == 0 {
		return nil, errors.New("found block has no accepted shares")
	}
	distributable := new(big.Int).Mul(reward, new(big.Int).SetUint64(10_000-poolFeeBPS))
	distributable.Quo(distributable, big.NewInt(10_000))
	poolFee := new(big.Int).Sub(reward, distributable)
	weighted := make([]weightedAddress, 0, len(grouped))
	allocated := new(big.Int)
	for address, work := range grouped {
		numerator := new(big.Int).Mul(distributable, work)
		amount, remainder := new(big.Int), new(big.Int)
		amount.QuoRem(numerator, selected, remainder)
		allocated.Add(allocated, amount)
		weighted = append(weighted, weightedAddress{address: address, work: work, amount: amount, remainder: remainder})
	}
	left := new(big.Int).Sub(distributable, allocated)
	sort.Slice(weighted, func(i, j int) bool {
		if cmp := weighted[i].remainder.Cmp(weighted[j].remainder); cmp != 0 {
			return cmp > 0
		}
		return weighted[i].address < weighted[j].address
	})
	for i := int64(0); i < left.Int64(); i++ {
		weighted[i%int64(len(weighted))].amount.Add(weighted[i%int64(len(weighted))].amount, big.NewInt(1))
	}
	sort.Slice(weighted, func(i, j int) bool { return weighted[i].address < weighted[j].address })
	block.PoolFeeAtomic = poolFee.String()
	block.WindowWork = pplnsWindow.String()
	block.SelectedWork = selected.String()
	if block.MaturityHeight == 0 {
		block.MaturityHeight = block.Height + 60
	}
	if _, err := tx.Exec(`INSERT INTO blocks(id,height,found_at,found_by_address,found_by_worker,reward_atomic,fees_atomic,pool_fee_atomic,window_work,selected_work,maturity_height,confirmations,canonical,status,last_error) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		block.ID, block.Height, block.FoundAt.UnixMilli(), block.FoundByAddress, block.FoundByWorker, reward.String(), fees.String(), poolFee.String(), pplnsWindow.String(), selected.String(), block.MaturityHeight, 1, 1, "pending", ""); err != nil {
		return nil, err
	}
	allocations := make([]Allocation, len(weighted))
	for i, item := range weighted {
		allocations[i] = Allocation{Address: item.address, Work: item.work.String(), AmountAtomic: item.amount.String()}
		if _, err := tx.Exec(`INSERT INTO allocations(block_id,address,work,amount_atomic) VALUES(?,?,?,?)`, block.ID, item.address, item.work.String(), item.amount.String()); err != nil {
			return nil, err
		}
	}
	return allocations, tx.Commit()
}

func scanBlock(scanner interface{ Scan(...any) error }) (FoundBlock, error) {
	var block FoundBlock
	var foundAt int64
	var canonical int
	err := scanner.Scan(&block.ID, &block.Height, &foundAt, &block.FoundByAddress, &block.FoundByWorker, &block.RewardAtomic, &block.FeesAtomic, &block.PoolFeeAtomic, &block.WindowWork, &block.SelectedWork, &block.MaturityHeight, &block.Confirmations, &canonical, &block.Status, &block.LastError)
	block.FoundAt = time.UnixMilli(foundAt).UTC()
	block.Canonical = canonical != 0
	return block, err
}

const blockColumns = `id,height,found_at,found_by_address,found_by_worker,reward_atomic,fees_atomic,pool_fee_atomic,window_work,selected_work,maturity_height,confirmations,canonical,status,last_error`

func (s *Store) Blocks(status string, limit, offset int) ([]FoundBlock, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, errors.New("invalid block page")
	}
	query := `SELECT ` + blockColumns + ` FROM blocks`
	args := []any{}
	if status != "" {
		query += ` WHERE status=?`
		args = append(args, status)
	}
	query += ` ORDER BY height DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FoundBlock
	for rows.Next() {
		block, err := scanBlock(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, block)
	}
	return result, rows.Err()
}

// PendingBlocks returns the oldest unresolved blocks first. The controller
// must eventually revisit every submitted candidate even if many newer blocks
// arrive while the node or pool is offline.
func (s *Store) PendingBlocks(limit int) ([]FoundBlock, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid pending block limit")
	}
	rows, err := s.db.Query(`SELECT `+blockColumns+` FROM blocks WHERE status='pending' ORDER BY height ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FoundBlock
	for rows.Next() {
		block, err := scanBlock(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, block)
	}
	return result, rows.Err()
}

func (s *Store) SetBlockState(id string, known, canonical bool, confirmations, tipHeight uint64, failure string) (string, error) {
	status := "pending"
	var height, maturity uint64
	if err := s.db.QueryRow(`SELECT height,maturity_height,status FROM blocks WHERE id=?`, id).Scan(&height, &maturity, &status); err != nil {
		return "", err
	}
	switch {
	case canonical && tipHeight >= maturity:
		status = "mature"
	case canonical:
		status = "pending"
	case tipHeight >= height+6:
		status = "orphan"
	case !known:
		status = "pending"
	}
	_, err := s.db.Exec(`UPDATE blocks SET confirmations=?,canonical=?,status=?,last_error=? WHERE id=?`, confirmations, boolInt(canonical), status, failure, id)
	return status, err
}

// SetBlockError records an observation failure without changing chain state.
// A timeout is not evidence that a block became an orphan.
func (s *Store) SetBlockError(id, failure string) error {
	if len(failure) > 1000 {
		failure = failure[:1000]
	}
	_, err := s.db.Exec(`UPDATE blocks SET last_error=? WHERE id=? AND status='pending'`, failure, id)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func randomRequestID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return "payout-" + hex.EncodeToString(entropy[:]), nil
}

func addAmount(m map[string]*big.Int, key string, value *big.Int) {
	if m[key] == nil {
		m[key] = new(big.Int)
	}
	m[key].Add(m[key], value)
}

func (s *Store) balances(tx *sql.Tx) (mature, pending, reserved, paid map[string]*big.Int, err error) {
	mature, pending, reserved, paid = make(map[string]*big.Int), make(map[string]*big.Int), make(map[string]*big.Int), make(map[string]*big.Int)
	rows, err := tx.Query(`SELECT a.address,a.amount_atomic,b.status FROM allocations a JOIN blocks b ON b.id=a.block_id WHERE b.status IN ('pending','mature')`)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var address, encoded, status string
		if err := rows.Scan(&address, &encoded, &status); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		amount, err := parseBig("allocation", encoded)
		if err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		if status == "mature" {
			addAmount(mature, address, amount)
		} else {
			addAmount(pending, address, amount)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, nil, err
	}
	rows, err = tx.Query(`SELECT pi.address,pi.amount_atomic,p.status FROM payout_items pi JOIN payouts p ON p.id=pi.payout_id WHERE p.status IN ('prepared','submitted','confirmed')`)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var address, encoded, status string
		if err := rows.Scan(&address, &encoded, &status); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		amount, err := parseBig("payout", encoded)
		if err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		addAmount(reserved, address, amount)
		if status == "submitted" || status == "confirmed" {
			addAmount(paid, address, amount)
		}
	}
	return mature, pending, reserved, paid, rows.Close()
}

func (s *Store) PreparePayouts(threshold, fee *big.Int, maxOutputs int) (*Payout, error) {
	if threshold == nil || threshold.Sign() <= 0 || fee == nil || fee.Sign() < 0 || maxOutputs < 1 || maxOutputs > maxPoolPayoutOutputs {
		return nil, errors.New("invalid payout policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	mature, _, reserved, _, err := s.balances(tx)
	if err != nil {
		return nil, err
	}
	type availableItem struct {
		address string
		amount  *big.Int
	}
	var available []availableItem
	for address, credited := range mature {
		amount := new(big.Int).Set(credited)
		if used := reserved[address]; used != nil {
			amount.Sub(amount, used)
		}
		if amount.Cmp(threshold) >= 0 {
			available = append(available, availableItem{address, amount})
		}
	}
	if len(available) == 0 {
		return nil, tx.Commit()
	}
	sort.Slice(available, func(i, j int) bool {
		if cmp := available[i].amount.Cmp(available[j].amount); cmp != 0 {
			return cmp > 0
		}
		return available[i].address < available[j].address
	})
	if len(available) > maxOutputs {
		available = available[:maxOutputs]
	}
	requestID, err := randomRequestID()
	if err != nil {
		return nil, err
	}
	total := new(big.Int)
	for _, item := range available {
		total.Add(total, item.amount)
	}
	created := time.Now().UTC()
	result, err := tx.Exec(`INSERT INTO payouts(request_id,created_at,amount_atomic,fee_atomic,status) VALUES(?,?,?,?,?)`, requestID, created.UnixMilli(), total.String(), fee.String(), "prepared")
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	payout := &Payout{ID: id, RequestID: requestID, CreatedAt: created, AmountAtomic: total.String(), FeeAtomic: fee.String(), Status: "prepared", Items: make([]PayoutItem, len(available))}
	payout.Outputs = len(available)
	for i, item := range available {
		payout.Items[i] = PayoutItem{Address: item.address, AmountAtomic: item.amount.String()}
		if _, err := tx.Exec(`INSERT INTO payout_items(payout_id,address,amount_atomic) VALUES(?,?,?)`, id, item.address, item.amount.String()); err != nil {
			return nil, err
		}
	}
	return payout, tx.Commit()
}

func (s *Store) ActivePayouts(limit int) ([]Payout, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("invalid payout limit")
	}
	rows, err := s.db.Query(`SELECT id,request_id,created_at,amount_atomic,fee_atomic,status,last_error FROM payouts WHERE status IN ('prepared','submitted') ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var payouts []Payout
	for rows.Next() {
		var payout Payout
		var created int64
		if err := rows.Scan(&payout.ID, &payout.RequestID, &created, &payout.AmountAtomic, &payout.FeeAtomic, &payout.Status, &payout.LastError); err != nil {
			return nil, err
		}
		payout.CreatedAt = time.UnixMilli(created).UTC()
		payouts = append(payouts, payout)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	} else if err := rows.Err(); err != nil {
		return nil, err
	}
	// The store deliberately uses one SQLite connection. Load child rows only
	// after closing the payout cursor so this method cannot wait on itself.
	for i := range payouts {
		items, err := s.payoutItems(payouts[i].ID)
		if err != nil {
			return nil, err
		}
		payouts[i].Items = items
		payouts[i].Outputs = len(items)
	}
	return payouts, nil
}

func (s *Store) payoutItems(id int64) ([]PayoutItem, error) {
	rows, err := s.db.Query(`SELECT address,amount_atomic FROM payout_items WHERE payout_id=? ORDER BY address`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []PayoutItem
	for rows.Next() {
		var item PayoutItem
		if err := rows.Scan(&item.Address, &item.AmountAtomic); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SetPayoutState(id int64, transactionID, status string) error {
	if transactionID == "" || (status != "submitted" && status != "confirmed") {
		return errors.New("invalid payout state")
	}
	result, err := s.db.Exec(`UPDATE payouts SET status=?,submitted_at=COALESCE(submitted_at,?),transaction_id=?,last_error='' WHERE id=? AND status IN ('prepared','submitted')`, status, time.Now().UTC().UnixMilli(), transactionID, id)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return errors.New("active payout not found")
	}
	return nil
}

func (s *Store) PayoutError(id int64, message string) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := s.db.Exec(`UPDATE payouts SET last_error=? WHERE id=? AND status IN ('prepared','submitted')`, message, id)
	return err
}

func (s *Store) Payouts(limit, offset int) ([]Payout, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, errors.New("invalid payout page")
	}
	rows, err := s.db.Query(`SELECT p.id,p.request_id,p.created_at,p.submitted_at,p.transaction_id,p.amount_atomic,p.fee_atomic,p.status,p.last_error,(SELECT COUNT(*) FROM payout_items pi WHERE pi.payout_id=p.id) FROM payouts p ORDER BY p.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Payout
	for rows.Next() {
		var payout Payout
		var created int64
		var submitted sql.NullInt64
		if err := rows.Scan(&payout.ID, &payout.RequestID, &created, &submitted, &payout.TransactionID, &payout.AmountAtomic, &payout.FeeAtomic, &payout.Status, &payout.LastError, &payout.Outputs); err != nil {
			return nil, err
		}
		payout.CreatedAt = time.UnixMilli(created).UTC()
		if submitted.Valid {
			value := time.UnixMilli(submitted.Int64).UTC()
			payout.SubmittedAt = &value
		}
		result = append(result, payout)
	}
	return result, rows.Err()
}

func (s *Store) Account(address string) (AddressAccount, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return AddressAccount{}, err
	}
	defer tx.Rollback()
	mature, pending, reserved, paid, err := s.balances(tx)
	if err != nil {
		return AddressAccount{}, err
	}
	value := func(values map[string]*big.Int) *big.Int {
		if values[address] == nil {
			return new(big.Int)
		}
		return new(big.Int).Set(values[address])
	}
	balance := value(mature)
	balance.Sub(balance, value(reserved))
	account := AddressAccount{Address: address, MatureAtomic: value(mature).String(), PendingAtomic: value(pending).String(), PaidAtomic: value(paid).String(), BalanceAtomic: balance.String()}
	if err := tx.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT worker) FROM shares WHERE address=?`, address).Scan(&account.Shares, &account.Workers); err != nil {
		return AddressAccount{}, err
	}
	return account, tx.Commit()
}

func (s *Store) TopMiners(since time.Time, limit int) ([]MinerStat, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("invalid miner limit")
	}
	rows, err := s.db.Query(`SELECT address,worker,work,created_at FROM shares WHERE created_at>=? ORDER BY id`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ address, worker string }
	stats := make(map[key]*MinerStat)
	work := make(map[key]*big.Int)
	for rows.Next() {
		var address, worker, encoded string
		var created int64
		if err := rows.Scan(&address, &worker, &encoded, &created); err != nil {
			return nil, err
		}
		value, err := parseBig("share work", encoded)
		if err != nil {
			return nil, err
		}
		k := key{address, worker}
		if stats[k] == nil {
			stats[k] = &MinerStat{Address: address, Worker: worker}
			work[k] = new(big.Int)
		}
		stats[k].Shares++
		stats[k].LastAt = time.UnixMilli(created).UTC()
		work[k].Add(work[k], value)
	}
	result := make([]MinerStat, 0, len(stats))
	for k, stat := range stats {
		stat.Work = work[k].String()
		result = append(result, *stat)
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := new(big.Int).SetString(result[i].Work, 10)
		b, _ := new(big.Int).SetString(result[j].Work, 10)
		if cmp := a.Cmp(b); cmp != 0 {
			return cmp > 0
		}
		return result[i].Worker < result[j].Worker
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *Store) ShareSummary(since time.Time) (work *big.Int, shares uint64, workers, addresses int, err error) {
	rows, err := s.db.Query(`SELECT address,worker,work FROM shares WHERE created_at>=?`, since.UnixMilli())
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer rows.Close()
	work = new(big.Int)
	workerSet, addressSet := make(map[string]bool), make(map[string]bool)
	for rows.Next() {
		var address, worker, encoded string
		if err := rows.Scan(&address, &worker, &encoded); err != nil {
			return nil, 0, 0, 0, err
		}
		value, err := parseBig("share work", encoded)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		work.Add(work, value)
		shares++
		workerSet[address+"\x00"+worker] = true
		addressSet[address] = true
	}
	return work, shares, len(workerSet), len(addressSet), rows.Err()
}

func (s *Store) RoundWork() (*big.Int, error) {
	var since sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(found_at) FROM blocks`).Scan(&since); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT work FROM shares WHERE created_at>?`, since.Int64)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	total := new(big.Int)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		value, err := parseBig("share work", encoded)
		if err != nil {
			return nil, err
		}
		total.Add(total, value)
	}
	return total, rows.Err()
}

func (s *Store) BlockCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&count)
	return count, err
}

func (s *Store) PayoutCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM payouts`).Scan(&count)
	return count, err
}

const maxPoolPayoutOutputs = 100
