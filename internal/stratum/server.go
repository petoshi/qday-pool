// Package stratum serves registration-free SiaMining Stratum jobs backed by
// transaction-aware QDAY templates.
package stratum

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/store"
	"go.sia.tech/core/types"
)

const (
	maxRequestBytes   = 64 << 10
	maxRememberedJobs = 8192
	maxSubmissions    = 8192
	maxRequestsSecond = 64
	templateWorkers   = 16
)

type nodeClient interface {
	GetBlockTemplate(context.Context, string, *uint64) (nodeapi.Template, error)
	SubmitBlock(context.Context, string) (string, error)
}

type shareStore interface {
	RecordShare(store.Share) (int64, error)
	DeleteShare(string) error
	AllocateBlock(store.FoundBlock, *big.Int, uint64) ([]store.Allocation, error)
	PruneShares(*big.Int) error
}

type Config struct {
	ListenAddress     string
	JobInterval       time.Duration
	ShareTarget       time.Duration
	InitialDifficulty float64
	MinimumDifficulty float64
	MaximumDifficulty float64
	PPLNSWindow       uint64
	PoolFeeBPS        uint64
	MaturityBlocks    uint64
	MaxMiners         int
	MaxMinersPerIP    int
	Logger            *slog.Logger
}

type Snapshot struct {
	Ready             bool      `json:"ready"`
	Height            uint64    `json:"height"`
	Parent            string    `json:"parent"`
	Transactions      int       `json:"transactions"`
	NetworkDifficulty float64   `json:"networkDifficulty"`
	NetworkHashrate   float64   `json:"networkHashrate"`
	NetworkWork       string    `json:"networkWork"`
	Connected         int       `json:"connected"`
	Authorized        int       `json:"authorized"`
	AcceptedShares    uint64    `json:"acceptedShares"`
	RejectedShares    uint64    `json:"rejectedShares"`
	BlocksFound       uint64    `json:"blocksFound"`
	UpdatedAt         time.Time `json:"updatedAt"`
	LastError         string    `json:"lastError,omitempty"`
}

type Server struct {
	node  nodeClient
	store shareStore
	cfg   Config

	mu          sync.Mutex
	clients     map[*client]struct{}
	ipClients   map[string]int
	jobs        map[string]*job
	jobOrder    []string
	parent      [32]byte
	height      uint64
	longPollID  string
	ready       bool
	lastError   string
	txCount     int
	networkDiff float64
	networkHash float64
	networkWork string
	updatedAt   time.Time

	sequence       atomic.Uint64
	workSequence   atomic.Uint64
	acceptedShares atomic.Uint64
	rejectedShares atomic.Uint64
	blocksFound    atomic.Uint64
}

