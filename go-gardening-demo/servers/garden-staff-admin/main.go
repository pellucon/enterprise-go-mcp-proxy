// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// garden-staff-admin is a go-gardening-demo MCP backend server: it holds
// the gardening store's staffer list in memory and keeps the auth server's
// role assignments for each staffer in sync with it. Per the
// gardening-demo spec: "These tools should update the
// demo-authorization-server with the roles specified for a staffer."
//
// See garden-customer-admin's doc comment for why -auth-server-url
// defaults to host.docker.internal (demo-authorization-server has no k8s
// manifest and runs host-side).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/authserver"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StafferRecord is this server's own record for a staffer: the same shape
// as entities.Staffer plus Deactivated, which entities.Staffer has no
// field for (deactivation is a live-server concept, not part of the
// generated seed data).
type StafferRecord struct {
	ID          string                  `json:"id"`
	Details     entities.ContactDetails `json:"details"`
	Roles       []string                `json:"roles"`
	Deactivated bool                    `json:"deactivated"`
}

// rolesFor computes the full auth server role list for rec: its functional
// roles as-is, plus "deactivated" if flagged. Recomputed fresh from rec
// every time rather than merged against whatever the auth server currently
// has — garden-staff-admin is the exclusive writer of "staff-*" usernames
// there, so there's nothing to merge against.
func rolesFor(rec StafferRecord) []string {
	roles := append([]string{}, rec.Roles...)
	if rec.Deactivated {
		roles = append(roles, "deactivated")
	}
	return roles
}

type staffAdmin struct {
	mu         sync.Mutex
	staffers   map[string]StafferRecord
	staffRoles map[string]entities.StaffRole // reference only, local
	authServer *authserver.Client
}

func newStaffAdmin(authServer *authserver.Client) *staffAdmin {
	return &staffAdmin{
		staffers:   make(map[string]StafferRecord),
		staffRoles: make(map[string]entities.StaffRole),
		authServer: authServer,
	}
}

type LoadResult struct {
	Loaded int `json:"loaded"`
}

type LoadStaffRolesArgs struct {
	StaffRoles []entities.StaffRole `json:"staffRoles" jsonschema:"The full staff_roles.json list to load."`
}

// LoadPromptsArgs is load_prompts' own bulk-load shape - see
// go-gardening-demo/promptstore's own doc comment for why this
// server's whole prompt set lives in data (prompts.json) rather than
// hand-written Go prompt functions.
type LoadPromptsArgs struct {
	Prompts []promptstore.Prompt `json:"prompts" jsonschema:"This server's entries from prompts.json (server field stripped - already implied by which server's load_prompts you're calling)."`
}

// loadStaffRoles is a local-only reference load — no auth-server write.
func (sa *staffAdmin) loadStaffRoles(ctx context.Context, req *mcp.CallToolRequest, args LoadStaffRolesArgs) (*mcp.CallToolResult, LoadResult, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	for _, r := range args.StaffRoles {
		sa.staffRoles[r.ID] = r
	}
	return nil, LoadResult{Loaded: len(args.StaffRoles)}, nil
}

type LoadStaffersArgs struct {
	Staffers []entities.Staffer `json:"staffers" jsonschema:"The full staffers.json list to load."`
}

// loadStaffers commits to memory only if the single bulk auth-server POST
// for all N staffers succeeds — atomic, no drift between the two stores.
func (sa *staffAdmin) loadStaffers(ctx context.Context, req *mcp.CallToolRequest, args LoadStaffersArgs) (*mcp.CallToolResult, LoadResult, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	candidates := make([]StafferRecord, len(args.Staffers))
	authUsers := make([]authserver.User, len(args.Staffers))
	for i, s := range args.Staffers {
		rec := StafferRecord{ID: s.ID, Details: s.Details, Roles: s.Roles}
		candidates[i] = rec
		authUsers[i] = authserver.User{Username: rec.ID, Roles: rolesFor(rec)}
	}

	if err := sa.authServer.UpsertUsers(ctx, authUsers); err != nil {
		return nil, LoadResult{}, fmt.Errorf("load_staffers: %w", err)
	}
	for _, rec := range candidates {
		sa.staffers[rec.ID] = rec
	}
	return nil, LoadResult{Loaded: len(args.Staffers)}, nil
}

type AddOrUpdateStafferArgs struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Address string   `json:"address,omitempty"`
	Phone   string   `json:"phone,omitempty"`
	Roles   []string `json:"roles" jsonschema:"Staff role ids, e.g. [\"customer-service-representative\"]."`
}

