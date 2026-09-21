// Package githubapp authenticates as a GitHub App installation and drives the
// Check Runs API.
//
// It is deliberately dependency-free (standard library only): an App JWT is
// signed with RS256 from a PKCS#1 or PKCS#8 private key, exchanged for an
// installation access token that is cached until shortly before it expires,
// and used to create or update check runs on a repository.
package githubapp
