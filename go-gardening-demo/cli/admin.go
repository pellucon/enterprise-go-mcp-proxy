// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/configmerge"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// applicationServerPrefix is the namespace prefix every go-gardening-demo
// server's tools share (matching sections' own "garden-*" Server values) —
// it's what separates the admin section's "application tools" (this demo)
// from "platform tools" (everything else the connected role can see).
const applicationServerPrefix = "garden-"

// runAdminCommand dispatches the CLI's "admin" section. Every tool but one
// is answered either locally from a tools/list (plus, for
// list_platform_tools, one call to the proxy's own proxy_utility.
// list_triggered_tools) or via one of the proxy's own _admin.export_*/
// update_* tools — never by forwarding to a garden-* backend directly.
// load_prompts is the one deliberate exception: prompts aren't a
// configsync-managed proxy config section the way accessRules/
// declarativeGuardrails/orchestration are, they're backend-owned data, so
// there's no _admin.* tool for them to go through — runLoadPrompts calls
// each named server's own <server>.load_prompts tool directly instead (see
// its own doc comment). Each tool parses its own flags from tokens
// (list_application_tools/list_platform_tools take -verbose, every other
// tool here takes -file) since they don't share a shape.
func runAdminCommand(ctx context.Context, session *mcp.ClientSession, tool string, tokens []string) error {
	switch tool {
	case "list_application_tools":
		verbose, err := parseVerboseFlag(tokens)
		if err != nil {
			return err
		}
		return listApplicationTools(ctx, session, verbose)
	case "list_platform_tools":
		verbose, err := parseVerboseFlag(tokens)
		if err != nil {
			return err
		}
		return listPlatformTools(ctx, session, verbose)
	case "update_access_rules":
		path, err := parseFileFlag(tokens)
		if err != nil {
			return err
		}
		return runUpdateAccessRules(ctx, session, path)
	case "update_declarative_guardrails":
		path, err := parseFileFlag(tokens)
		if err != nil {
			return err
		}
		return runUpdateDeclarativeGuardrails(ctx, session, path)
	case "update_orchestration":
		path, err := parseFileFlag(tokens)
		if err != nil {
			return err
		}
		return runUpdateOrchestration(ctx, session, path)
	case "load_prompts":
		path, err := parseFileFlag(tokens)
		if err != nil {
			return err
		}
		return runLoadPrompts(ctx, session, path)
	default:
		return fmt.Errorf("unknown admin tool %q (valid: list_application_tools, list_platform_tools, update_access_rules, update_declarative_guardrails, update_orchestration, load_prompts)", tool)
	}
}

// parseVerboseFlag is admin's own tiny flag parser for list_application_tools/
// list_platform_tools — neither takes anything else (see buildArguments,
// the generic parser every other section's tools go through instead).
func parseVerboseFlag(tokens []string) (bool, error) {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "print each tool's full Description/InputSchema/OutputSchema instead of just its name")
	if err := fs.Parse(tokens); err != nil {
		return false, err
	}
	return *verbose, nil
}

// parseFileFlag is update_access_rules' own tiny flag parser, mirroring
// every load_* tool's -file convention (see buildArguments) even though
// update_access_rules isn't itself a load_* tool routed through sections.
func parseFileFlag(tokens []string) (string, error) {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	path := fs.String("file", "", "path to a JSON array of access_control.AccessibilityRule (e.g. go-gardening-demo/initial-data/client_access_rules.json)")
	if err := fs.Parse(tokens); err != nil {
		return "", err
	}
	if *path == "" {
		return "", fmt.Errorf("update_access_rules requires -file <path>")
	}
	return *path, nil
}

// printToolLine prints one listing entry: its name (plus " *triggered" when
// triggered is true), and — only when verbose is true — the indented JSON
// of tool's full Description/InputSchema/OutputSchema beneath it.
func printToolLine(name string, triggered, verbose bool, tool *mcp.Tool) {
	label := name
	if triggered {
		label += " *triggered"
	}
	fmt.Println(label)
	if !verbose || tool == nil {
		return
	}
	raw, err := json.MarshalIndent(tool, "  ", "  ")
	if err != nil {
		fmt.Printf("  <failed to marshal tool: %v>\n", err)
		return
	}
	fmt.Println("  " + string(raw))
}

