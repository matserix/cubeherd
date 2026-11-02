package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Token holds the SHA-256 of the agent's bearer token. The token itself is never stored.
type Token struct{ hash [sha256.Size]byte }

// NewToken hashes a token the operator supplied (CUBEHERD_AGENT_TOKEN).
func NewToken(plain string) Token { return Token{sha256.Sum256([]byte(plain))} }

// Check compares a presented token in constant time.
func (t Token) Check(plain string) bool {
	h := sha256.Sum256([]byte(plain))
	return subtle.ConstantTimeCompare(h[:], t.hash[:]) == 1
}

// LoadOrCreateToken resolves the agent's token. A configured value wins. Otherwise the hash stored in
// dir/token.sha256 is used; on the very first start a random token is generated, only its hash is
// written, and the plain value is returned once so the operator can copy it into the dashboard.
func LoadOrCreateToken(dir, configured string) (t Token, generated string, err error) {
	if configured != "" {
		return NewToken(configured), "", nil
	}
	path := filepath.Join(dir, "token.sha256")
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(raw) != sha256.Size {
			return Token{}, "", errors.New("agent token file is corrupt: " + path)
		}
		copy(t.hash[:], raw)
		return t, "", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Token{}, "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, "", err
	}
	generated = "chag_" + base64.RawURLEncoding.EncodeToString(buf)
	t = NewToken(generated)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Token{}, "", err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(t.hash[:])+"\n"), 0o600); err != nil {
		return Token{}, "", err
	}
	return t, generated, nil
}
