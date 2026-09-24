// Command relsign is the release-side half of arkex's update system.
//
// It never ships to users; goreleaser invokes it with `go run ./scripts/relsign`.
//
//	relsign keygen                      print a new ed25519 key pair (private key
//	                                    to stdout, public key line to stderr)
//	relsign sign FILE                   write FILE.sig using $ARKEX_SIGNING_KEY
//	relsign manifest -version V -out F  write the channel manifest JSON
//
// The signature file format and the manifest schema are defined in
// internal/update, which is also the verifier — keep them in one place.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dantearo/arkex/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "check-key":
		err = checkKey()
	case "sign":
		err = sign(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relsign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relsign keygen | check-key | sign FILE | manifest -version V -out FILE")
	os.Exit(2)
}

// keygen prints the private key (base64 seed) to stdout and the public key
// line for internal/update/keys.txt to stderr, plus a PEM for install.sh.
func keygen() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed()))

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "# public key line for internal/update/keys.txt:\n%s\n\n", update.PublicKeyLine(pub))
	fmt.Fprintf(os.Stderr, "# PEM (SubjectPublicKeyInfo) for install.sh:\n%s", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyEnv := fs.String("key-env", "ARKEX_SIGNING_KEY", "environment variable holding the base64 ed25519 seed")
	out := fs.String("out", "", "signature path (default FILE.sig)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("sign: exactly one FILE required")
	}
	file := fs.Arg(0)

	priv, err := privateKey(*keyEnv)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sigLine := update.SignatureLine(priv, data)
	if *out == "" {
		*out = file + ".sig"
	}
	return os.WriteFile(*out, []byte(sigLine+"\n"), 0o644)
}

func privateKey(keyEnv string) (ed25519.PrivateKey, error) {
	seedB64 := strings.TrimSpace(os.Getenv(keyEnv))
	if seedB64 == "" {
		return nil, fmt.Errorf("%s is not set", keyEnv)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a base64 %d-byte ed25519 seed", keyEnv, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// checkKey proves the configured private key is trusted by installed clients,
// without printing it or writing it to disk.
func checkKey() error {
	priv, err := privateKey("ARKEX_SIGNING_KEY")
	if err != nil {
		return err
	}
	keys, err := update.EmbeddedKeys()
	if err != nil {
		return err
	}
	data := []byte("arkex release key check")
	return update.Verify(keys, data, []byte(update.SignatureLine(priv, data)))
}

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	version := fs.String("version", "", "release version without leading v (required)")
	out := fs.String("out", "", "output path (required)")
	notes := fs.String("notes", "", "optional one-line release note")
	_ = fs.Parse(args)
	if *version == "" || *out == "" {
		return fmt.Errorf("manifest: -version and -out are required")
	}
	m := update.Manifest{
		Version:     strings.TrimPrefix(*version, "v"),
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Notes:       *notes,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*out, append(b, '\n'), 0o644)
}