func NewServer(node nodeClient, shares shareStore, cfg Config) (*Server, error) {
	if node == nil || shares == nil {
		return nil, errors.New("QDAY node and share store are required")
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:3333"
	}
	if cfg.JobInterval == 0 {
		cfg.JobInterval = time.Second
	}
	if cfg.JobInterval < 250*time.Millisecond {
		return nil, errors.New("job interval cannot be below 250ms")
	}
	if cfg.ShareTarget == 0 {
		cfg.ShareTarget = 15 * time.Second
	}
	if cfg.ShareTarget < time.Second || cfg.ShareTarget > 5*time.Minute {
		return nil, errors.New("share target must be between 1 second and 5 minutes")
	}
	if cfg.InitialDifficulty == 0 {
		cfg.InitialDifficulty = 1
	}
	if cfg.MinimumDifficulty == 0 {
		cfg.MinimumDifficulty = .01
	}
	if cfg.MaximumDifficulty == 0 {
		cfg.MaximumDifficulty = 1e12
	}
	if cfg.MinimumDifficulty <= 0 || cfg.MaximumDifficulty < cfg.MinimumDifficulty || cfg.InitialDifficulty < cfg.MinimumDifficulty || cfg.InitialDifficulty > cfg.MaximumDifficulty ||
		math.IsNaN(cfg.MinimumDifficulty) || math.IsNaN(cfg.InitialDifficulty) || math.IsNaN(cfg.MaximumDifficulty) ||
		math.IsInf(cfg.MinimumDifficulty, 0) || math.IsInf(cfg.InitialDifficulty, 0) || math.IsInf(cfg.MaximumDifficulty, 0) {
		return nil, errors.New("invalid share difficulty bounds")
	}
	if cfg.PPLNSWindow == 0 {
		cfg.PPLNSWindow = 2
	}
	if cfg.PPLNSWindow > 100 || cfg.PoolFeeBPS > 10_000 {
		return nil, errors.New("invalid PPLNS or pool fee setting")
	}
	if cfg.MaturityBlocks == 0 {
		cfg.MaturityBlocks = 60
	}
	if cfg.MaxMiners == 0 {
		cfg.MaxMiners = 1024
	}
	if cfg.MaxMinersPerIP == 0 {
		cfg.MaxMinersPerIP = 32
	}
	if cfg.MaxMiners < 1 || cfg.MaxMiners > 100_000 || cfg.MaxMinersPerIP < 1 || cfg.MaxMinersPerIP > cfg.MaxMiners {
		return nil, errors.New("invalid miner connection limits")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{node: node, store: shares, cfg: cfg, clients: make(map[*client]struct{}), ipClients: make(map[string]int), jobs: make(map[string]*job)}, nil
}

func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	s.cfg.Logger.Info("QDAY pool Stratum ready", "listen", listener.Addr().String())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	go s.templateLoop(ctx)
	go s.refreshLoop(ctx)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		s.mu.Lock()
		if len(s.clients) >= s.cfg.MaxMiners || s.ipClients[host] >= s.cfg.MaxMinersPerIP {
			s.mu.Unlock()
			conn.Close()
			s.cfg.Logger.Warn("miner connection limit reached", "remote", conn.RemoteAddr().String())
			continue
		}
		c := newClient(s, conn, host)
		s.clients[c] = struct{}{}
		s.ipClients[host]++
		s.mu.Unlock()
		go c.run(ctx)
	}
}

func (s *Server) setTemplateError(err error) {
	s.mu.Lock()
	s.ready = false
	s.lastError = err.Error()
	clients := make([]*client, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	for _, client := range clients {
		client.clearTemplate()
	}
}

func (s *Server) templateLoop(ctx context.Context) {
	longPollID := ""
	for ctx.Err() == nil {
		template, err := s.node.GetBlockTemplate(ctx, longPollID, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setTemplateError(err)
			s.cfg.Logger.Error("QDAY template unavailable", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			longPollID = ""
			continue
		}
		parent, err := decode32("previous block hash", template.PreviousBlockHash)
		if err != nil || template.LongPollID == "" {
			if err == nil {
				err = errors.New("QDAY template has no long-poll ID")
			}
			s.setTemplateError(err)
			longPollID = ""
			continue
		}
		longPollID = template.LongPollID
		target, targetErr := decode32("target", template.Target)
		if targetErr != nil {
			s.setTemplateError(targetErr)
			longPollID = ""
			continue
		}
		s.mu.Lock()
		previousParent := s.parent
		s.parent, s.height, s.longPollID = parent, template.Height, template.LongPollID
		s.ready, s.lastError, s.txCount = true, "", len(template.Transactions)
		s.networkDiff, _ = targetDifficulty(target)
		networkWork := TargetWork(target)
		hashrate, _ := new(big.Float).Quo(new(big.Float).SetInt(networkWork), big.NewFloat(60)).Float64()
		s.networkHash, s.networkWork, s.updatedAt = hashrate, networkWork.String(), time.Now().UTC()
		clients := make([]*client, 0, len(s.clients))
		for client := range s.clients {
			if client.authorized.Load() {
				clients = append(clients, client)
			}
		}
		s.mu.Unlock()
		s.assignTemplates(ctx, clients)
		if previousParent != parent {
			retention := new(big.Int).Mul(networkWork, new(big.Int).SetUint64(s.cfg.PPLNSWindow*100))
			if err := s.store.PruneShares(retention); err != nil {
				s.cfg.Logger.Warn("could not prune old pool shares", "error", err)
			}
		}
	}
}

func (s *Server) assignTemplates(ctx context.Context, clients []*client) {
	sem := make(chan struct{}, templateWorkers)
	var wg sync.WaitGroup
	for _, client := range clients {
		client := client
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.assignTemplate(requestCtx, client, true)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.cfg.Logger.Warn("worker template unavailable", "worker", client.workerName(), "error", err)
			}
		}()
	}
	wg.Wait()
}