// listApplicationTools prints every visible tool belonging to one of
// go-gardening-demo's own garden-* servers — i.e. the tools every other CLI
// section actually calls.
func listApplicationTools(ctx context.Context, session *mcp.ClientSession, verbose bool) error {
	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("admin list_application_tools: %w", err)
	}
	var tools []*mcp.Tool
	for _, t := range res.Tools {
		if strings.HasPrefix(t.Name, applicationServerPrefix) {
			tools = append(tools, t)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for _, t := range tools {
		printToolLine(t.Name, false, verbose, t)
	}
	return nil
}

// listPlatformTools prints every visible tool NOT belonging to one of
// go-gardening-demo's own garden-* servers — _admin.*, proxy_utility.*,
// platform-mcp-servers' own tools, etc. — plus every triggered tool
// (CLAUDE.md's "Triggered tools / orchestration"), which an
// ordinary tools/list never includes at all since a triggered tool is
// deliberately hidden from clients by design. A triggered tool's name is
// suffixed " *triggered" so the two sources stay visually distinguishable
// in the printed list. Calling proxy_utility.list_triggered_tools directly
// (rather than via proxy_utility.call_tool) requires a role authorized for
// it — in every committed *.cfg file that's the "admin" role, matching this
// CLI's own admin-section framing.
func listPlatformTools(ctx context.Context, session *mcp.ClientSession, verbose bool) error {
	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("admin list_platform_tools: %w", err)
	}

	type entry struct {
		name      string
		triggered bool
		tool      *mcp.Tool
	}
	seen := map[string]int{} // name -> index into entries
	var entries []entry
	for _, t := range res.Tools {
		if strings.HasPrefix(t.Name, applicationServerPrefix) {
			continue
		}
		seen[t.Name] = len(entries)
		entries = append(entries, entry{name: t.Name, tool: t})
	}

	// Always fetched verbose regardless of our own -verbose flag: it's the
	// only source of a triggered tool's name at all (see doc comment
	// above), and fetching the full object every time — rather than
	// switching proxy_utility.list_triggered_tools' own verbose argument on
	// our -verbose flag — keeps this call, and parseTriggeredTools' shape,
	// identical either way; verbose only changes what we print locally.
	triggeredTools, err := listTriggeredTools(ctx, session)
	if err != nil {
		return fmt.Errorf("admin list_platform_tools: %w", err)
	}
	for _, t := range triggeredTools {
		if strings.HasPrefix(t.Name, applicationServerPrefix) {
			continue
		}
		if i, ok := seen[t.Name]; ok {
			entries[i].triggered = true
			entries[i].tool = t
			continue
		}
		seen[t.Name] = len(entries)
		entries = append(entries, entry{name: t.Name, triggered: true, tool: t})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	for _, e := range entries {
		printToolLine(e.name, e.triggered, verbose, e.tool)
	}
	return nil
}

// listTriggeredTools calls proxy_utility.list_triggered_tools with
// verbose=true and parses its result into full *mcp.Tool objects.
func listTriggeredTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "proxy_utility.list_triggered_tools",
		Arguments: map[string]any{"verbose": true},
	})
	if err != nil {
		return nil, fmt.Errorf("proxy_utility.list_triggered_tools call failed: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("proxy_utility.list_triggered_tools returned an error: %s", toolErrorText(res.Content))
	}
	return parseTriggeredTools(res.StructuredContent)
}

// parseTriggeredTools is the pure decode step of listTriggeredTools, split
// out for direct testing: a raw (non-typed) CallTool response decodes
// StructuredContent as generic any (here, []any of map[string]any for
// proxy_utility.list_triggered_tools' verbose=true shape) — round-tripping
// it through JSON into []*mcp.Tool is simpler and less error-prone than
// hand-walking that generic structure field by field.
func parseTriggeredTools(structuredContent any) ([]*mcp.Tool, error) {
	raw, err := json.Marshal(structuredContent)
	if err != nil {
		return nil, fmt.Errorf("failed to re-marshal structuredContent: %w", err)
	}
	var tools []*mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("expected an array of Tool objects: %w", err)
	}
	return tools, nil
}

