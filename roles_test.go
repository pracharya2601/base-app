package main

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestPermissionTokens(t *testing.T) {
	got := permissionTokens("articles")
	want := []string{"articles:read", "articles:create", "articles:update", "articles:delete"}
	if len(got) != len(want) {
		t.Fatalf("permissionTokens len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// governsPermissions decides which collections get RBAC tokens + auto-rules.
// System collections (and nil) must be excluded so we never try to role-gate
// _superusers, _roles, etc.
func TestGovernsPermissions(t *testing.T) {
	user := core.NewBaseCollection("widgets")
	if !governsPermissions(user) {
		t.Error("a normal (non-system) collection should be governed")
	}

	sys := core.NewBaseCollection("_secret")
	sys.System = true
	if governsPermissions(sys) {
		t.Error("a system collection must NOT be governed")
	}

	if governsPermissions(nil) {
		t.Error("nil collection must not be governed")
	}
}
