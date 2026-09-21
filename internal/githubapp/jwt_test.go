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
	"strings"
	"testing"
	"time"
)

// testKey generates an RSA key once per test and returns it with its PKCS#1
// and PKCS#8 PEM encodings.
func testKey(t *testing.T) (key *rsa.PrivateKey, pkcs1PEM, pkcs8PEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pkcs1PEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8PEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return key, pkcs1PEM, pkcs8PEM
}

func TestSignJWT(t *testing.T) {
	key, pkcs1PEM, pkcs8PEM := testKey(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		pem  []byte
	}{
		{"pkcs1", pkcs1PEM},
		{"pkcs8", pkcs8PEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := SignJWT(Credentials{AppID: "123456", PrivateKeyPEM: tc.pem}, now)
			if err != nil {
				t.Fatalf("SignJWT: %v", err)
			}

			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("token has %d segments, want 3: %q", len(parts), token)
			}

			var header struct {
				Alg string `json:"alg"`
				Typ string `json:"typ"`
			}
			decodeSegment(t, parts[0], &header)
			if header.Alg != "RS256" {
				t.Errorf("alg = %q, want RS256", header.Alg)
			}
			if header.Typ != "JWT" {
				t.Errorf("typ = %q, want JWT", header.Typ)
			}

			// iss must decode as a JSON string, not a number.
			var claims struct {
				IssuedAt  int64           `json:"iat"`
				ExpiresAt int64           `json:"exp"`
				Issuer    json.RawMessage `json:"iss"`
			}
			decodeSegment(t, parts[1], &claims)
			if got, want := string(claims.Issuer), `"123456"`; got != want {
				t.Errorf("iss raw = %s, want %s", got, want)
			}
			if got, want := claims.IssuedAt, now.Add(-60*time.Second).Unix(); got != want {
				t.Errorf("iat = %d, want %d", got, want)
			}
			if got := claims.ExpiresAt - claims.IssuedAt; got != 600 {
				t.Errorf("exp-iat = %d, want 600", got)
			}

			signingInput := parts[0] + "." + parts[1]
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatalf("decode signature: %v", err)
			}
			digest := sha256.Sum256([]byte(signingInput))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
				t.Errorf("signature does not verify: %v", err)
			}
		})
	}

	t.Run("garbage pem", func(t *testing.T) {
		if _, err := SignJWT(Credentials{AppID: "1", PrivateKeyPEM: []byte("not a pem")}, now); err == nil {
			t.Fatal("want an error for a non-PEM key, got nil")
		}
	})

	t.Run("pem with garbage der", func(t *testing.T) {
		bad := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nonsense")})
		if _, err := SignJWT(Credentials{AppID: "1", PrivateKeyPEM: bad}, now); err == nil {
			t.Fatal("want an error for an undecodable key, got nil")
		}
	})

	t.Run("empty app id", func(t *testing.T) {
		if _, err := SignJWT(Credentials{PrivateKeyPEM: pkcs1PEM}, now); err == nil {
			t.Fatal("want an error for an empty app id, got nil")
		}
	})
}

func decodeSegment(t *testing.T, segment string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal segment %q: %v", raw, err)
	}
}

func TestCredentialsFromSecretData(t *testing.T) {
	t.Run("flux keys", func(t *testing.T) {
		cr, err := CredentialsFromSecretData(map[string][]byte{
			"githubAppID":             []byte("123"),
			"githubAppInstallationID": []byte("42"),
			"githubAppPrivateKey":     []byte("KEY"),
		})
		if err != nil {
			t.Fatalf("CredentialsFromSecretData: %v", err)
		}
		if cr.AppID != "123" || cr.InstallationID != "42" || string(cr.PrivateKeyPEM) != "KEY" {
			t.Fatalf("got %+v", cr)
		}
	})

	t.Run("arc fallback keys", func(t *testing.T) {
		cr, err := CredentialsFromSecretData(map[string][]byte{
			"app_id":          []byte("123"),
			"installation_id": []byte("42"),
			"private_key":     []byte("KEY"),
		})
		if err != nil {
			t.Fatalf("CredentialsFromSecretData: %v", err)
		}
		if cr.AppID != "123" || cr.InstallationID != "42" || string(cr.PrivateKeyPEM) != "KEY" {
			t.Fatalf("got %+v", cr)
		}
	})

	t.Run("github_app fallback keys", func(t *testing.T) {
		cr, err := CredentialsFromSecretData(map[string][]byte{
			"github_app_id":              []byte("123"),
			"github_app_installation_id": []byte("42"),
			"github_app_private_key":     []byte("KEY"),
		})
		if err != nil {
			t.Fatalf("CredentialsFromSecretData: %v", err)
		}
		if cr.AppID != "123" {
			t.Fatalf("got %+v", cr)
		}
	})

	t.Run("flux keys win over fallback", func(t *testing.T) {
		cr, err := CredentialsFromSecretData(map[string][]byte{
			"githubAppID":             []byte("flux"),
			"app_id":                  []byte("arc"),
			"githubAppInstallationID": []byte("42"),
			"githubAppPrivateKey":     []byte("KEY"),
		})
		if err != nil {
			t.Fatalf("CredentialsFromSecretData: %v", err)
		}
		if cr.AppID != "flux" {
			t.Errorf("AppID = %q, want flux", cr.AppID)
		}
	})

	t.Run("whitespace trimmed", func(t *testing.T) {
		cr, err := CredentialsFromSecretData(map[string][]byte{
			"githubAppID":             []byte("  123\n"),
			"githubAppInstallationID": []byte("\t42\n"),
			"githubAppPrivateKey":     []byte("\n-----BEGIN-----\n"),
		})
		if err != nil {
			t.Fatalf("CredentialsFromSecretData: %v", err)
		}
		if cr.AppID != "123" || cr.InstallationID != "42" {
			t.Fatalf("got %+v", cr)
		}
		if string(cr.PrivateKeyPEM) != "-----BEGIN-----" {
			t.Fatalf("private key = %q", cr.PrivateKeyPEM)
		}
	})

	for _, tc := range []struct {
		name string
		data map[string][]byte
	}{
		{"missing app id", map[string][]byte{
			"githubAppInstallationID": []byte("42"),
			"githubAppPrivateKey":     []byte("KEY"),
		}},
		{"missing installation id", map[string][]byte{
			"githubAppID":         []byte("123"),
			"githubAppPrivateKey": []byte("KEY"),
		}},
		{"missing private key", map[string][]byte{
			"githubAppID":             []byte("123"),
			"githubAppInstallationID": []byte("42"),
		}},
		{"empty value", map[string][]byte{
			"githubAppID":             []byte("   "),
			"githubAppInstallationID": []byte("42"),
			"githubAppPrivateKey":     []byte("KEY"),
		}},
		{"nil data", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CredentialsFromSecretData(tc.data); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}
