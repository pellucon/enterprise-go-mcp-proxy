// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// users.go is the in-memory user store for demo-authorization-server. This
// is a demo: every user's password is always identical to its username
// (e.g. admin/admin, alice/alice) — there is no separate password field or
// hashing anywhere in this server.
package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

const adminUsername = "admin"

// errCannotDeleteAdmin is also returned (as a plain message check) when a
// caller tries to add/edit "admin" via upsertMany — its roles are fixed
// and it's never modified through the admin API.
var errCannotDeleteAdmin = fmt.Errorf("the built-in %q user cannot be deleted", adminUsername)

type user struct {
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
}

type userStore struct {
	mu    sync.Mutex
	users map[string]user
}

// newUserStore returns a store seeded with the built-in admin user — never
// explicitly added, always present, never removable or editable via the
// admin API (see delete/upsertMany below).
func newUserStore() *userStore {
	return &userStore{
		users: map[string]user{
			adminUsername: {Username: adminUsername, Roles: []string{"admin"}},
		},
	}
}

// authenticate checks the username==password demo rule, that the user
// actually exists (the built-in admin always does), and that they don't
// hold the "deactivated" role (see go-gardening-demo's garden-customer-
// admin/garden-staff-admin, the only current source of that role via
// mark_customer_deactivated/mark_staffer_deactivated) — a deactivated
// user's login must fail here, at the one place both the browser
// (POST /authorize) and direct-credential (oauthclient's -username/
// -password) login paths both funnel through, rather than needing a
// separate check in each.
func (s *userStore) authenticate(username, password string) (user, bool) {
	if username == "" || username != password {
		return user{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	if !ok || hasRole(u.Roles, "deactivated") {
		return user{}, false
	}
	return u, true
}

type userUpsertRequest struct {
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
}

// upsertMany creates or replaces (never merges) each entry's role list.
// Naming "admin" in any entry rejects the whole batch — nothing is
// partially applied.
func (s *userStore) upsertMany(entries []userUpsertRequest) ([]user, error) {
	for _, e := range entries {
		if e.Username == "" {
			return nil, fmt.Errorf("username must not be empty")
		}
		if e.Username == adminUsername {
			return nil, fmt.Errorf("the built-in %q user cannot be added or edited via this API", adminUsername)
		}
	}

	s.mu.Lock()
	for _, e := range entries {
		roles := append([]string{}, e.Roles...)
		s.users[e.Username] = user{Username: e.Username, Roles: roles}
	}
	s.mu.Unlock()

	return s.list(), nil
}

// list returns every user sorted by username, including the built-in admin.
func (s *userStore) list() []user {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]user, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

type errNotFound struct{ username string }

func (e errNotFound) Error() string { return fmt.Sprintf("user %q not found", e.username) }

// delete removes username. Deleting "admin" is always rejected.
func (s *userStore) delete(username string) error {
	if username == adminUsername {
		return errCannotDeleteAdmin
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[username]; !ok {
		return errNotFound{username: username}
	}
	delete(s.users, username)
	return nil
}

// requireAdmin wraps next with HTTP Basic Auth (checked against the same
// username==password rule) plus a check that the authenticated user holds
// the "admin" role. Used by every /admin/users endpoint.
func requireAdmin(store *userStore, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="demo-authorization-server admin API"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		u, ok := store.authenticate(username, password)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="demo-authorization-server admin API"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !hasRole(u.Roles, "admin") {
			http.Error(w, "forbidden: admin role required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
