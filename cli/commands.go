// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runListTools(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("list-tools", flag.ContinueOnError)
	var schema bool
	fs.BoolVar(&schema, "schema", false, "Print each tool's full JSON definition (including inputSchema) instead of just name/description")
	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return err
	}
	for _, t := range result.Tools {
		if schema {
			raw, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(raw))
			continue
		}
		fmt.Printf("%s\t%s\n", t.Name, t.Description)
	}
	return nil
}

func runListPrompts(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("list-prompts", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := session.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return err
	}
	for _, p := range result.Prompts {
		fmt.Printf("%s\t%s\n", p.Name, p.Description)
	}
	return nil
}

func runListResources(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("list-resources", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := session.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		return err
	}
	for _, r := range result.Resources {
		fmt.Printf("%s\t%s\n", r.URI, r.Description)
	}
	return nil
}

func runCallTool(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("call-tool", flag.ContinueOnError)
	var name, rawArgs string
	fs.StringVar(&name, "name", "", "Namespaced tool name, e.g. event-aggregator.list_events (required)")
	fs.StringVar(&rawArgs, "args", "{}", "JSON object of tool arguments")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if name == "" {
		return fmt.Errorf("call-tool: -name is required")
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &arguments); err != nil {
		return fmt.Errorf("invalid -args JSON: %w", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	printContent(result.Content)
	printStructuredContent(result.StructuredContent)
	if result.IsError {
		return fmt.Errorf("call-tool: %s returned an error (see output above)", name)
	}
	return nil
}

// runGetPrompt's -name is illustrated with a hypothetical
// "backend-name.prompt-name" in topLevelUsage rather than a real one: no
// backend currently in this repo (platform-mcp-servers/, ai-mcp-servers/,
// go-gardening-demo/servers/) registers any prompts — this command still
// works against any MCP server that does.
func runGetPrompt(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("get-prompt", flag.ContinueOnError)
	var name, rawArgs string
	fs.StringVar(&name, "name", "", "Namespaced prompt name, e.g. backend-name.prompt-name (required)")
	fs.StringVar(&rawArgs, "args", "{}", "JSON object of string prompt arguments")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if name == "" {
		return fmt.Errorf("get-prompt: -name is required")
	}
	var arguments map[string]string
	if err := json.Unmarshal([]byte(rawArgs), &arguments); err != nil {
		return fmt.Errorf("invalid -args JSON: %w", err)
	}

	result, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	for _, m := range result.Messages {
		fmt.Printf("[%s] ", m.Role)
		printContent([]mcp.Content{m.Content})
	}
	return nil
}

// runReadResource's -uri is illustrated with a hypothetical
// "backend-name:file:///path" in topLevelUsage rather than a real one, for
// the same reason as runGetPrompt above: no backend currently in this repo
// registers any resources.
func runReadResource(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("read-resource", flag.ContinueOnError)
	var uri string
	fs.StringVar(&uri, "uri", "", "Namespaced resource URI, e.g. backend-name:file:///path (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if uri == "" {
		return fmt.Errorf("read-resource: -uri is required")
	}

	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return err
	}
	for _, c := range result.Contents {
		if c.Text != "" {
			fmt.Println(c.Text)
		} else {
			fmt.Printf("<%d bytes of binary content, mime type %s>\n", len(c.Blob), c.MIMEType)
		}
	}
	return nil
}

// runExportSection and runUpdateSection implement every export-<section>/
// update-<section> command pair (role-permissions, orchestration,
// discovery, event-dictionary, declarative-guardrails) — all 5 exchange
// the identical configsync.VersionedConfig[T] envelope shape
// (`{"version":N,"config":{...}}`), so one generic pair of implementations
// covers all 10 commands; only cmdName/toolName differ per call site (see
// the dispatch switch in repl.go). This mirrors admin_tools.go's own
// registerSectionAdminTools generic-over-explicit-call-sites shape.
// runExportSection also backs export-full-config, which has no matching
// update-* counterpart and takes no flags of its own.
func runExportSection(ctx context.Context, session *mcp.ClientSession, cmdName, toolName string) error {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: map[string]any{}})
	if err != nil {
		return err
	}
	printContent(result.Content)
	if result.IsError {
		return fmt.Errorf("%s: %s returned an error (see output above)", cmdName, toolName)
	}
	return nil
}