// runUpdateAccessRules reads path — a JSON array of access_control.
// AccessibilityRule, e.g. go-gardening-demo/initial-data/
// client_access_rules.json, generated by gendata/accessrules.go's
// buildClientAccessRules — fetches the gateway's current access rules via
// _admin.export_role_permissions, merges in the new rules (see
// configmerge.MergeAccessRules — also used offline by go-gardening-demo/
// patchcfg to apply the same merge directly to a committed *.cfg file's
// own baked-in accessRules section, with no live proxy involved), and
// writes the result back via _admin.update_role_permissions. Both admin
// tools require the caller's role to be exactly "admin"
// (CLAUDE.md's "Auth and access control" section) — the same requirement
// cli init's own -username admin -password admin already satisfies.
func runUpdateAccessRules(ctx context.Context, session *mcp.ClientSession, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("admin update_access_rules: failed to read %s: %w", path, err)
	}
	var newRules []access_control.AccessibilityRule
	if err := json.Unmarshal(raw, &newRules); err != nil {
		return fmt.Errorf("admin update_access_rules: failed to parse %s as a JSON array of access rules: %w", path, err)
	}

	current, err := exportAccessRules(ctx, session)
	if err != nil {
		return fmt.Errorf("admin update_access_rules: %w", err)
	}

	merged := configmerge.MergeAccessRules(current.Config.ClientAccessRules, newRules)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "_admin.update_role_permissions",
		Arguments: map[string]any{
			"version": current.Version,
			"config":  map[string]any{"clientAccessRules": merged},
		},
	})
	if err != nil {
		return fmt.Errorf("admin update_access_rules: _admin.update_role_permissions call failed: %w", err)
	}
	if res.IsError {
		return fmt.Errorf("admin update_access_rules: _admin.update_role_permissions rejected the call: %s", toolErrorText(res.Content))
	}
	printContent(res.Content)
	fmt.Printf("%d existing rule(s) kept, %d new rule(s) applied from %s.\n", len(merged)-len(newRules), len(newRules), path)
	return nil
}

// exportAccessRules calls _admin.export_role_permissions and parses its
// result the same round-trip-through-JSON way parseTriggeredTools does.
func exportAccessRules(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[access_control.AccessConfig], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_role_permissions", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, fmt.Errorf("_admin.export_role_permissions call failed: %w", err)
	}
	if res.IsError {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, fmt.Errorf("_admin.export_role_permissions returned an error: %s", toolErrorText(res.Content))
	}
	rawJSON, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, fmt.Errorf("failed to re-marshal structuredContent: %w", err)
	}
	var out configsync.VersionedConfig[access_control.AccessConfig]
	if err := json.Unmarshal(rawJSON, &out); err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, fmt.Errorf("failed to parse export result: %w", err)
	}
	return out, nil
}

// runUpdateDeclarativeGuardrails reads path — a JSON array of
// guardrailcfg.RawDeclaration, e.g. go-gardening-demo/initial-data/
// declarative_guardrails.json — fetches the gateway's current
// declarativeGuardrails config via _admin.export_declarative_guardrails,
// merges in the new declarations (configmerge.MergeDeclarativeGuardrails:
// replace by name, union the actions catalog — also used offline by
// go-gardening-demo/patchcfg), and writes the result back via _admin.
// update_declarative_guardrails. Both admin tools require the caller's
// role to be exactly "admin", same as update_access_rules.
func runUpdateDeclarativeGuardrails(ctx context.Context, session *mcp.ClientSession, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("admin update_declarative_guardrails: failed to read %s: %w", path, err)
	}
	var newDeclarations []guardrailcfg.RawDeclaration
	if err := json.Unmarshal(raw, &newDeclarations); err != nil {
		return fmt.Errorf("admin update_declarative_guardrails: failed to parse %s as a JSON array of declarations: %w", path, err)
	}

	current, err := exportDeclarativeGuardrails(ctx, session)
	if err != nil {
		return fmt.Errorf("admin update_declarative_guardrails: %w", err)
	}

	mergedActions, mergedDeclarations := configmerge.MergeDeclarativeGuardrails(current.Config, newDeclarations)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "_admin.update_declarative_guardrails",
		Arguments: map[string]any{
			"version": current.Version,
			"config":  map[string]any{"actions": mergedActions, "declarations": mergedDeclarations},
		},
	})
	if err != nil {
		return fmt.Errorf("admin update_declarative_guardrails: _admin.update_declarative_guardrails call failed: %w", err)
	}
	if res.IsError {
		return fmt.Errorf("admin update_declarative_guardrails: _admin.update_declarative_guardrails rejected the call: %s", toolErrorText(res.Content))
	}
	printContent(res.Content)
	fmt.Printf("%d declaration(s) applied from %s.\n", len(newDeclarations), path)
	return nil
}

