package evidence

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Entry kinds written by Capacity Ledger tools.
const (
	KindKey         = "key"         // self-signed key announcement
	KindObservation = "observation" // raw fault or measurement
	KindEvent       = "event"       // availability event for the SLA engine
	KindHeartbeat   = "heartbeat"   // "this writer was alive at this time"
	KindStatement   = "statement"   // sealed SLA statement
	KindBundle      = "bundle"      // digest of a signed evidence bundle
)

// KeyAnnouncement is the payload of a KindKey entry. The entry is signed by
// the key it announces, which proves possession of the private key. It does
// not prove who holds it: a verifier decides that when enrolling the key.
type KeyAnnouncement struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
	Node      string `json:"node,omitempty"`
	Role      string `json:"role,omitempty"`
	Software  string `json:"software,omitempty"`
}

// PublicKeyPEM encodes a public key as PKIX PEM.
func PublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ParsePublicKeyPEM decodes a PKIX PEM Ed25519 public key.
func ParsePublicKeyPEM(text string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return pub, nil
}

// CheckAnnouncement verifies that e is a well-formed key announcement signed
// by the key it announces, and returns the announcement and the key.
func CheckAnnouncement(e Entry) (KeyAnnouncement, ed25519.PublicKey, error) {
	var a KeyAnnouncement
	if e.Kind != KindKey {
		return a, nil, fmt.Errorf("seq %d is a %q entry, not a key announcement", e.Seq, e.Kind)
	}
	if err := e.DecodePayload(&a); err != nil {
		return a, nil, fmt.Errorf("seq %d: %w", e.Seq, err)
	}
	pub, err := ParsePublicKeyPEM(a.PublicKey)
	if err != nil {
		return a, nil, fmt.Errorf("seq %d: %w", e.Seq, err)
	}
	id := KeyID(pub)
	if a.KeyID != id || e.KeyID != id {
		return a, nil, fmt.Errorf("seq %d: key id does not match the announced public key", e.Seq)
	}
	h, err := e.ComputeHash()
	if err != nil || h != e.Hash {
		return a, nil, fmt.Errorf("seq %d: content does not match its hash", e.Seq)
	}
	digest, _ := hex.DecodeString(e.Hash)
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil || !ed25519.Verify(pub, digest, sig) {
		return a, nil, fmt.Errorf("seq %d: announcement is not signed by the announced key", e.Seq)
	}
	return a, pub, nil
}

// LoadOrCreateKey returns the private key in dir, creating a new key pair
// there if none exists. created reports whether a key was generated.
func LoadOrCreateKey(dir string) (priv ed25519.PrivateKey, created bool, err error) {
	path := filepath.Join(dir, PrivateKeyFile)
	if _, statErr := os.Stat(path); statErr == nil {
		priv, err = LoadPrivateKey(path)
		return priv, false, err
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, statErr
	}
	privPath, _, _, err := GenerateKeyPair(dir)
	if err != nil {
		return nil, false, err
	}
	priv, err = LoadPrivateKey(privPath)
	return priv, true, err
}