func runUpdateSection(ctx context.Context, session *mcp.ClientSession, cmdName, toolName string, args []string) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	var rawConfig, file string
	fs.StringVar(&rawConfig, "config", "", `JSON {"version":N,"config":{...}} envelope, e.g. one produced by the matching export-* command (mutually exclusive with -file)`)
	fs.StringVar(&file, "file", "", "Path to a JSON file holding that envelope (mutually exclusive with -config)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if (rawConfig == "") == (file == "") {
		return fmt.Errorf("%s: exactly one of -config or -file is required", cmdName)
	}

	raw := []byte(rawConfig)
	if file != "" {
		var err error
		raw, err = os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading -file %q: %w", file, err)
		}
	}

	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return fmt.Errorf("invalid config JSON: %w", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: arguments})
	if err != nil {
		return err
	}
	printContent(result.Content)
	if result.IsError {
		return fmt.Errorf("%s: %s returned an error (see output above)", cmdName, toolName)
	}
	return nil
}

func runFindRelevantTools(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("find-relevant-tools", flag.ContinueOnError)
	var task string
	fs.StringVar(&task, "task", "", "Description of the task to filter your visible tools against (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if task == "" {
		return fmt.Errorf("find-relevant-tools: -task is required")
	}

	// find_relevant_tools is go-proxy's own native "utility" tool (see
	// CLAUDE.md's "Utility tools" section): it builds the
	// caller's own already-authorized candidate list in-process, then
	// calls ai-powered-tools._filter_tool directly to do the LLM
	// filtering. This client just calls it with the task, same as calling
	// any other tool.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "proxy_utility.find_relevant_tools",
		Arguments: map[string]any{"task": task},
	})
	if err != nil {
		return err
	}
	if result.IsError {
		printContent(result.Content)
		return fmt.Errorf("find-relevant-tools: proxy_utility.find_relevant_tools returned an error (see output above)")
	}

	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return fmt.Errorf("expected text content from find_relevant_tools, got %T", result.Content[0])
	}
	var parsed struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal([]byte(textContent.Text), &parsed); err != nil {
		return fmt.Errorf("failed to parse find_relevant_tools result as a JSON tool list: %w (raw: %s)", err, textContent.Text)
	}
	for _, t := range parsed.Tools {
		fmt.Printf("%s\t%s\n", t.Name, t.Description)
	}
	return nil
}

