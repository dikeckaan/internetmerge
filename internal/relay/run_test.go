package relay

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateKeyDecodesTo32Bytes(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		t.Fatalf("generated key not valid base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("key length = %d, want 32", len(raw))
	}
}

func TestGenerateKeyIsRandom(t *testing.T) {
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	if a == b {
		t.Fatal("two generated keys are identical")
	}
}

func TestDecodeKeyErrors(t *testing.T) {
	if _, err := DecodeKey(""); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("empty key: want 'no key' error, got %v", err)
	}
	if _, err := DecodeKey("!!!not base64!!!"); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("bad base64: want base64 error, got %v", err)
	}
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	if _, err := DecodeKey(short); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short key: want 'too short' error, got %v", err)
	}
}

func TestDecodeKeyValid(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	key, err := DecodeKey(good)
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("decoded length = %d, want 32", len(key))
	}
}

func TestListenAndServeRejectsBadKey(t *testing.T) {
	if err := ListenAndServe("127.0.0.1:0", "", nil); err == nil {
		t.Fatal("expected error for empty key")
	}
}
