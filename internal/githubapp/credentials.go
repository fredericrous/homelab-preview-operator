package githubapp

import (
	"fmt"
	"strings"
)

// Credentials identifies one GitHub App installation.
type Credentials struct {
	AppID          string
	InstallationID string
	PrivateKeyPEM  []byte
}

// cacheKey identifies the installation whose access token is cached.
func (c Credentials) cacheKey() string {
	return c.AppID + "/" + c.InstallationID
}

// Secret keys understood by CredentialsFromSecretData, most preferred first.
//
// The first group is the shape produced by `flux create secret githubapp`; the
// rest are the ARC / Vault spellings commonly found in existing secrets.
var (
	appIDKeys = []string{
		"githubAppID",
		"github_app_id",
		"app_id",
	}
	installationIDKeys = []string{
		"githubAppInstallationID",
		"github_app_installation_id",
		"installation_id",
	}
	privateKeyKeys = []string{
		"githubAppPrivateKey",
		"github_app_private_key",
		"private_key",
	}
)

// lookup returns the first non-empty, whitespace-trimmed value among keys.
func lookup(data map[string][]byte, keys []string) []byte {
	for _, k := range keys {
		v, ok := data[k]
		if !ok {
			continue
		}
		if trimmed := []byte(strings.TrimSpace(string(v))); len(trimmed) > 0 {
			return trimmed
		}
	}
	return nil
}

// CredentialsFromSecretData reads a Kubernetes Secret's data.
//
// Preferred keys are Flux's `flux create secret githubapp` shape: githubAppID,
// githubAppInstallationID, githubAppPrivateKey. The ARC / Vault shape (app_id,
// installation_id, private_key, and their github_app_* spellings) is accepted
// as a fallback. Values are whitespace-trimmed; it is an error for any of the
// three to be missing or empty.
func CredentialsFromSecretData(data map[string][]byte) (Credentials, error) {
	appID := lookup(data, appIDKeys)
	if len(appID) == 0 {
		return Credentials{}, fmt.Errorf("github: secret is missing an app id (one of %s)", strings.Join(appIDKeys, ", "))
	}
	installationID := lookup(data, installationIDKeys)
	if len(installationID) == 0 {
		return Credentials{}, fmt.Errorf("github: secret is missing an installation id (one of %s)", strings.Join(installationIDKeys, ", "))
	}
	privateKey := lookup(data, privateKeyKeys)
	if len(privateKey) == 0 {
		return Credentials{}, fmt.Errorf("github: secret is missing a private key (one of %s)", strings.Join(privateKeyKeys, ", "))
	}
	return Credentials{
		AppID:          string(appID),
		InstallationID: string(installationID),
		PrivateKeyPEM:  privateKey,
	}, nil
}