// exportDeclarativeGuardrails calls _admin.export_declarative_guardrails
// and parses its result, the same round-trip-through-JSON way
// exportAccessRules does.
func exportDeclarativeGuardrails(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[guardrailcfg.RawConfig], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_declarative_guardrails", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[guardrailcfg.RawConfig]{}, fmt.Errorf("_admin.export_declarative_guardrails call failed: %w", err)
	}
	if res.IsError {
		return configsync.VersionedConfig[guardrailcfg.RawConfig]{}, fmt.Errorf("_admin.export_declarative_guardrails returned an error: %s", toolErrorText(res.Content))
	}
	rawJSON, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[guardrailcfg.RawConfig]{}, fmt.Errorf("failed to re-marshal structuredContent: %w", err)
	}
	var out configsync.VersionedConfig[guardrailcfg.RawConfig]
	if err := json.Unmarshal(rawJSON, &out); err != nil {
		return configsync.VersionedConfig[guardrailcfg.RawConfig]{}, fmt.Errorf("failed to parse export result: %w", err)
	}
	return out, nil
}

// runUpdateOrchestration reads path — a triggered_tools_runtime.
// OrchestrationConfig-shaped JSON fragment (a subset of orchestrations/
// toolMappings to add or replace), e.g. go-gardening-demo/initial-data/
// orchestration.json — fetches the gateway's current governanceOrchestration
// config via _admin.export_orchestration, merges it in
// (configmerge.MergeOrchestration: upsert orchestrations by name, replace
// toolMappings by pattern and prepend so a specific pattern reliably
// out-ranks a pre-existing catch-all "*" mapping - toolMappings is
// evaluated first-match-wins, see CLAUDE.md's "Triggered tools /
// orchestration" — also used offline by go-gardening-demo/patchcfg), and
// writes the result back via _admin.update_orchestration.
func runUpdateOrchestration(ctx context.Context, session *mcp.ClientSession, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("admin update_orchestration: failed to read %s: %w", path, err)
	}
	var newConfig triggered_tools_runtime.OrchestrationConfig
	if err := json.Unmarshal(raw, &newConfig); err != nil {
		return fmt.Errorf("admin update_orchestration: failed to parse %s as an orchestration config fragment: %w", path, err)
	}

	current, err := exportOrchestration(ctx, session)
	if err != nil {
		return fmt.Errorf("admin update_orchestration: %w", err)
	}

	merged := configmerge.MergeOrchestration(current.Config, newConfig)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "_admin.update_orchestration",
		Arguments: map[string]any{
			"version": current.Version,
			"config":  merged,
		},
	})
	if err != nil {
		return fmt.Errorf("admin update_orchestration: _admin.update_orchestration call failed: %w", err)
	}
	if res.IsError {
		return fmt.Errorf("admin update_orchestration: _admin.update_orchestration rejected the call: %s", toolErrorText(res.Content))
	}
	printContent(res.Content)
	fmt.Printf("%d orchestration(s) and %d toolMapping(s) applied from %s.\n", len(newConfig.Orchestrations), len(newConfig.ToolMappings), path)
	return nil
}

