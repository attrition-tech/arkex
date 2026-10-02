// Package testutil contains shared black-box test servers.
package testutil

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// SIWC is a signed fake OpenID provider. Tests may register auth, token, and
// revocation handlers on Mux before making requests. Discovery and JWKS are
// always provided by the fixture.
type SIWC struct {
	Server *httptest.Server
	Mux    *http.ServeMux
	key    *rsa.PrivateKey
}

func NewSIWC(t testing.TB) *SIWC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &SIWC{Mux: http.NewServeMux(), key: key}
	s.Server = httptest.NewServer(s.Mux)
	s.Mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": s.Server.URL, "authorization_endpoint": s.Server.URL + "/api/accounts/authorize", "token_endpoint": s.Server.URL + "/api/accounts/oauth/token", "revocation_endpoint": s.Server.URL + "/revoke", "jwks_uri": s.Server.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	s.Mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key", "n": n, "e": e}}})
	})
	t.Cleanup(s.Server.Close)
	return s
}

// IDToken returns a real RS256 ID token suitable for exercising production
// OIDC verification. The nonce should be captured from Login.URL.
func (s *SIWC) IDToken(subject, clientID, nonce string) string {
	return s.Sign(map[string]any{"iss": s.Server.URL, "sub": subject, "aud": clientID, "nonce": nonce, "email": "dev@example.com", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
}

// Sign allows negative tests to vary individual signed claims independently.
func (s *SIWC) Sign(payload map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	claims, _ := json.Marshal(payload)
	h := base64.RawURLEncoding.EncodeToString(header)
	c := base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(h + "." + c))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(fmt.Sprintf("sign test ID token: %v", err))
	}
	return h + "." + c + "." + base64.RawURLEncoding.EncodeToString(sig)
}
