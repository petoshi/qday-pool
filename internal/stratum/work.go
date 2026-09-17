package stratum

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/petoshi/qday-pool/internal/nodeapi"
	"go.sia.tech/core/types"
	"golang.org/x/crypto/blake2b"
)

var (
	difficultyOneTarget = mustBigInt("00000000ffff0000000000000000000000000000000000000000000000000000")
	twoTo256            = new(big.Int).Lsh(big.NewInt(1), 256)
)

func mustBigInt(encoded string) *big.Int {
	n, ok := new(big.Int).SetString(encoded, 16)
	if !ok {
		panic("invalid integer constant")
	}
	return n
}

type workTemplate struct {
	height       uint64
	parent       [32]byte
	target       [32]byte
	timestamp    int64
	coinbase1    []byte
	merkleBranch [][32]byte
	block        []byte
	networkDiff  float64
	bits         string
	longPollID   string
	transactions int
	rewardAtomic string
	feesAtomic   string
	workNonce    uint64
}

type job struct {
	*workTemplate
	id          string
	owner       *client
	address     string
	worker      string
	sequence    uint64
	timestamp   uint64
	difficulty  float64
	shareTarget [32]byte
}

func decode32(name, encoded string) ([32]byte, error) {
	var value [32]byte
	b, err := hex.DecodeString(encoded)
	if err != nil || len(b) != len(value) {
		return value, fmt.Errorf("%s must be 32 bytes of hexadecimal data", name)
	}
	copy(value[:], b)
	return value, nil
}

