// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RoleMapping maps the gateway's built-in logical roles to whatever role
// name a deployment's IdP actually issues (e.g. an Okta group such as
// "mcp-gateway-admins"). Today there is exactly one built-in role,
// administrator: the only role allowed to call the proxy's native _admin.*
// tools (see IsToolAuthorized).
//
// Deliberately NOT a configsync-managed section: it is read once at
// startup and fixed for the life of the process (AccessPolicyEngine keeps
// it in an immutable field, outside the config Update swaps). Since the
// _admin.* tools are exactly what can change every live-updatable section,
// letting them change who counts as an administrator would let one admin
// hand the gateway to an arbitrary role, or lock every admin out with no
// way back short of a redeploy. Changing it means editing the config file
// and restarting/redeploying, like auth.
type RoleMapping struct {
	// Administrator is the exact role name (matched as a literal string,
	// never as a glob) that grants the administrator role.
	Administrator string `json:"administrator"`
}

// ParseRoleMapping parses the combined config file's "roleMapping"
// section. The section is required and must name a non-empty
// administrator role: there is deliberately no default, so a deployment
// always states explicitly which of its IdP's roles administers the
// gateway, and startup fails rather than guessing.
func ParseRoleMapping(raw json.RawMessage) (RoleMapping, error) {
	if len(raw) == 0 {
		return RoleMapping{}, fmt.Errorf("section is required: it must map \"administrator\" to the role allowed to call _admin.* tools, e.g. {\"administrator\": \"admin\"}")
	}
	var m RoleMapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return RoleMapping{}, err
	}
	if strings.TrimSpace(m.Administrator) == "" {
		return RoleMapping{}, fmt.Errorf("\"administrator\" must name a non-empty role")
	}
	return m, nil
}
