package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPrivateKeyValidation(t *testing.T) {
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		t.Setenv("ARKEX_SIGNING_KEY", value)
		if _, err := privateKey("ARKEX_SIGNING_KEY"); err == nil {
			t.Fatal("invalid key accepted")
		} else if value != "" && strings.Contains(err.Error(), value) {
			t.Fatal("error exposes key material")
		}
	}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	t.Setenv("ARKEX_SIGNING_KEY", " \n"+base64.StdEncoding.EncodeToString(seed)+"\n")
	key, err := privateKey("ARKEX_SIGNING_KEY")
	if err != nil || !bytes.Equal(key.Seed(), seed) {
		t.Fatal("valid seed did not round trip")
	}
	if err := checkKey(); err == nil {
		t.Fatal("untrusted signing key accepted")
	}
}