func newWorkTemplate(template nodeapi.Template) (*workTemplate, error) {
	if template.LongPollID == "" || template.Height == 0 || template.Timestamp <= 0 || template.WorkNonce == 0 {
		return nil, errors.New("QDAY node returned an incomplete pool template")
	}
	if template.Stratum.Block == "" || template.PayoutAtomic == "" || template.FeesAtomic == "" {
		return nil, errors.New("QDAY node has no pool template data; update QDAY to v0.8.1 or newer")
	}
	blockReward, okReward := new(big.Int).SetString(template.BlockRewardAtomic, 10)
	fees, okFees := new(big.Int).SetString(template.FeesAtomic, 10)
	payout, okPayout := new(big.Int).SetString(template.PayoutAtomic, 10)
	if !okReward || !okFees || !okPayout || blockReward.Sign() < 0 || fees.Sign() < 0 || payout.Sign() <= 0 || new(big.Int).Add(blockReward, fees).Cmp(payout) != 0 {
		return nil, errors.New("QDAY template contains inconsistent atomic rewards")
	}
	if blockReward.String() != template.BlockRewardAtomic || fees.String() != template.FeesAtomic || payout.String() != template.PayoutAtomic {
		return nil, errors.New("QDAY template contains non-canonical atomic rewards")
	}
	if len(template.Transactions) == 0 {
		return nil, errors.New("QDAY template has no miner marker transaction")
	}
	parent, err := decode32("previous block hash", template.PreviousBlockHash)
	if err != nil {
		return nil, err
	}
	target, err := decode32("target", template.Target)
	if err != nil {
		return nil, err
	}
	commitment, err := decode32("commitment", template.Commitment)
	if err != nil {
		return nil, err
	}
	coinbase1, err := hex.DecodeString(template.Transactions[len(template.Transactions)-1].Data)
	if err != nil || len(coinbase1) == 0 {
		return nil, errors.New("rightmost QDAY template transaction is invalid")
	}
	branches := make([][32]byte, len(template.Stratum.MerkleBranch))
	if len(branches) > 64 {
		return nil, errors.New("QDAY node returned too many Merkle branches")
	}
	for i, encoded := range template.Stratum.MerkleBranch {
		branches[i], err = decode32("Merkle branch", encoded)
		if err != nil {
			return nil, err
		}
	}
	root := transactionLeaf(coinbase1)
	for _, left := range branches {
		root = merklePair(left, root)
	}
	if root != commitment {
		return nil, errors.New("QDAY Stratum branch does not match its commitment")
	}
	block, err := hex.DecodeString(template.Stratum.Block)
	if err != nil || len(block) < 80 {
		return nil, errors.New("QDAY node returned an invalid complete block")
	}
	header, err := hex.DecodeString(template.Header)
	if err != nil || len(header) != 80 {
		return nil, errors.New("QDAY node returned an invalid 80-byte header")
	}
	if !bytes.Equal(header[:32], parent[:]) || !bytes.Equal(header[48:], commitment[:]) || binary.LittleEndian.Uint64(header[40:48]) != uint64(template.Timestamp) {
		return nil, errors.New("QDAY template fields do not describe the same header")
	}
	decoder := types.NewBufDecoder(block)
	var encodedBlock types.V2Block
	encodedBlock.DecodeFrom(decoder)
	if decoder.Err() != nil {
		return nil, errors.New("QDAY node returned an invalid complete block")
	}
	var canonical bytes.Buffer
	encoder := types.NewEncoder(&canonical)
	encodedBlock.EncodeTo(encoder)
	if encoder.Flush() != nil || !bytes.Equal(canonical.Bytes(), block) {
		return nil, errors.New("QDAY node returned a non-canonical complete block")
	}
	decodedBlock := encodedBlock.Cast()
	blockHeader := decodedBlock.Header()
	if blockHeader.ParentID != types.BlockID(parent) || blockHeader.Nonce != binary.LittleEndian.Uint64(header[32:40]) || blockHeader.Timestamp.Unix() != template.Timestamp || blockHeader.Commitment != types.Hash256(commitment) {
		return nil, errors.New("QDAY complete block does not match its work header")
	}
	networkDiff, err := targetDifficulty(target)
	if err != nil {
		return nil, err
	}
	return &workTemplate{
		height:       template.Height,
		parent:       parent,
		target:       target,
		timestamp:    template.Timestamp,
		coinbase1:    coinbase1,
		merkleBranch: branches,
		block:        block,
		networkDiff:  networkDiff,
		bits:         template.Bits,
		longPollID:   template.LongPollID,
		transactions: len(template.Transactions),
		rewardAtomic: template.PayoutAtomic,
		feesAtomic:   template.FeesAtomic,
		workNonce:    template.WorkNonce,
	}, nil
}

func (t *workTemplate) newJob(id string, sequence uint64, owner *client, timestamp time.Time, difficulty float64) (*job, error) {
	when := uint64(timestamp.Unix())
	if when < uint64(t.timestamp) {
		when = uint64(t.timestamp)
	}
	var shareTarget [32]byte
	if difficulty >= t.networkDiff {
		// A share target must never be harder than the block target. Otherwise
		// hardware can discard a perfectly valid block before submitting it.
		difficulty, shareTarget = t.networkDiff, t.target
	} else {
		var err error
		shareTarget, err = targetForDifficulty(difficulty)
		if err != nil {
			return nil, err
		}
	}
	address, worker := owner.identity()
	return &job{workTemplate: t, id: id, owner: owner, address: address, worker: worker, sequence: sequence, timestamp: when, difficulty: difficulty, shareTarget: shareTarget}, nil
}

func transactionLeaf(transaction []byte) [32]byte {
	data := make([]byte, 1+len(transaction))
	copy(data[1:], transaction)
	return blake2b.Sum256(data)
}

func merklePair(left, right [32]byte) [32]byte {
	var data [65]byte
	data[0] = 1
	copy(data[1:33], left[:])
	copy(data[33:], right[:])
	return blake2b.Sum256(data[:])
}