// runListEvents drives event-aggregator's list_events tool in a loop,
// following each page's nextCursor until it comes back empty, so the
// caller doesn't have to manage pagination by hand — one command prints
// every matching event across both the in-memory buffer and the on-disk
// log tiers (see CLAUDE.md's "NATS event system" and the
// tiered-event-storage design). Every list_events filter field is exposed
// as its own flag, AND'd together exactly as list_events itself defines.
// -page-size sets how many events list_events returns per underlying
// call (its own "limit" argument) — not a cap on the total printed; the
// loop keeps paging until nextCursor is exhausted, so a small page size
// just means more, smaller round trips, not fewer results.
func runListEvents(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("list-events", flag.ContinueOnError)
	var typ, subtype, user, roles, tool, reason, toolCallID, since string
	var pageSize int
	fs.StringVar(&typ, "type", "", "Event type to filter by; empty means every type")
	fs.StringVar(&subtype, "subtype", "", "Event subtype to filter by; empty means every subtype")
	fs.StringVar(&user, "user", "", "Exact metadata.user to filter by")
	fs.StringVar(&roles, "roles", "", "Comma-separated metadata.roles to filter by; matches if the event has at least one of these roles")
	fs.StringVar(&tool, "tool", "", "Wildcard pattern ('*' matches any characters) to match against metadata.tool")
	fs.StringVar(&reason, "reason", "", "Wildcard pattern ('*' matches any characters) to match against metadata.reason")
	fs.StringVar(&toolCallID, "tool-call-id", "", "Exact metadata.toolCallId to filter by")
	fs.StringVar(&since, "since", "", "RFC3339 lower time bound, e.g. 2026-01-01T00:00:00Z; resolved once, before the first page")
	fs.IntVar(&pageSize, "page-size", 50, `Events per underlying list_events call (its own "limit" argument; default 50, max 500) — not a cap on the total printed, this command pages until every match has been printed`)
	if err := fs.Parse(args); err != nil {
		return err
	}

	filterArgs := map[string]any{}
	if typ != "" {
		filterArgs["type"] = typ
	}
	if subtype != "" {
		filterArgs["subtype"] = subtype
	}
	if user != "" {
		filterArgs["user"] = user
	}
	if roles != "" {
		filterArgs["roles"] = strings.Split(roles, ",")
	}
	if tool != "" {
		filterArgs["tool"] = tool
	}
	if reason != "" {
		filterArgs["reason"] = reason
	}
	if toolCallID != "" {
		filterArgs["toolCallId"] = toolCallID
	}
	if since != "" {
		filterArgs["since"] = since
	}
	if pageSize > 0 {
		filterArgs["limit"] = pageSize
	}

	var page struct {
		Events     []json.RawMessage `json:"events"`
		NextCursor string            `json:"nextCursor"`
	}

	total := 0
	cursor := ""
	for {
		arguments := make(map[string]any, len(filterArgs)+1)
		for k, v := range filterArgs {
			arguments[k] = v
		}
		if cursor != "" {
			arguments["cursor"] = cursor
		}

		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "event-aggregator.list_events", Arguments: arguments})
		if err != nil {
			return err
		}
		if result.IsError {
			printContent(result.Content)
			return fmt.Errorf("list-events: event-aggregator.list_events returned an error (see output above)")
		}
		textContent, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			return fmt.Errorf("expected text content from list_events, got %T", result.Content[0])
		}
		page.Events, page.NextCursor = nil, ""
		if err := json.Unmarshal([]byte(textContent.Text), &page); err != nil {
			return fmt.Errorf("failed to parse list_events result as JSON: %w (raw: %s)", err, textContent.Text)
		}

		for _, e := range page.Events {
			pretty, err := json.MarshalIndent(e, "", "  ")
			if err != nil {
				fmt.Println(string(e))
				continue
			}
			fmt.Println(string(pretty))
		}
		total += len(page.Events)

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	fmt.Fprintf(os.Stderr, "Total: %d event(s)\n", total)
	return nil
}

// runLoadEvents reads a local JSON file holding an array of events (e.g.
// go-gardening-demo/initial-data/sample-events.json, generated by
// go-gardening-demo/gendata and normally loaded via go-gardening-demo/cli's
// own "events load_events" - see that package's init.go/tools.go - not this
// generic command) and sends it as event-aggregator.load_events' "events"
// argument. The file is read and parsed here, client-side — event-aggregator
// itself never reads a file for this tool, unlike the unrelated
// -events-dictionary server-side-file convention.
func runLoadEvents(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("load-events", flag.ContinueOnError)
	var file string
	fs.StringVar(&file, "file", "", "Path to a JSON file holding an array of events to load, replacing event-aggregator's current buffer (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("load-events: -file is required")
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading -file %q: %w", file, err)
	}
	var parsedEvents []map[string]any
	if err := json.Unmarshal(raw, &parsedEvents); err != nil {
		return fmt.Errorf("invalid events JSON in %q: %w", file, err)
	}
	fmt.Printf("Loading %d events from %s\n", len(parsedEvents), file)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "event-aggregator.load_events",
		Arguments: map[string]any{"events": parsedEvents},
	})
	if err != nil {
		return err
	}
	printContent(result.Content)
	if result.IsError {
		return fmt.Errorf("load-events: event-aggregator.load_events returned an error (see output above)")
	}
	return nil
}
