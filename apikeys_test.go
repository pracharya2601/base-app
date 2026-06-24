package main

import (
	"regexp"
	"strings"
	"testing"
)

// requiredScope is the control-plane auth gate: a request method+path maps to the
// single scope an API key must hold. Getting this wrong silently over- or
// under-privileges every key, so it's worth pinning every branch.
func TestRequiredScope(t *testing.T) {
	cases := []struct {
		method, path string
		want         string
	}{
		// AI proxy
		{"POST", "/api/ai/openai/generate", "ai:use"},
		{"GET", "/api/ai/catalog", "ai:use"},
		// key management
		{"POST", "/api/superadmin/apikeys", "keys:manage"},
		{"DELETE", "/api/superadmin/apikeys/abc", "keys:manage"},
		// provision
		{"POST", "/api/superadmin/provision", "schema:write"},
		// roles: read vs write by method
		{"GET", "/api/superadmin/roles", "roles:read"},
		{"POST", "/api/superadmin/roles", "roles:write"},
		{"POST", "/api/superadmin/users/roles", "roles:write"},
		{"GET", "/api/superadmin/users/roles", "roles:read"},
		// field-types catalog
		{"GET", "/api/superadmin/field-types", "schema:read"},
		// settings: read vs write
		{"GET", "/api/settings", "settings:read"},
		{"PATCH", "/api/settings", "settings:write"},
		// records: read vs write, and must win over the generic /api/collections branch
		{"GET", "/api/collections/articles/records", "records:read"},
		{"POST", "/api/collections/articles/records", "records:write"},
		{"PATCH", "/api/collections/articles/records/xyz", "records:write"},
		// collections schema (no /records segment)
		{"GET", "/api/collections", "schema:read"},
		{"POST", "/api/collections", "schema:write"},
		// anything unrecognized must fail closed to full admin
		{"GET", "/api/superadmin/scopes", "admin"},
		{"GET", "/some/unknown/path", "admin"},
	}
	for _, c := range cases {
		if got := requiredScope(c.method, c.path); got != c.want {
			t.Errorf("requiredScope(%q, %q) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestHasScope(t *testing.T) {
	cases := []struct {
		name     string
		granted  []string
		required string
		want     bool
	}{
		{"exact match", []string{"records:read"}, "records:read", true},
		{"admin satisfies anything", []string{"admin"}, "schema:write", true},
		{"admin among others", []string{"records:read", "admin"}, "settings:write", true},
		{"no match", []string{"records:read"}, "records:write", false},
		{"empty grants deny", []string{}, "records:read", false},
		{"nil grants deny", nil, "ai:use", false},
		{"match later in slice", []string{"a", "b", "ai:use"}, "ai:use", true},
	}
	for _, c := range cases {
		if got := hasScope(c.granted, c.required); got != c.want {
			t.Errorf("%s: hasScope(%v, %q) = %v, want %v", c.name, c.granted, c.required, got, c.want)
		}
	}
}

func TestSha256hex(t *testing.T) {
	// Known SHA-256 vector for "abc".
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := sha256hex("abc"); got != want {
		t.Errorf("sha256hex(\"abc\") = %q, want %q", got, want)
	}
	// Deterministic and 64 hex chars wide.
	if a, b := sha256hex("x"), sha256hex("x"); a != b {
		t.Errorf("sha256hex not deterministic: %q != %q", a, b)
	}
	if l := len(sha256hex("anything")); l != 64 {
		t.Errorf("sha256hex length = %d, want 64", l)
	}
}

// shouldStampLastUsed throttles the per-request "last used" DB write — the
// biggest write-amplification tax found in load testing. Stamp at most once per
// lastUsedStampInterval per key.
func TestShouldStampLastUsed(t *testing.T) {
	const now = 1_000_000
	cases := []struct {
		name string
		last int64
		want bool
	}{
		{"never used (zero) stamps immediately", 0, true},
		{"just stamped, skip", now, false},
		{"within interval, skip", now - (lastUsedStampInterval - 1), false},
		{"exactly at interval, stamp", now - lastUsedStampInterval, true},
		{"well past interval, stamp", now - 10*lastUsedStampInterval, true},
	}
	for _, c := range cases {
		if got := shouldStampLastUsed(c.last, now); got != c.want {
			t.Errorf("%s: shouldStampLastUsed(%d, %d) = %v, want %v", c.name, c.last, now, got, c.want)
		}
	}
}

var rawKeyRe = regexp.MustCompile(`^pbk_[0-9a-f]{48}$`)

func TestNewRawKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		k, err := newRawKey()
		if err != nil {
			t.Fatalf("newRawKey error: %v", err)
		}
		if !rawKeyRe.MatchString(k) {
			t.Fatalf("key %q does not match pbk_<48 hex>", k)
		}
		if !strings.HasPrefix(k, "pbk_") {
			t.Fatalf("key %q missing pbk_ prefix", k)
		}
		if seen[k] {
			t.Fatalf("newRawKey produced a duplicate: %q", k)
		}
		seen[k] = true
	}
}