// exportOrchestration calls _admin.export_orchestration and parses its
// result, the same round-trip-through-JSON way exportAccessRules does.
func exportOrchestration(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_orchestration", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]{}, fmt.Errorf("_admin.export_orchestration call failed: %w", err)
	}
	if res.IsError {
		return configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]{}, fmt.Errorf("_admin.export_orchestration returned an error: %s", toolErrorText(res.Content))
	}
	rawJSON, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]{}, fmt.Errorf("failed to re-marshal structuredContent: %w", err)
	}
	var out configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]
	if err := json.Unmarshal(rawJSON, &out); err != nil {
		return configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]{}, fmt.Errorf("failed to parse export result: %w", err)
	}
	return out, nil
}

// promptsFileEntry is one entry in a prompts.json file (e.g.
// go-gardening-demo/initial-data/prompts.json) - Server names which
// garden-* server's own load_prompts tool this entry is routed to; the
// embedded promptstore.Prompt supplies name/description/text, forwarded
// verbatim (Server stripped, since by the time a specific server's
// load_prompts tool receives it, which server it's for is already implicit).
type promptsFileEntry struct {
	Server string `json:"server"`
	promptstore.Prompt
}

// runLoadPrompts reads path — a JSON array of promptsFileEntry, e.g.
// go-gardening-demo/initial-data/prompts.json — groups entries by their
// Server field (preserving each server's first-seen order, and the order
// servers themselves are first seen in, purely so output is deterministic
// rather than a Go map's randomized iteration), and calls that server's own
// <server>.load_prompts tool once with its own entries. Unlike every other
// admin-section tool, this forwards directly to garden-* backends rather
// than going through a proxy _admin.* tool — see runAdminCommand's own doc
// comment for why. Each server's load_prompts call fully replaces that
// server's prompt set (see promptstore.Store.Load), so a repeat run with a
// trimmed-down file also removes prompts no longer listed for a given
// server, not just upserts the ones still present.
func runLoadPrompts(ctx context.Context, session *mcp.ClientSession, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("admin load_prompts: failed to read %s: %w", path, err)
	}
	var entries []promptsFileEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("admin load_prompts: failed to parse %s as a JSON array of prompts: %w", path, err)
	}

	servers, byServer, err := groupPromptsByServer(entries)
	if err != nil {
		return fmt.Errorf("admin load_prompts: %s: %w", path, err)
	}

	total := 0
	for _, server := range servers {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      server + ".load_prompts",
			Arguments: map[string]any{"prompts": byServer[server]},
		})
		if err != nil {
			return fmt.Errorf("admin load_prompts: %s.load_prompts call failed: %w", server, err)
		}
		if res.IsError {
			return fmt.Errorf("admin load_prompts: %s.load_prompts rejected the call: %v", server, res.Content)
		}
		printContent(res.Content)
		total += len(byServer[server])
	}
	fmt.Printf("%d prompt(s) applied from %s across %d server(s).\n", total, path, len(servers))
	return nil
}

// groupPromptsByServer splits entries by their Server field, returning the
// distinct server names in first-seen order (for deterministic dispatch/
// output) alongside each server's own []promptstore.Prompt (Server
// stripped). An entry with an empty Server is a config-authoring error.
func groupPromptsByServer(entries []promptsFileEntry) (servers []string, byServer map[string][]promptstore.Prompt, err error) {
	byServer = make(map[string][]promptstore.Prompt, len(entries))
	for _, e := range entries {
		if e.Server == "" {
			return nil, nil, fmt.Errorf("entry %q has no \"server\"", e.Name)
		}
		if _, ok := byServer[e.Server]; !ok {
			servers = append(servers, e.Server)
		}
		byServer[e.Server] = append(byServer[e.Server], e.Prompt)
	}
	return servers, byServer, nil
}

// toolErrorText is the text of an error tool result's content, for an
// error message (formatting the content slice itself with %v prints
// pointers).
func toolErrorText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		} else {
			parts = append(parts, fmt.Sprintf("<%T>", c))
		}
	}
	return strings.Join(parts, " ")
}
