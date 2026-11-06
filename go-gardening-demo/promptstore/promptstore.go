// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package promptstore is a small, shared (importable, non-main) helper any
// go-gardening-demo server can use to expose data-driven MCP prompts
// instead of hand-written Go prompt functions: a server registers a
// load_prompts tool backed by a *Store, and from then on its whole set of
// prompts is whatever was last loaded - see go-gardening-demo/initial-data/
// prompts.json and cli/admin.go's "admin load_prompts" command, which reads
// that file, groups its entries by server, and calls each named server's
// own load_prompts tool with its slice.
package promptstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PromptArgument names one caller-suppliable value a Prompt's Text may
// reference as "{{.Name}}" (or branch on via "{{if .Name}}...{{end}}") -
// standard Go text/template syntax against the caller's own Arguments map.
// Mirrors mcp.PromptArgument's own field shape (minus Title, unused by
// this demo).
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one server's prompt definition - Name/Description/Arguments
// double as the registered mcp.Prompt's own fields. Text is a Go
// text/template string, executed on request (see Store.handle) against
// the caller's Arguments (a map[string]string, per the MCP spec - prompt
// arguments are always strings) plus a server-injected "today" (see
// Store.handle): a bare question with no template actions at all executes
// to itself unchanged, and referencing an argument that's absent (map-key
// miss) evaluates to "" rather than erroring, which is what makes
// "{{if .foo}}...{{end}}" a valid presence check for an optional
// argument. Arguments[i].Required is enforced separately, before
// execution (see Store.handle) - text/template's own map-miss behavior
// can't distinguish "absent" from "explicitly empty", so it can't carry
// that enforcement on its own.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Text        string           `json:"text"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// Store owns one *mcp.Server's whole prompt set, replaced wholesale on
// every Load call - the same full-replace convention every other load_*
// tool in this demo already uses for its own in-memory map.
type Store struct {
	mu      sync.Mutex
	server  *mcp.Server
	prompts map[string]Prompt
}

// New returns a Store bound to server. Register load_prompts (via
// mcp.AddTool, calling Load) separately - Store itself never touches
// tools, only prompts.
func New(server *mcp.Server) *Store {
	return &Store{server: server, prompts: make(map[string]Prompt)}
}

// Load replaces the store's entire prompt set with prompts: every entry is
// (re-)registered on the underlying *mcp.Server via AddPrompt (which
// itself fires "notifications/prompts/list_changed"), and any prompt
// present before this call but absent from prompts is removed via
// RemovePrompts. Returns the new count.
func (s *Store) Load(prompts []Prompt) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]Prompt, len(prompts))
	for _, p := range prompts {
		next[p.Name] = p
		s.server.AddPrompt(&mcp.Prompt{Name: p.Name, Description: p.Description, Arguments: mcpArguments(p.Arguments)}, s.handle)
	}
	for name := range s.prompts {
		if _, ok := next[name]; !ok {
			s.server.RemovePrompts(name)
		}
	}
	s.prompts = next
	return len(next)
}

// mcpArguments converts this package's own PromptArgument into the
// go-sdk's mcp.PromptArgument - nil in, nil out, so a Prompt with no
// Arguments registers with none rather than an empty non-nil slice.
func mcpArguments(args []PromptArgument) []*mcp.PromptArgument {
	if args == nil {
		return nil
	}
	out := make([]*mcp.PromptArgument, len(args))
	for i, a := range args {
		out[i] = &mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required}
	}
	return out
}

// handle is the one PromptHandler backing every prompt Load registers -
// req.Params.Name says which prompt was actually requested (the go-sdk's
// own Server.getPrompt already looks it up by name before dispatching
// here, so a lookup miss below is defensive, not expected in practice).
// Every declared Arguments[i].Required is checked for presence first (see
// Prompt's own doc comment for why text/template can't do this on its
// own); Text is then executed as a template against the caller's raw
// Arguments map.
func (s *Store) handle(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	s.mu.Lock()
	p, ok := s.prompts[req.Params.Name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown prompt %q", req.Params.Name)
	}

	for _, arg := range p.Arguments {
		if arg.Required {
			if _, given := req.Params.Arguments[arg.Name]; !given {
				return nil, fmt.Errorf("prompt %q: missing required argument %q", p.Name, arg.Name)
			}
		}
	}

	// missingkey=zero: an argument absent from the caller's map renders as
	// "" (the map's zero value) rather than the default diagnostic
	// "<no value>" - the same behavior a required-argument check above
	// would otherwise have to special-case for every optional argument.
	tmpl, err := template.New(p.Name).Option("missingkey=zero").Parse(p.Text)
	if err != nil {
		return nil, fmt.Errorf("prompt %q: invalid template: %w", p.Name, err)
	}

	// data is the caller's own Arguments plus a server-computed "today" -
	// an LLM asked to reason about e.g. "more than N days ago" is far more
	// reliable given a concrete reference date than left to infer the
	// current date itself. Copied rather than mutating the caller's map
	// directly; set after the copy so a caller can't override it with an
	// argument literally named "today".
	data := make(map[string]string, len(req.Params.Arguments)+1)
	for k, v := range req.Params.Arguments {
		data[k] = v
	}
	data["today"] = time.Now().Format("2006-01-02")

	var text strings.Builder
	if err := tmpl.Execute(&text, data); err != nil {
		return nil, fmt.Errorf("prompt %q: failed to render: %w", p.Name, err)
	}

	return &mcp.GetPromptResult{
		Description: p.Description,
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: text.String()}},
		},
	}, nil
}
