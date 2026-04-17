package srv

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// googleClaims holds the verified claims from a Google ID token.
type googleClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// Google's public key endpoint
const googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

var (
	cachedKeys   *jwks
	cachedKeysAt time.Time
	cachedKeysMu sync.Mutex
)

func fetchGoogleKeys(ctx context.Context) (*jwks, error) {
	cachedKeysMu.Lock()
	defer cachedKeysMu.Unlock()

	if cachedKeys != nil && time.Since(cachedKeysAt) < time.Hour {
		return cachedKeys, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", googleCertsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch google certs: %w", err)
	}
	defer resp.Body.Close()

	var keys jwks
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("decode google certs: %w", err)
	}

	cachedKeys = &keys
	cachedKeysAt = time.Now()
	return &keys, nil
}

func base64URLDecode(s string) ([]byte, error) {
	// Add padding if needed
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

func jwkToRSAPublicKey(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64URLDecode(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode N: %w", err)
	}
	eBytes, err := base64URLDecode(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode E: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}

// verifyGoogleIDToken verifies a Google ID token JWT and returns claims.
func verifyGoogleIDToken(ctx context.Context, tokenString, clientID string) (*googleClaims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}

	// Decode header to get kid
	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", header.Alg)
	}

	// Fetch Google's public keys
	keys, err := fetchGoogleKeys(ctx)
	if err != nil {
		return nil, err
	}

	// Find matching key
	var matchingKey *jwk
	for i := range keys.Keys {
		if keys.Keys[i].Kid == header.Kid {
			matchingKey = &keys.Keys[i]
			break
		}
	}
	if matchingKey == nil {
		// Keys may have rotated, force refresh
		cachedKeysMu.Lock()
		cachedKeys = nil
		cachedKeysMu.Unlock()
		keys, err = fetchGoogleKeys(ctx)
		if err != nil {
			return nil, err
		}
		for i := range keys.Keys {
			if keys.Keys[i].Kid == header.Kid {
				matchingKey = &keys.Keys[i]
				break
			}
		}
		if matchingKey == nil {
			return nil, errors.New("no matching key found")
		}
	}

	// Verify signature
	pubKey, err := jwkToRSAPublicKey(*matchingKey)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}

	signedContent := parts[0] + "." + parts[1]
	signatureBytes, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	hash := sha256.Sum256([]byte(signedContent))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], signatureBytes); err != nil {
		return nil, fmt.Errorf("invalid signature: %w", err)
	}

	// Decode and verify payload
	payloadBytes, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var payload struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Exp           int64  `json:"exp"`
		Iat           int64  `json:"iat"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}

	// Verify issuer
	if payload.Iss != "accounts.google.com" && payload.Iss != "https://accounts.google.com" {
		return nil, fmt.Errorf("invalid issuer: %s", payload.Iss)
	}

	// Verify audience matches our client ID
	if clientID != "" && payload.Aud != clientID {
		return nil, fmt.Errorf("invalid audience: %s", payload.Aud)
	}

	// Verify expiration
	if time.Now().Unix() > payload.Exp {
		return nil, errors.New("token expired")
	}

	if payload.Sub == "" || payload.Email == "" {
		return nil, errors.New("missing sub or email in token")
	}

	return &googleClaims{
		Sub:   payload.Sub,
		Email: payload.Email,
	}, nil
}
