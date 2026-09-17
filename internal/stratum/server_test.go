package stratum

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/store"
	"go.sia.tech/core/types"
)

func syntheticTemplate(t *testing.T, target [32]byte, workNonce uint64) nodeapi.Template {
	t.Helper()
	parent := [32]byte{0: 0x51, 31: 0x59}
	txn := types.V2Transaction{ArbitraryData: make([]byte, 16)}
	binary.LittleEndian.PutUint64(txn.ArbitraryData, workNonce)
	copy(txn.ArbitraryData[8:], "QDAYPOOL")
	var transaction bytes.Buffer
	encoder := types.NewEncoder(&transaction)
	txn.EncodeTo(encoder)
	if err := encoder.Flush(); err != nil {
		t.Fatal(err)
	}
	left := [32]byte{0: 0xaa, 31: 0xbb}
	commitment := merklePair(left, transactionLeaf(transaction.Bytes()))
	timestamp := time.Now().UTC().Unix()
	block := types.Block{
		ParentID:  types.BlockID(parent),
		Timestamp: time.Unix(timestamp, 0),
		V2: &types.V2BlockData{
			Height:       42,
			Commitment:   types.Hash256(commitment),
			Transactions: []types.V2Transaction{txn},
		},
	}
	var header bytes.Buffer
	headerEncoder := types.NewEncoder(&header)
	block.Header().EncodeTo(headerEncoder)
	if err := headerEncoder.Flush(); err != nil {
		t.Fatal(err)
	}
	var encodedBlock bytes.Buffer
	blockEncoder := types.NewEncoder(&encodedBlock)
	types.V2Block(block).EncodeTo(blockEncoder)
	if err := blockEncoder.Flush(); err != nil {
		t.Fatal(err)
	}
	return nodeapi.Template{
		Header: hex.EncodeToString(header.Bytes()), Commitment: hex.EncodeToString(commitment[:]),
		Transactions:      []nodeapi.TemplateTransaction{{Data: hex.EncodeToString(transaction.Bytes()), TxID: "marker"}},
		PreviousBlockHash: hex.EncodeToString(parent[:]), LongPollID: "template-1", Target: hex.EncodeToString(target[:]),
		Height: 42, Timestamp: timestamp, Bits: "207fffff", WorkNonce: workNonce,
		BlockRewardAtomic: "8000", FeesAtomic: "240000", PayoutAtomic: "248000",
		Stratum: nodeapi.StratumTemplate{Block: hex.EncodeToString(encodedBlock.Bytes()), MerkleBranch: []string{hex.EncodeToString(left[:])}},
	}
}

type fakeNode struct {
	t          *testing.T
	target     [32]byte
	ready      chan struct{}
	once       sync.Once
	submits    chan string
	mu         sync.Mutex
	workNonces []uint64
	allocated  *atomic.Bool
}

func (n *fakeNode) GetBlockTemplate(ctx context.Context, longPollID string, nonce *uint64) (nodeapi.Template, error) {
	if nonce != nil {
		n.mu.Lock()
		n.workNonces = append(n.workNonces, *nonce)
		n.mu.Unlock()
		return syntheticTemplate(n.t, n.target, *nonce), nil
	}
	if longPollID == "" {
		n.once.Do(func() { close(n.ready) })
		return syntheticTemplate(n.t, n.target, 1), nil
	}
	<-ctx.Done()
	return nodeapi.Template{}, ctx.Err()
}

func (n *fakeNode) SubmitBlock(_ context.Context, block string) (string, error) {
	if n.allocated != nil && !n.allocated.Load() {
		n.t.Error("block reached QDAY before its PPLNS allocation was durable")
	}
	n.submits <- block
	raw, err := hex.DecodeString(block)
	if err != nil {
		n.t.Fatalf("invalid submitted block: %v", err)
	}
	decoder := types.NewBufDecoder(raw)
	var encodedBlock types.V2Block
	encodedBlock.DecodeFrom(decoder)
	if err := decoder.Err(); err != nil {
		n.t.Fatalf("invalid submitted block: %v", err)
	}
	decodedBlock := encodedBlock.Cast()
	return decodedBlock.ID().String(), nil
}

type fakeStore struct {
	shares    chan store.Share
	blocks    chan store.FoundBlock
	allocated *atomic.Bool
}

func (s *fakeStore) RecordShare(share store.Share) (int64, error) {
	s.shares <- share
	return 1, nil
}
func (s *fakeStore) DeleteShare(string) error   { return nil }
func (s *fakeStore) PruneShares(*big.Int) error { return nil }
func (s *fakeStore) AllocateBlock(block store.FoundBlock, _ *big.Int, _ uint64) ([]store.Allocation, error) {
	if s.allocated != nil {
		s.allocated.Store(true)
	}
	s.blocks <- block
	return nil, nil
}

type wireMessage struct {
	ID     any               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	Result json.RawMessage   `json:"result"`
	Error  json.RawMessage   `json:"error"`
}

func writeRPC(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func readWire(t *testing.T, reader *bufio.Reader) wireMessage {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var message wireMessage
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}
	return message
}

