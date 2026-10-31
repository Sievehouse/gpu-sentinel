// Package canonical produces the deterministic JSON encoding used for every
// hash in GPU Sentinel: object keys sorted, no insignificant whitespace,
// no HTML escaping, and numbers kept exactly as encoded.
//
// It implements the subset of RFC 8785 (JSON Canonicalization Scheme) that
// this project needs. Hashed structures avoid floating-point values; money
// and percentages travel as decimal strings.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Marshal returns the canonical JSON encoding of v.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SHA256Hex returns the lowercase hex SHA-256 of the canonical encoding of v.
func SHA256Hex(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
