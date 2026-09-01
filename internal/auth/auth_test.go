package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNewStaticStore(t *testing.T) {
	s, err := NewStaticStore("alice:gw_one, ci:gw_two ")
	if err != nil {
		t.Fatalf("NewStaticStore() failed: %v", err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}

	key, ok := s.Lookup("gw_one")
	if !ok {
		t.Fatal("a configured secret was not recognised")
	}
	if key.Name != "alice" {
		t.Errorf("Name = %q, want alice", key.Name)
	}
	// Whitespace around an entry must not become part of the secret.
	if _, ok := s.Lookup("gw_two"); !ok {
		t.Error("the trimmed secret was not recognised")
	}
	if _, ok := s.Lookup("gw_nope"); ok {
		t.Error("an unknown secret was accepted")
	}
	if _, ok := s.Lookup(""); ok {
		t.Error("an empty secret was accepted")
	}
}

func TestNewStaticStore_Rejects(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want error
	}{
		{"empty", "", ErrNoKeys},
		{"only separators", " , , ", ErrNoKeys},
		{"no secret", "alice:", nil},
		{"no name", ":gw_one", nil},
		{"no colon", "gw_one", nil},
		// Two callers sharing a credential makes rate limits and audit trails
		// meaningless, so it is a configuration error rather than a quirk.
		{"duplicate secret", "alice:gw_one,bob:gw_one", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewStaticStore(tt.spec)
			if err == nil {
				t.Fatalf("NewStaticStore(%q) = %v, nil; want an error", tt.spec, s)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// The store must never be able to hand back a usable credential.
func TestStaticStore_KeepsNoPlaintext(t *testing.T) {
	const secret = "gw_supersecret"

	s, err := NewStaticStore("alice:" + secret)
	if err != nil {
		t.Fatalf("NewStaticStore() failed: %v", err)
	}

	for digest, key := range s.byDigest {
		if strings.Contains(string(digest[:]), secret) {
			t.Error("the secret is recoverable from the store")
		}
		if strings.Contains(key.Name, secret) {
			t.Error("the secret leaked into the key name")
		}
	}
}

func TestGenerate(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}
	if !strings.HasPrefix(first, KeyPrefix) {
		t.Errorf("key = %q, want the %q prefix", first, KeyPrefix)
	}
	// 24 bytes hex-encoded, plus the prefix.
	if want := len(KeyPrefix) + 48; len(first) != want {
		t.Errorf("length = %d, want %d", len(first), want)
	}

	second, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}
	if first == second {
		t.Error("two generated keys are identical")
	}
}
