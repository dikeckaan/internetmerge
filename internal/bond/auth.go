package bond

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// computeMAC binds a flow to a session under the shared key.
func computeMAC(key []byte, sid [16]byte, flowIndex, flowCount uint16, nonce [16]byte) [32]byte {
	m := hmac.New(sha256.New, key)
	m.Write(sid[:])
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], flowIndex)
	m.Write(u16[:])
	binary.BigEndian.PutUint16(u16[:], flowCount)
	m.Write(u16[:])
	m.Write(nonce[:])
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// verifyMAC checks a Hello MAC in constant time.
func verifyMAC(key []byte, sid [16]byte, flowIndex, flowCount uint16, nonce [16]byte, mac [32]byte) bool {
	want := computeMAC(key, sid, flowIndex, flowCount, nonce)
	return hmac.Equal(want[:], mac[:])
}
