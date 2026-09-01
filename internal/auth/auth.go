// Package auth issues and verifies the gateway's own API keys.
//
// Clients authenticate with keys this gateway hands out, never with a
// provider's credentials. That separation is the point: a leaked client key
// costs one revocation, while a leaked OpenAI key costs a rotation and
// whatever was spent in between.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeyPrefix marks a gateway-issued key, so a key found in a log or a git diff
// is recognisable at a glance.
const KeyPrefix = "gw_"

// Key is an authenticated caller.
type Key struct {
	// Name identifies the caller in logs, metrics and rate limit buckets. It
	// is not a secret.
	Name string
}

// Store resolves a presented secret to the caller it belongs to.
type Store interface {
	// Lookup reports the key a secret belongs to. The second result is false
	// for any secret that is not recognised.
	Lookup(secret string) (Key, bool)

	// Len reports how many keys are configured, for startup logging.
	Len() int
}

// StaticStore holds a fixed set of keys loaded at startup.
//
// Secrets are kept only as SHA-256 digests: a memory dump or a stray log of
// the process state cannot leak a usable credential.
type StaticStore struct {
	byDigest map[[32]byte]Key
}

var _ Store = (*StaticStore)(nil)

// ErrNoKeys reports an empty configuration, which is almost always a mistake
// rather than an intentional open gateway.
var ErrNoKeys = errors.New("no API keys configured")

// NewStaticStore parses a comma-separated specification of `name:secret`
// pairs, as supplied by GATEWAY_API_KEYS.
//
//	alice:gw_7f3c...,ci:gw_91ab...
//
// Names need not be unique-looking to a human, but duplicated secrets are
// rejected: two callers sharing one credential makes rate limits and audit
// trails meaningless.
func NewStaticStore(spec string) (*StaticStore, error) {
	s := &StaticStore{byDigest: make(map[[32]byte]Key)}

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		name, secret, ok := strings.Cut(entry, ":")
		name, secret = strings.TrimSpace(name), strings.TrimSpace(secret)
		if !ok || name == "" || secret == "" {
			return nil, fmt.Errorf("malformed key entry %q: want name:secret", entry)
		}

		digest := sha256.Sum256([]byte(secret))
		if existing, taken := s.byDigest[digest]; taken {
			return nil, fmt.Errorf("keys %q and %q share the same secret", existing.Name, name)
		}
		s.byDigest[digest] = Key{Name: name}
	}

	if len(s.byDigest) == 0 {
		return nil, ErrNoKeys
	}
	return s, nil
}

// Lookup implements Store.
func (s *StaticStore) Lookup(secret string) (Key, bool) {
	digest := sha256.Sum256([]byte(secret))

	// The map lookup alone would be constant time enough, but comparing the
	// digests explicitly keeps the intent visible: never compare credentials
	// with ==, whose early exit leaks how much of a guess was right.
	for candidate, key := range s.byDigest {
		if subtle.ConstantTimeCompare(candidate[:], digest[:]) == 1 {
			return key, true
		}
	}
	return Key{}, false
}

// Len implements Store.
func (s *StaticStore) Len() int { return len(s.byDigest) }

// Generate returns a new random key. 24 bytes of entropy is well past the
// point where guessing is the weakest link.
func Generate() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating key: %w", err)
	}
	return KeyPrefix + hex.EncodeToString(raw[:]), nil
}
