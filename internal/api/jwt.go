// Package api is the HTTP API, the dashboard authentication and static hosting.
package api

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
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Issuer = "skibot"
	// TokenTTLSeconds and the cookie maxAge must stay in sync: both use the same
	// value, seconds in the JWT and milliseconds in the cookie.
	TokenTTLSeconds = 86400000
)

type JWT struct {
	private *rsa.PrivateKey
	public  *rsa.PublicKey
}

// LoadOrCreateJWT reads data/jwt/{private,public}.key, creating them if absent.
func LoadOrCreateJWT(dir string) (*JWT, error) {
	if dir == "" {
		dir = "data/jwt"
	}
	privatePath := filepath.Join(dir, "private.key")
	publicPath := filepath.Join(dir, "public.key")

	if _, err := os.Stat(privatePath); err == nil {
		if _, err := os.Stat(publicPath); err == nil {
			privateKey, err := readPrivateKey(privatePath)
			if err != nil {
				return nil, err
			}
			publicKey, err := readPublicKey(publicPath)
			if err != nil {
				return nil, err
			}
			return &JWT{private: privateKey, public: publicKey}, nil
		}
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(privateKey)})
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := os.WriteFile(privatePath, privatePEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(publicPath, publicPEM, 0o644); err != nil {
		return nil, err
	}
	return &JWT{private: privateKey, public: &privateKey.PublicKey}, nil
}

func mustPKCS8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil
	}
	return der
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s does not contain a PEM block", path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("%s is not an RSA private key", path)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func readPublicKey(path string) (*rsa.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s does not contain a PEM block", path)
	}
	if parsed, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := parsed.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}

func b64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func (j *JWT) Issue(claims map[string]any, expiresInSeconds int64) (string, error) {
	if expiresInSeconds <= 0 {
		expiresInSeconds = TokenTTLSeconds
	}
	now := time.Now().Unix()
	payload := map[string]any{}
	for key, value := range claims {
		payload[key] = value
	}
	payload["iss"] = Issuer
	payload["iat"] = now
	payload["exp"] = now + expiresInSeconds

	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(headerJSON) + "." + b64url(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, j.private, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(signature), nil
}

func (j *JWT) Verify(token string) (map[string]any, error) {
	if token == "" {
		return nil, errors.New("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var header map[string]any
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, err
	}
	if header["alg"] != "RS256" {
		return nil, errors.New("unexpected signing algorithm")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(j.public, crypto.SHA256, digest[:], signature); err != nil {
		return nil, errors.New("invalid signature")
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return nil, err
	}
	if exp, ok := payload["exp"].(float64); ok {
		if int64(exp) < time.Now().Unix() {
			return nil, errors.New("token expired")
		}
	}
	if payload["iss"] != Issuer {
		return nil, errors.New("unexpected issuer")
	}
	return payload, nil
}
