package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

const (
	// jwtBackdate is how far the issued-at claim is moved into the past, to
	// absorb clock skew between this process and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how far the expiry claim is moved into the future.
	// GitHub rejects App JWTs valid for more than 10 minutes.
	jwtLifetime = 540 * time.Second
)

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

// parsePrivateKey decodes a PEM-encoded RSA private key in either PKCS#1
// ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY") form.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want an RSA key", parsed)
	}
	return key, nil
}

// SignJWT returns a compact RS256 JWT authenticating as the App itself.
//
// The header is {"alg":"RS256","typ":"JWT"} and the claims are
// {"iat": now-60, "exp": now+540, "iss": AppID} — iss is a string, as GitHub's
// documentation specifies. The private key may be PKCS#1 or PKCS#8 PEM.
func SignJWT(cr Credentials, now time.Time) (string, error) {
	if cr.AppID == "" {
		return "", fmt.Errorf("github: sign jwt: app id is empty")
	}
	key, err := parsePrivateKey(cr.PrivateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: %w", err)
	}

	headerJSON, err := json.Marshal(jwtHeader{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: encode header: %w", err)
	}
	claimsJSON, err := json.Marshal(jwtClaims{
		IssuedAt:  now.Add(-jwtBackdate).Unix(),
		ExpiresAt: now.Add(jwtLifetime).Unix(),
		Issuer:    cr.AppID,
	})
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: encode claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}