func (s *Server) nextWorkNonce() uint64 {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err == nil {
		if value := binary.LittleEndian.Uint64(entropy[:]); value != 0 {
			return value
		}
	}
	return s.workSequence.Add(1)
}

func (s *Server) assignTemplate(ctx context.Context, c *client, clean bool) error {
	nonce := s.nextWorkNonce()
	template, err := s.node.GetBlockTemplate(ctx, "", &nonce)
	if err != nil {
		return err
	}
	parsed, err := newWorkTemplate(template)
	if err != nil {
		return err
	}
	s.mu.Lock()
	currentParent := s.parent
	s.mu.Unlock()
	if parsed.parent != currentParent {
		return errors.New("QDAY tip changed while assigning work")
	}
	c.setTemplate(parsed)
	return s.publishClient(c, time.Now(), clean)
}

func (s *Server) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.JobInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			clients := make([]*client, 0, len(s.clients))
			for client := range s.clients {
				if client.authorized.Load() {
					clients = append(clients, client)
				}
			}
			s.mu.Unlock()
			for _, client := range clients {
				client.lowerIdleDifficulty(now)
				_ = s.publishClient(client, now, true)
			}
		}
	}
}

func (s *Server) publishClient(c *client, now time.Time, clean bool) error {
	template, difficulty := c.templateAndDifficulty()
	if template == nil {
		return nil
	}
	sequence := s.sequence.Add(1)
	id := fmt.Sprintf("%x", sequence)
	next, err := template.newJob(id, sequence, c, now, difficulty)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.jobs[next.id] = next
	s.jobOrder = append(s.jobOrder, next.id)
	for len(s.jobOrder) > maxRememberedJobs {
		delete(s.jobs, s.jobOrder[0])
		s.jobOrder = s.jobOrder[1:]
	}
	s.mu.Unlock()
	return c.sendJob(next, clean)
}

func (s *Server) currentParent() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parent
}

func (s *Server) lookupJob(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Server) remove(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	if s.ipClients[c.ip] <= 1 {
		delete(s.ipClients, c.ip)
	} else {
		s.ipClients[c.ip]--
	}
	s.mu.Unlock()
}

func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	authorized := 0
	for client := range s.clients {
		if client.authorized.Load() {
			authorized++
		}
	}
	return Snapshot{
		Ready: s.ready, Height: s.height, Parent: hex.EncodeToString(s.parent[:]), Transactions: s.txCount,
		NetworkDifficulty: s.networkDiff, NetworkHashrate: s.networkHash, NetworkWork: s.networkWork, Connected: len(s.clients), Authorized: authorized,
		AcceptedShares: s.acceptedShares.Load(), RejectedShares: s.rejectedShares.Load(), BlocksFound: s.blocksFound.Load(), UpdatedAt: s.updatedAt, LastError: s.lastError,
	}
}

type rpcRequest struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcResponse struct {
	ID     any `json:"id"`
	Result any `json:"result"`
	Error  any `json:"error"`
}

func rpcFailure(code int, message string) []any { return []any{code, message, nil} }

type rpcNotification struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type client struct {
	server *Server
	conn   net.Conn
	ip     string
	write  sync.Mutex
	state  sync.Mutex
	once   sync.Once

	subscribed atomic.Bool
	authorized atomic.Bool
	address    string
	worker     string
	template   *workTemplate
	difficulty float64
	lastDiff   float64
	lastJob    uint64
	shareTimes []time.Time
	lastShare  time.Time

	submitMu sync.Mutex
	submits  map[string]struct{}
	rateAt   time.Time
	rateN    int
}

func newClient(server *Server, conn net.Conn, ip string) *client {
	return &client{server: server, conn: conn, ip: ip, difficulty: server.cfg.InitialDifficulty, submits: make(map[string]struct{}), rateAt: time.Now()}
}

func (c *client) close() {
	c.once.Do(func() {
		c.conn.Close()
		c.server.remove(c)
	})
}

