package stratum

import (
	"encoding/hex"
	"testing"
)

func TestObeliskSC1HeaderVector(t *testing.T) {
	decode := func(encoded string) []byte {
		t.Helper()
		value, err := hex.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	coinbase1 := decode("0200010000000000001000000000000000514441590204")
	extraNonce1 := decode("01020304")
	extraNonce2 := decode("05060708")
	coinbase2 := decode("0000")
	transaction := append(append(append(append([]byte{}, coinbase1...), extraNonce1...), extraNonce2...), coinbase2...)
	left := [32]byte{0: 0xaa, 31: 0xbb}
	root := merklePair(left, transactionLeaf(transaction))
	parent := [32]byte{0: 0x51, 31: 0x59}
	header := make([]byte, 80)
	copy(header[:32], parent[:])
	copy(header[40:48], decode("8877665544332211"))
	copy(header[48:], root[:])
	const expected = "510000000000000000000000000000000000000000000000000000000000005900000000000000008877665544332211309f36bacf3562bb7950f00159822c3489ebb89d26544d42664ce21144376dc5"
	if got := hex.EncodeToString(header); got != expected {
		t.Fatalf("stock Obelisk Sia header mismatch\n got %s\nwant %s", got, expected)
	}
}