// addOrUpdateStaffer upserts by id. An existing staffer's Deactivated
// flag is preserved (updating a staffer's roles/contact info shouldn't
// silently reactivate them).
func (sa *staffAdmin) addOrUpdateStaffer(ctx context.Context, req *mcp.CallToolRequest, args AddOrUpdateStafferArgs) (*mcp.CallToolResult, StafferRecord, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	rec := StafferRecord{
		ID:      args.ID,
		Details: entities.ContactDetails{Name: args.Name, Address: args.Address, Phone: args.Phone},
		Roles:   args.Roles,
	}
	if existing, ok := sa.staffers[args.ID]; ok {
		rec.Deactivated = existing.Deactivated
	}

	if err := sa.authServer.UpsertUser(ctx, rec.ID, rolesFor(rec)); err != nil {
		return nil, StafferRecord{}, fmt.Errorf("add_or_update_staffer: %w", err)
	}
	sa.staffers[rec.ID] = rec
	return nil, rec, nil
}

type MarkStafferDeactivatedArgs struct {
	ID          string `json:"id"`
	Deactivated bool   `json:"deactivated"`
}

func (sa *staffAdmin) markStafferDeactivated(ctx context.Context, req *mcp.CallToolRequest, args MarkStafferDeactivatedArgs) (*mcp.CallToolResult, StafferRecord, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	rec, ok := sa.staffers[args.ID]
	if !ok {
		return nil, StafferRecord{}, fmt.Errorf("mark_staffer_deactivated: unknown staffer %q", args.ID)
	}
	rec.Deactivated = args.Deactivated

	if err := sa.authServer.UpsertUser(ctx, rec.ID, rolesFor(rec)); err != nil {
		return nil, StafferRecord{}, fmt.Errorf("mark_staffer_deactivated: %w", err)
	}
	sa.staffers[rec.ID] = rec
	return nil, rec, nil
}

// fetchUnresolvedServiceCalls relays to garden-customer-service.
// get_customer_service_calls (resolved=false only) via the proxy, using the
// same backend-calls-back-into-the-proxy mechanism as garden-online-store's
// fetchActiveDeals/fetchCustomerType (see CLAUDE.md's "Internal
// identity assertions" section) - same shape/error convention, reused
// rather than reimplemented differently.
func fetchUnresolvedServiceCalls(ctx context.Context, req *mcp.CallToolRequest) ([]entities.CustomerServiceCall, *mcp.CallToolResult) {
	incoming := relayheaders.Extract(req.GetExtra().Header)
	if incoming.Assertion == "" || incoming.CallbackURL == "" {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "missing identity assertion or callback URL — is internalAuth and selfUrl configured on the proxy?"}},
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "garden-staff-admin", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   incoming.CallbackURL,
		HTTPClient: incoming.Client(nil),
	}, nil)
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to connect back to proxy at %s: %v", incoming.CallbackURL, err)}},
		}
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "garden-customer-service.get_customer_service_calls",
		Arguments: map[string]any{"resolved": false},
	})
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("relayed call to garden-customer-service.get_customer_service_calls failed: %v", err)}},
		}
	}
	if result.IsError {
		return nil, &mcp.CallToolResult{IsError: true, Content: result.Content}
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to re-marshal get_customer_service_calls output: %v", err)}},
		}
	}
	var out struct {
		Calls []entities.CustomerServiceCall `json:"calls"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to decode get_customer_service_calls output: %v", err)}},
		}
	}
	return out.Calls, nil
}

// validateStaffHandling is a business-logic-validation triggered tool (see
// CLAUDE.md's "Business logic validation" section) guarding
// mark_staffer_deactivated: rejects deactivating (Deactivated:true) a
// staffer who is still the handler or escalation target of any unresolved
// customer service call. Reactivating (Deactivated:false) is never
// restricted by this check - only deactivation is. Stateless (no receiver
// needed, same convention garden-customer-service's modifyCustomerSentiment
// already follows).
func validateStaffHandling(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
	// in.CallRequest's real Go type depends on how this tool was invoked -
	// see garden-online-store's validateStoreRequests for why re-marshaling
	// handles both the native (json.RawMessage) and discovered/relayed
	// (map[string]any) cases uniformly.
	raw, err := json.Marshal(in.CallRequest)
	if err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_staff_handling: failed to re-marshal CallRequest: %w", err)
	}
	var call struct {
		Arguments MarkStafferDeactivatedArgs `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_staff_handling: failed to decode CallRequest: %w", err)
	}
	if !call.Arguments.Deactivated {
		return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	calls, errResult := fetchUnresolvedServiceCalls(ctx, req)
	if errResult != nil {
		return errResult, triggered_tools_runtime.AuthorizationOutput{}, nil
	}

	for _, c := range calls {
		if c.HandledByStafferID == call.Arguments.ID || (c.EscalatedToStafferID != nil && *c.EscalatedToStafferID == call.Arguments.ID) {
			return nil, triggered_tools_runtime.AuthorizationOutput{
				Authorized: false,
				Reason:     fmt.Sprintf("staffer %q is still assigned to unresolved customer service call %q", call.Arguments.ID, c.ID),
			}, nil
		}
	}
	return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
}