func (c *client) run(ctx context.Context) {
	defer c.close()
	c.server.cfg.Logger.Info("miner connected", "remote", c.conn.RemoteAddr().String())
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes)
	for scanner.Scan() {
		now := time.Now()
		if now.Sub(c.rateAt) >= time.Second {
			c.rateAt, c.rateN = now, 0
		}
		c.rateN++
		if c.rateN > maxRequestsSecond {
			c.server.cfg.Logger.Warn("miner request rate exceeded", "remote", c.conn.RemoteAddr().String())
			return
		}
		var request rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || request.Method == "" {
			c.respond(nil, nil, rpcFailure(-32700, "invalid JSON-RPC request"))
			continue
		}
		c.handle(ctx, request)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		c.server.cfg.Logger.Info("miner disconnected", "worker", c.workerName(), "error", err)
	}
}

func decodeParams(raw json.RawMessage, result any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("[]")
	}
	return json.Unmarshal(raw, result)
}

func parseWorkerLogin(value string) (address, worker string, err error) {
	value = strings.TrimSpace(value)
	if len(value) < 64 || len(value) > 97 {
		return "", "", errors.New("use qday-address.worker as the username")
	}
	address = value[:64]
	parsed, err := types.ParseQdayAddress(address)
	if err != nil || parsed.String() != address {
		return "", "", errors.New("worker username must begin with a canonical QDAY address")
	}
	worker = "default"
	if len(value) > 64 {
		if value[64] != '.' || len(value) == 65 {
			return "", "", errors.New("worker username must be qday-address.worker")
		}
		worker = value[65:]
		for _, r := range worker {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return "", "", errors.New("worker label may contain letters, digits, dash and underscore")
			}
		}
	}
	return address, worker, nil
}

func (c *client) handle(ctx context.Context, request rpcRequest) {
	switch request.Method {
	case "mining.subscribe":
		c.subscribed.Store(true)
		subscription := strconv.FormatInt(time.Now().UnixNano(), 16)
		c.respond(request.ID, []any{[]any{[]any{"mining.set_difficulty", subscription}, []any{"mining.notify", subscription}}, "", 0}, nil)
	case "mining.authorize":
		if !c.subscribed.Load() {
			c.respond(request.ID, false, rpcFailure(25, "subscribe before authorization"))
			return
		}
		if c.authorized.Load() {
			c.respond(request.ID, false, rpcFailure(25, "worker is already authorized"))
			return
		}
		var params []string
		if err := decodeParams(request.Params, &params); err != nil || len(params) < 1 {
			c.respond(request.ID, false, rpcFailure(20, "use qday-address.worker as the username"))
			return
		}
		address, worker, err := parseWorkerLogin(params[0])
		if err != nil {
			c.respond(request.ID, false, rpcFailure(20, err.Error()))
			return
		}
		difficulty := c.server.cfg.InitialDifficulty
		if len(params) > 1 {
			if suggested, ok := passwordDifficulty(params[1]); ok {
				if suggested < c.server.cfg.MinimumDifficulty || suggested > c.server.cfg.MaximumDifficulty {
					c.respond(request.ID, false, rpcFailure(20, "password difficulty is outside pool limits"))
					return
				}
				difficulty = suggested
			}
		}
		c.state.Lock()
		c.address, c.worker = address, worker
		c.difficulty, c.template = difficulty, nil
		c.shareTimes, c.lastShare = nil, time.Time{}
		c.state.Unlock()
		c.authorized.Store(true)
		_ = c.conn.SetReadDeadline(time.Time{})
		c.respond(request.ID, true, nil)
		c.server.cfg.Logger.Info("miner authorized", "address", address, "worker", worker, "remote", c.conn.RemoteAddr().String())
		assignCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = c.server.assignTemplate(assignCtx, c, true)
		cancel()
		if err != nil {
			c.server.cfg.Logger.Warn("initial worker template unavailable", "worker", c.workerName(), "error", err)
		}
	case "mining.configure":
		c.respond(request.ID, map[string]any{}, nil)
	case "mining.extranonce.subscribe":
		c.respond(request.ID, true, nil)
	case "mining.suggest_difficulty":
		var params []float64
		if err := decodeParams(request.Params, &params); err != nil || len(params) != 1 || params[0] < c.server.cfg.MinimumDifficulty || params[0] > c.server.cfg.MaximumDifficulty || math.IsNaN(params[0]) || math.IsInf(params[0], 0) {
			c.respond(request.ID, false, rpcFailure(20, "suggested difficulty is outside pool limits"))
			return
		}
		c.state.Lock()
		c.difficulty, c.shareTimes = params[0], nil
		c.state.Unlock()
		c.respond(request.ID, true, nil)
		if c.authorized.Load() {
			_ = c.server.publishClient(c, time.Now(), true)
		}
	case "mining.submit":
		c.submit(ctx, request)
	default:
		c.respond(request.ID, nil, rpcFailure(-32601, "method not found"))
	}
}