func targetDifficulty(target [32]byte) (float64, error) {
	t := new(big.Int).SetBytes(target[:])
	if t.Sign() == 0 {
		return 0, errors.New("target is zero")
	}
	ratio := new(big.Float).SetPrec(256).Quo(new(big.Float).SetInt(difficultyOneTarget), new(big.Float).SetInt(t))
	difficulty, accuracy := ratio.Float64()
	if accuracy == big.Below {
		difficulty = math.Nextafter(difficulty, math.Inf(1))
	}
	if difficulty <= 0 || math.IsInf(difficulty, 0) || math.IsNaN(difficulty) {
		return 0, errors.New("target difficulty cannot be represented")
	}
	return difficulty, nil
}

func targetForDifficulty(difficulty float64) ([32]byte, error) {
	var target [32]byte
	if difficulty <= 0 || math.IsInf(difficulty, 0) || math.IsNaN(difficulty) {
		return target, errors.New("difficulty must be finite and positive")
	}
	d := new(big.Float).SetPrec(256).SetFloat64(difficulty)
	value, _ := new(big.Float).SetPrec(256).Quo(new(big.Float).SetInt(difficultyOneTarget), d).Int(nil)
	if value.Sign() <= 0 {
		value.SetInt64(1)
	}
	max := new(big.Int).Sub(twoTo256, big.NewInt(1))
	if value.Cmp(max) > 0 {
		value.Set(max)
	}
	b := value.Bytes()
	copy(target[len(target)-len(b):], b)
	return target, nil
}

// TargetWork returns the expected hashes represented by a target.
func TargetWork(target [32]byte) *big.Int {
	denominator := new(big.Int).Add(new(big.Int).SetBytes(target[:]), big.NewInt(1))
	return new(big.Int).Quo(new(big.Int).Set(twoTo256), denominator)
}

func littleEndianHex(value uint64) string {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	return hex.EncodeToString(encoded[:])
}

func (j *job) notifyParams(clean bool) []any {
	branches := make([]string, len(j.merkleBranch))
	for i := range j.merkleBranch {
		branches[i] = hex.EncodeToString(j.merkleBranch[i][:])
	}
	return []any{j.id, hex.EncodeToString(j.parent[:]), hex.EncodeToString(j.coinbase1), "", branches, "", j.bits, littleEndianHex(j.timestamp), clean}
}

func (j *job) solve(extraNonce1, extraNonce2, encodedTime, encodedNonce string) (block []byte, hash [32]byte, network bool, err error) {
	if extraNonce1 != "" || extraNonce2 != "" {
		return nil, hash, false, errors.New("QDAY pool requires an empty extranonce")
	}
	timeBytes, err := hex.DecodeString(encodedTime)
	if err != nil || len(timeBytes) != 8 {
		return nil, hash, false, errors.New("timestamp must contain 8 bytes of hexadecimal data")
	}
	if binary.LittleEndian.Uint64(timeBytes) != j.timestamp {
		return nil, hash, false, errors.New("timestamp does not match the assigned job")
	}
	nonceBytes, err := hex.DecodeString(encodedNonce)
	if err != nil || len(nonceBytes) != 8 {
		return nil, hash, false, errors.New("nonce must contain 8 bytes of hexadecimal data")
	}
	root := transactionLeaf(j.coinbase1)
	for _, left := range j.merkleBranch {
		root = merklePair(left, root)
	}
	header := make([]byte, 80)
	copy(header[:32], j.parent[:])
	copy(header[32:40], nonceBytes)
	copy(header[40:48], timeBytes)
	copy(header[48:], root[:])
	hash = blake2b.Sum256(header)
	if bytes.Compare(hash[:], j.shareTarget[:]) > 0 {
		return nil, hash, false, errors.New("low difficulty share")
	}
	network = bytes.Compare(hash[:], j.target[:]) <= 0
	if network {
		block = append([]byte(nil), j.block...)
		copy(block[32:40], nonceBytes)
		copy(block[40:48], timeBytes)
	}
	return block, hash, network, nil
}
