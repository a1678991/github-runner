// Package github is a minimal GitHub REST client for App-authenticated
// ephemeral-runner management. Only the handful of endpoints the controller
// needs — no SDK dependency.
package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// ParseRSAPrivateKey parses a GitHub App private key in PKCS#1 or PKCS#8 PEM.
func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

// mintJWT builds the short-lived RS256 App JWT GitHub requires for
// installation-token requests (iat 60s in the past for clock skew, exp
// 9 minutes out — under GitHub's 10-minute cap).
func mintJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	b64 := base64.RawURLEncoding
	header := b64.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprintf("%d", appID),
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64.EncodeToString(sig), nil
}