func passwordDifficulty(password string) (float64, bool) {
	for _, field := range strings.FieldsFunc(password, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || (key != "d" && key != "diff" && key != "difficulty") {
			continue
		}
		difficulty, err := strconv.ParseFloat(value, 64)
		return difficulty, err == nil && difficulty > 0 && !math.IsNaN(difficulty) && !math.IsInf(difficulty, 0)
	}
	return 0, false
}

func (c *client) workerName() string {
	c.state.Lock()
	defer c.state.Unlock()
	if c.worker == "" {
		return "unauthorized"
	}
	return c.worker
}

func (c *client) identity() (string, string) {
	c.state.Lock()
	defer c.state.Unlock()
	return c.address, c.worker
}

func (c *client) setTemplate(template *workTemplate) {
	c.state.Lock()
	c.template = template
	c.state.Unlock()
}

func (c *client) clearTemplate() {
	c.state.Lock()
	c.template = nil
	c.state.Unlock()
}

func (c *client) templateAndDifficulty() (*workTemplate, float64) {
	c.state.Lock()
	defer c.state.Unlock()
	return c.template, c.difficulty
}

func (c *client) send(value any) error {
	c.write.Lock()
	defer c.write.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return json.NewEncoder(c.conn).Encode(value)
}

func (c *client) respond(id, result, failure any) {
	if err := c.send(rpcResponse{ID: id, Result: result, Error: failure}); err != nil {
		c.close()
	}
}

func (c *client) notify(method string, params any) error {
	return c.send(rpcNotification{ID: nil, Method: method, Params: params})
}

func (c *client) sendJob(j *job, clean bool) error {
	c.state.Lock()
	if j.sequence <= c.lastJob {
		c.state.Unlock()
		return nil
	}
	changeDiff := c.lastDiff != j.difficulty
	if changeDiff {
		c.lastDiff = j.difficulty
	}
	c.lastJob = j.sequence
	c.state.Unlock()
	if changeDiff {
		if err := c.notify("mining.set_difficulty", []any{j.difficulty}); err != nil {
			return err
		}
	}
	return c.notify("mining.notify", j.notifyParams(clean))
}

func (c *client) acceptedAt(now time.Time) bool {
	c.state.Lock()
	defer c.state.Unlock()
	c.lastShare = now
	c.shareTimes = append(c.shareTimes, now)
	if len(c.shareTimes) < 2 {
		return false
	}
	span := c.shareTimes[len(c.shareTimes)-1].Sub(c.shareTimes[0])
	if span <= 0 {
		span = time.Millisecond
	}
	observed := span.Seconds() / float64(len(c.shareTimes)-1)
	factor := c.server.cfg.ShareTarget.Seconds() / observed
	factor = math.Max(.25, math.Min(64, factor))
	next := math.Max(c.server.cfg.MinimumDifficulty, math.Min(c.server.cfg.MaximumDifficulty, c.difficulty*factor))
	c.shareTimes = c.shareTimes[len(c.shareTimes)-1:]
	if math.Abs(next/c.difficulty-1) < .15 {
		return false
	}
	c.difficulty = next
	return true
}

func (c *client) lowerIdleDifficulty(now time.Time) {
	c.state.Lock()
	defer c.state.Unlock()
	if c.lastShare.IsZero() || now.Sub(c.lastShare) < 4*c.server.cfg.ShareTarget || c.difficulty <= c.server.cfg.MinimumDifficulty {
		return
	}
	c.difficulty = math.Max(c.server.cfg.MinimumDifficulty, c.difficulty/2)
	c.lastShare = now
}