type GetStaffersArgs struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty" jsonschema:"Case-insensitive substring match."`
	Role        string `json:"role,omitempty" jsonschema:"Match staffers holding this role id among their (possibly multiple) roles."`
	Deactivated *bool  `json:"deactivated,omitempty" jsonschema:"Omit for no filter on deactivated status."`
}

// GetStaffersResult wraps get_staffers' list in a named field rather than
// returning a bare array: some MCP clients (e.g. Claude Desktop) require
// every tool's inferred outputSchema to be a JSON object at the top
// level, and a bare []StafferRecord Out infers as an array instead.
type GetStaffersResult struct {
	Staffers []StafferRecord `json:"staffers"`
}

// getStaffers ANDs together whichever filters are non-empty; no filters
// at all returns every staffer.
func (sa *staffAdmin) getStaffers(ctx context.Context, req *mcp.CallToolRequest, args GetStaffersArgs) (*mcp.CallToolResult, GetStaffersResult, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	nameQuery := strings.ToLower(args.Name)
	out := make([]StafferRecord, 0, len(sa.staffers))
	for _, rec := range sa.staffers {
		if args.ID != "" && rec.ID != args.ID {
			continue
		}
		if nameQuery != "" && !strings.Contains(strings.ToLower(rec.Details.Name), nameQuery) {
			continue
		}
		if args.Role != "" && !hasRole(rec.Roles, args.Role) {
			continue
		}
		if args.Deactivated != nil && rec.Deactivated != *args.Deactivated {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetStaffersResult{Staffers: out}, nil
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func createServer(sa *staffAdmin) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garden-staff-admin", Version: "1.0.0"}, nil)
	prompts := promptstore.New(server)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_staff_roles",
		Description: "Bulk-load the staff role reference list (staff_roles.json's shape). Local reference only - no auth server write.",
	}, sa.loadStaffRoles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_staffers",
		Description: "Bulk-load the staffer list (staffers.json's shape), syncing each staffer's auth server roles.",
	}, sa.loadStaffers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_or_update_staffer",
		Description: "Add or update (upsert by id) one staffer, syncing their auth server roles.",
	}, sa.addOrUpdateStaffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mark_staffer_deactivated",
		Description: "Set a staffer's deactivated flag, adding/removing the \"deactivated\" role in the auth server without disturbing their other roles.",
	}, sa.markStafferDeactivated)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_staffers",
		Description: "List staffers, optionally filtered by id/name/role/deactivated (all filters given are ANDed together; no filters returns everyone).",
	}, sa.getStaffers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_staff_handling",
		Description: "Business logic validation for mark_staffer_deactivated: reject deactivating a staffer who is still the handler or escalation target of an unresolved customer service call. Hidden from ordinary clients - invocable only by go-proxy's orchestration engine as a call-authorization step.",
		Meta:        mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true},
	}, validateStaffHandling)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_prompts",
		Description: "Bulk-load this server's MCP prompts (prompts.json's shape, filtered to this server's entries). Replaces the current set - a prompt loaded before but not named this time is removed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args LoadPromptsArgs) (*mcp.CallToolResult, LoadResult, error) {
		return nil, LoadResult{Loaded: prompts.Load(args.Prompts)}, nil
	})

	return server
}

func main() {
	var port, authServerURL, authServerUsername, authServerPassword, logLevel string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&authServerURL, "auth-server-url", "http://host.docker.internal:8102",
		"Base URL of the auth server's admin API. Defaults to demo-authorization-server (it has no k8s manifest and runs host-side, hence host.docker.internal) - overridden at deploy time via scripts/deploy.sh's --auth-server-url flag.")
	flag.StringVar(&authServerUsername, "auth-server-username", "admin", "Basic Auth username for the auth server's admin API")
	flag.StringVar(&authServerPassword, "auth-server-password", "admin", "Basic Auth password for the auth server's admin API")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this server has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	// Never log authServerPassword.
	logging.Infof("startup configuration: authServerUrl=%s authServerUsername=%s", authServerURL, authServerUsername)

	authServer := authserver.New(authServerURL, authServerUsername, authServerPassword)
	sa := newStaffAdmin(authServer)
	server := createServer(sa)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting garden-staff-admin on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
