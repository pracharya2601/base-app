package main

import "testing"

// isRecordRoute splits the data plane (RBAC-gated, key acts as its service
// account) from the control plane (scope-gated, key acts as superuser). A wrong
// answer here is an auth-bypass class bug, so cover the edges.
func TestIsRecordRoute(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/collections/articles/records", true},
		{"/api/collections/articles/records/abc123", true},
		{"/api/collections/users/records?filter=x", true},
		// control-plane: schema management is /api/collections WITHOUT /records
		{"/api/collections", false},
		{"/api/collections/articles", false},
		// superadmin + settings are control-plane
		{"/api/superadmin/provision", false},
		{"/api/settings", false},
		// a "/records" substring outside the collections prefix must not match
		{"/api/superadmin/records-export", false},
		{"/records", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isRecordRoute(c.path); got != c.want {
			t.Errorf("isRecordRoute(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