func (c *client) submit(ctx context.Context, request rpcRequest) {
	if !c.authorized.Load() {
		c.respond(request.ID, false, rpcFailure(24, "unauthorized worker"))
		return
	}
	var params []string
	if err := decodeParams(request.Params, &params); err != nil || len(params) != 5 {
		c.respond(request.ID, false, rpcFailure(20, "submit requires worker, job, extranonce2, time and nonce"))
		return
	}
	j := c.server.lookupJob(params[1])
	if j == nil || j.owner != c || j.parent != c.server.currentParent() {
		c.server.rejectedShares.Add(1)
		c.respond(request.ID, false, rpcFailure(21, "stale or unknown job"))
		return
	}
	key := strings.Join(params[1:], ":")
	c.submitMu.Lock()
	_, duplicate := c.submits[key]
	if !duplicate {
		if len(c.submits) >= maxSubmissions {
			c.submits = make(map[string]struct{})
		}
		c.submits[key] = struct{}{}
	}
	c.submitMu.Unlock()
	if duplicate {
		c.server.rejectedShares.Add(1)
		c.respond(request.ID, false, rpcFailure(22, "duplicate share"))
		return
	}
	block, hash, network, err := j.solve("", params[2], params[3], params[4])
	if err != nil {
		c.server.rejectedShares.Add(1)
		c.respond(request.ID, false, rpcFailure(23, err.Error()))
		return
	}
	address, worker := j.address, j.worker
	now := time.Now().UTC()
	shareID, err := c.server.store.RecordShare(store.Share{CreatedAt: now, Height: j.height, ParentID: hex.EncodeToString(j.parent[:]), JobID: j.id, Address: address, Worker: worker, Difficulty: j.difficulty, Work: TargetWork(j.shareTarget), Hash: hex.EncodeToString(hash[:])})
	if err != nil {
		c.server.rejectedShares.Add(1)
		c.server.cfg.Logger.Error("accepted share could not be stored", "worker", worker, "error", err)
		c.respond(request.ID, false, rpcFailure(20, "pool accounting unavailable"))
		return
	}
	c.server.acceptedShares.Add(1)
	retarget := c.acceptedAt(now)
	if !network {
		c.respond(request.ID, true, nil)
		if retarget {
			_ = c.server.publishClient(c, now, true)
		}
		return
	}
	predictedID := hex.EncodeToString(hash[:])
	window := new(big.Int).Mul(TargetWork(j.target), new(big.Int).SetUint64(c.server.cfg.PPLNSWindow))
	_, err = c.server.store.AllocateBlock(store.FoundBlock{
		ID: predictedID, Height: j.height, FoundAt: now, FoundByAddress: address, FoundByWorker: worker,
		RewardAtomic: j.rewardAtomic, FeesAtomic: j.feesAtomic, MaturityHeight: j.height + c.server.cfg.MaturityBlocks, CutoffShareID: shareID,
	}, window, c.server.cfg.PoolFeeBPS)
	if err != nil {
		c.server.cfg.Logger.Error("solved block was not submitted because accounting failed", "block", predictedID, "height", j.height, "error", err)
		c.respond(request.ID, false, rpcFailure(20, "pool accounting unavailable; block was not submitted"))
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	blockID, err := c.server.node.SubmitBlock(requestCtx, hex.EncodeToString(block))
	cancel()
	if err != nil {
		// The candidate and frozen PPLNS allocation stay pending. A local HTTP
		// failure can happen after the node accepted the block; blockstatus will
		// later distinguish that case from a rejected or stale candidate.
		c.server.cfg.Logger.Warn("solved block submission is unresolved", "worker", worker, "height", j.height, "block", predictedID, "error", err)
		c.respond(request.ID, false, rpcFailure(20, "QDAY block submission is unresolved: "+err.Error()))
		return
	}
	if blockID != predictedID {
		c.server.cfg.Logger.Error("QDAY returned an unexpected block ID", "expected", predictedID, "actual", blockID)
		c.respond(request.ID, false, rpcFailure(20, "QDAY returned an unexpected block ID"))
		return
	}
	c.server.blocksFound.Add(1)
	c.server.cfg.Logger.Info("BLOCK ACCEPTED", "height", j.height, "worker", worker, "block", blockID, "transactions", j.transactions, "feesAtomic", j.feesAtomic)
	c.respond(request.ID, true, nil)
}
