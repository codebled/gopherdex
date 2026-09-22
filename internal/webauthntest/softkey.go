// Package webauthntest provides a software passkey for tests: it answers
// WebAuthn requests the way a phone or security key does, with a real
// P-256 key, so tests exercise the same verification as a browser.
package webauthntest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
)

// SoftKey is a software passkey: it answers WebAuthn requests the way a
// phone or security key does, with a real P-256 key, so the tests go
// through the same verification as a browser.
type SoftKey struct {
	RPID, Origin string
	key          *ecdsa.PrivateKey
	id           []byte
	userHandle   []byte
	Counter      uint32
}

// NewSoftKey returns a passkey for the relying party rpID that signs its
// client data as coming from origin. It holds a fresh key and credential ID,
// and is bound to a user handle by its first Create.
func NewSoftKey(t *testing.T, rpID, origin string) *SoftKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, credentialIDLen)
	rand.Read(id)
	return &SoftKey{RPID: rpID, Origin: origin, key: key, id: id}
}

var b64url = base64.RawURLEncoding

// credentialIDLen is the length of a SoftKey's credential ID in bytes.
const credentialIDLen = 16

// Flags: user present, user verified, backup eligible and backed up (a
// synced passkey), and attested credential data included.
const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

func (k *SoftKey) authData(flags byte) []byte {
	rpHash := sha256.Sum256([]byte(k.RPID))
	var buf bytes.Buffer
	buf.Write(rpHash[:])
	buf.WriteByte(flags)
	buf.Write(binary.BigEndian.AppendUint32(nil, k.Counter))
	return buf.Bytes()
}

func (k *SoftKey) clientData(t *testing.T, typ string, challenge []byte) []byte {
	data, err := json.Marshal(map[string]any{"type": typ, "challenge": b64url.EncodeToString(challenge), "origin": k.Origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Create answers navigator.credentials.create() with a new credential,
// remembering the user handle in options for later Gets.
func (k *SoftKey) Create(t *testing.T, options *protocol.CredentialCreation) []byte {
	t.Helper()
	switch h := options.Response.User.ID.(type) {
	case protocol.URLEncodedBase64:
		k.userHandle = h
	case []byte:
		k.userHandle = h
	case string: // after a JSON round trip, as a browser receives it
		var err error
		if k.userHandle, err = b64url.DecodeString(h); err != nil {
			t.Fatalf("user handle %q: %v", h, err)
		}
	default:
		t.Fatalf("user handle of type %T", h)
	}
	pub, err := k.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	x, y := pub[1:33], pub[33:65]
	enc, _ := cbor.CTAP2EncOptions().EncMode()
	cose, err := enc.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	var auth bytes.Buffer
	auth.Write(k.authData(flagUP | flagUV | flagBE | flagBS | flagAT))
	auth.Write(make([]byte, 16)) // AAGUID
	auth.Write(binary.BigEndian.AppendUint16(nil, uint16(credentialIDLen)))
	auth.Write(k.id)
	auth.Write(cose)
	att, err := enc.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{
		"id": b64url.EncodeToString(k.id), "rawId": b64url.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64url.EncodeToString(k.clientData(t, "webauthn.create", options.Response.Challenge)),
			"attestationObject": b64url.EncodeToString(att),
			"transports":        []string{"internal", "hybrid"},
		},
		"clientExtensionResults": map[string]any{},
	})
	return resp
}

// Get answers navigator.credentials.get(), advancing Counter first as an
// authenticator does on every use.
func (k *SoftKey) Get(t *testing.T, options *protocol.CredentialAssertion) []byte {
	t.Helper()
	k.Counter++
	auth := k.authData(flagUP | flagUV | flagBE | flagBS)
	cd := k.clientData(t, "webauthn.get", options.Response.Challenge)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{
		"id": b64url.EncodeToString(k.id), "rawId": b64url.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64url.EncodeToString(cd),
			"authenticatorData": b64url.EncodeToString(auth),
			"signature":         b64url.EncodeToString(sig),
			"userHandle":        b64url.EncodeToString(k.userHandle),
		},
		"clientExtensionResults": map[string]any{},
	})
	return resp
}
