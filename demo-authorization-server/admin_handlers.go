// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// admin_handlers.go implements the user-management admin API
// (POST/GET /admin/users, DELETE /admin/users/{username}), all gated by
// requireAdmin (HTTP Basic Auth + the admin role — see users.go).
package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

func handleAdminUsersPost(state *State) http.HandlerFunc {
	return requireAdmin(state.users, func(w http.ResponseWriter, r *http.Request) {
		var entries []userUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			http.Error(w, `invalid JSON body: expected an array of {"username","roles"} objects`, http.StatusBadRequest)
			return
		}
		users, err := state.users.upsertMany(entries)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, users)
	})
}

func handleAdminUsersGet(state *State) http.HandlerFunc {
	return requireAdmin(state.users, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, state.users.list())
	})
}

func handleAdminUsersDelete(state *State) http.HandlerFunc {
	return requireAdmin(state.users, func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		err := state.users.delete(username)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, errCannotDeleteAdmin):
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			var nf errNotFound
			if errors.As(err, &nf) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
