package update

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// keys.txt holds the release public keys trusted by this build, one per line:
//
//	<keyid> <base64 ed25519 public key>
//
// Add a new line to rotate; remove a line to revoke. Generate with
// `go run ./scripts/relsign keygen`.
//
//go:embed keys.txt
var keysFile string

// KeyID is the first 8 bytes of SHA-256(public key), hex encoded. It lets a
// signature file name which key produced it without shipping the key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// PublicKeyLine formats pub for keys.txt.
func PublicKeyLine(pub ed25519.PublicKey) string {
	return KeyID(pub) + " " + base64.StdEncoding.EncodeToString(pub)
}

// SignatureLine signs data and formats the result for a .sig file:
//
//	<keyid> <base64 signature>
func SignatureLine(priv ed25519.PrivateKey, data []byte) string {
	pub := priv.Public().(ed25519.PublicKey)
	return KeyID(pub) + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
}

// ParseKeys parses keys.txt content. Blank lines and '#' comments are ignored.
func ParseKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, b64, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("keys line %d: expected \"<keyid> <base64>\"", n)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("keys line %d: not a base64 ed25519 public key", n)
		}
		pub := ed25519.PublicKey(raw)
		if KeyID(pub) != id {
			return nil, fmt.Errorf("keys line %d: key id %s does not match key (want %s)", n, id, KeyID(pub))
		}
		keys = append(keys, pub)
	}
	return keys, sc.Err()
}

// EmbeddedKeys returns the release keys compiled into this binary.
func EmbeddedKeys() ([]ed25519.PublicKey, error) {
	keys, err := ParseKeys(keysFile)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("no release public keys embedded in this build")
	}
	return keys, nil
}

// ErrBadSignature is returned when no trusted key verifies the signature.
var ErrBadSignature = errors.New("signature verification failed")

// Verify checks sigFile (one or more SignatureLine entries) against data using
// the trusted keys. Any one valid line from a trusted key is sufficient.
func Verify(keys []ed25519.PublicKey, data, sigFile []byte) error {
	byID := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		byID[KeyID(k)] = k
	}
	sc := bufio.NewScanner(strings.NewReader(string(sigFile)))
	seen := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seen++
		id, b64, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pub, trusted := byID[id]
		if !trusted {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		if ed25519.Verify(pub, data, sig) {
			return nil
		}
	}
	if seen == 0 {
		return fmt.Errorf("%w: signature file is empty", ErrBadSignature)
	}
	return ErrBadSignature
}
