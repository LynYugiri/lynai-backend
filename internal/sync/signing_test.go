package sync

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestSyncRequestFixedVector(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	bodyHash, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	privateKey := ed25519.NewKeyFromSeed(seed)
	message := SyncRequestMessage(1, "42", "session-vector-1", "kzdvvj2umnduyauf35o36k6kw462mujvra46tn3uqgzovmihocga",
		1700000000123, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX", "POST", "/sync/changes", bodyHash)
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))

	const expectedMessage = "4c796e41492f76312f73796e632d72657175657374000001000000020001000200000002343200030000001073657373696f6e2d766563746f722d310004000000346b7a6476766a32756d6e64757961756633356f33366b366b773436326d756a7672613436746e337571677a6f766d69686f6367610005000000080000018bcfe5687b00060000002041414543417751464267634943516f4c4441304f4478415245684d5546525958000700000004504f535400080000000d2f73796e632f6368616e676573000900000020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	const expectedSignature = "ijPzt7fLykodsX18MwAXhwlvPUMMdtWqraTQUUwREshEkGTXxu09x8Ziz8a3dqkU2dCL6GVLgRoBKxzcGXSaCw"
	if got := hex.EncodeToString(message); got != expectedMessage {
		t.Fatalf("message = %s", got)
	}
	if signature != expectedSignature {
		t.Fatalf("signature = %s", signature)
	}
}
