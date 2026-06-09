package bond

import "testing"

func TestAuthMACVerifies(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var sid [16]byte
	var nonce [16]byte
	sid[0], nonce[0] = 9, 7
	mac := computeMAC(key, sid, 1, 2, nonce)
	if !verifyMAC(key, sid, 1, 2, nonce, mac) {
		t.Fatal("valid MAC rejected")
	}
}

func TestAuthMACRejectsWrongKeyOrNonce(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("ffffffffffffffffffffffffffffffff")
	var sid, nonce, nonce2 [16]byte
	nonce2[0] = 1
	mac := computeMAC(key, sid, 0, 1, nonce)
	if verifyMAC(other, sid, 0, 1, nonce, mac) {
		t.Fatal("wrong key accepted")
	}
	if verifyMAC(key, sid, 0, 1, nonce2, mac) {
		t.Fatal("wrong nonce accepted")
	}
}