func TestNotificationWriteFailureEvictsConnection(t *testing.T) {
	server := &Server{
		clients:   make(map[*client]struct{}),
		ipClients: make(map[string]int),
	}
	poolSide, minerSide := net.Pipe()
	minerSide.Close()
	client := newClient(server, poolSide, "127.0.0.1")
	server.clients[client] = struct{}{}
	server.ipClients[client.ip] = 1

	if err := client.notify("mining.notify", []any{"job"}); err == nil {
		t.Fatal("notification to a closed miner succeeded")
	}
	if snapshot := server.Snapshot(); snapshot.Connected != 0 || snapshot.Authorized != 0 {
		t.Fatalf("failed miner remained connected: %+v", snapshot)
	}
}

func TestPublicSiaStratumRoundTrip(t *testing.T) {
	var target [32]byte
	for i := range target {
		target[i] = 0xff
	}
	var allocated atomic.Bool
	node := &fakeNode{t: t, target: target, ready: make(chan struct{}), submits: make(chan string, 1), allocated: &allocated}
	database := &fakeStore{shares: make(chan store.Share, 1), blocks: make(chan store.FoundBlock, 1), allocated: &allocated}
	server, err := NewServer(node, database, Config{
		JobInterval: time.Hour, InitialDifficulty: 1e-12, MinimumDifficulty: 1e-12, MaximumDifficulty: 1e12,
		PPLNSWindow: 2, PoolFeeBPS: 100, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	<-node.ready

	keys, err := types.QdayKeysFromSeed([32]byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	login := keys.Public.String() + ".asic01"
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writeRPC(t, conn, map[string]any{"id": 1, "method": "mining.subscribe", "params": []string{"gominer"}})
	if message := readWire(t, reader); string(message.Error) != "null" {
		t.Fatalf("subscribe failed: %+v", message)
	}
	writeRPC(t, conn, map[string]any{"id": 2, "method": "mining.authorize", "params": []string{login, "x"}})
	if message := readWire(t, reader); string(message.Result) != "true" {
		t.Fatalf("authorize failed: %+v", message)
	}
	if message := readWire(t, reader); message.Method != "mining.set_difficulty" {
		t.Fatalf("expected difficulty, got %+v", message)
	}
	notify := readWire(t, reader)
	if notify.Method != "mining.notify" || len(notify.Params) != 9 {
		t.Fatalf("wrong job: %+v", notify)
	}
	var jobID, ntime string
	if err := json.Unmarshal(notify.Params[0], &jobID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(notify.Params[7], &ntime); err != nil {
		t.Fatal(err)
	}
	writeRPC(t, conn, map[string]any{"id": 3, "method": "mining.submit", "params": []string{login, jobID, "", ntime, "0000000000000000"}})
	if message := readWire(t, reader); string(message.Result) != "true" || string(message.Error) != "null" {
		t.Fatalf("share failed: %+v", message)
	}
	select {
	case share := <-database.shares:
		if share.Address != keys.Public.String() || share.Worker != "asic01" || share.Work.Sign() <= 0 {
			t.Fatalf("wrong stored share: %+v", share)
		}
	case <-time.After(time.Second):
		t.Fatal("share was not stored")
	}
	select {
	case block := <-database.blocks:
		if block.RewardAtomic != "248000" || block.FeesAtomic != "240000" || block.FoundByWorker != "asic01" {
			t.Fatalf("wrong allocated block: %+v", block)
		}
	case <-time.After(time.Second):
		t.Fatal("block was not allocated")
	}
	select {
	case encoded := <-node.submits:
		block, err := hex.DecodeString(encoded)
		if err != nil || len(block) < 48 || binary.LittleEndian.Uint64(block[32:40]) != 0 {
			t.Fatalf("wrong submitted block: %x, %v", block, err)
		}
	case <-time.After(time.Second):
		t.Fatal("block was not submitted")
	}
	node.mu.Lock()
	if len(node.workNonces) != 1 || node.workNonces[0] == 0 {
		t.Fatalf("worker did not receive independent work nonce: %v", node.workNonces)
	}
	node.mu.Unlock()
}

func TestWorkerAndTimestampValidation(t *testing.T) {
	keys, _ := types.QdayKeysFromSeed([32]byte{9})
	if address, worker, err := parseWorkerLogin(keys.Public.String() + ".rig_1"); err != nil || address != keys.Public.String() || worker != "rig_1" {
		t.Fatalf("valid login rejected: %q %q %v", address, worker, err)
	}
	if _, _, err := parseWorkerLogin(keys.Public.String() + ".bad.label"); err == nil {
		t.Fatal("invalid worker label accepted")
	}
	if value, ok := passwordDifficulty("x,d=35000"); !ok || value != 35000 {
		t.Fatalf("password difficulty = %v, %v", value, ok)
	}
	var target [32]byte
	for i := range target {
		target[i] = 0xff
	}
	template, err := newWorkTemplate(syntheticTemplate(t, target, 1))
	if err != nil {
		t.Fatal(err)
	}
	owner := &client{address: keys.Public.String(), worker: "rig", difficulty: 1e-12}
	job, err := template.newJob("job", 1, owner, time.Now(), 1e-12)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := job.solve("", "", littleEndianHex(job.timestamp+1), "0000000000000000"); err == nil {
		t.Fatal("accepted timestamp outside the assigned job")
	}
	harder, err := template.newJob("harder", 2, owner, time.Now(), 1e12)
	if err != nil {
		t.Fatal(err)
	}
	if harder.shareTarget != target || harder.difficulty != template.networkDiff {
		t.Fatal("share target became harder than the network block target")
	}
}
