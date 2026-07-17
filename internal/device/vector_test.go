package device

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestEnrollmentFixedVector(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	challenge, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	deviceID := deriveDeviceID(publicKey)
	message := EnrollmentMessage(
		1, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX", challenge, "42", "session-vector-1",
		deviceID, publicKey, "LynAI Test Device", "linux",
	)
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))

	const expectedDeviceID = "kzdvvj2umnduyauf35o36k6kw462mujvra46tn3uqgzovmihocga"
	const expectedMessage = "4c796e41492f76312f656e726f6c6c6d656e7400000100000002000100020000002041414543417751464267634943516f4c4441304f4478415245684d5546525958000300000020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f000400000002343200050000001073657373696f6e2d766563746f722d310006000000346b7a6476766a32756d6e64757961756633356f33366b366b773436326d756a7672613436746e337571677a6f766d69686f63676100070000002003a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b80008000000114c796e41492054657374204465766963650009000000056c696e7578"
	const expectedSignature = "6Mr7DylNhi4lvmRlcAkODJoRmQx0XbJlocqFS2oWate0HRz-jM_0ZbblRzaBZvMHL4R-hyrMPcFAYKyF7PjZDg"
	if deviceID != expectedDeviceID {
		t.Fatalf("deviceId = %s", deviceID)
	}
	if got := hex.EncodeToString(message); got != expectedMessage {
		t.Fatalf("message = %s", got)
	}
	if signature != expectedSignature {
		t.Fatalf("signature = %s", signature)
	}
}
