// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package promptstore

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStoreLoadRegistersAndServesPrompts(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	n := store.Load([]Prompt{
		{Name: "p1", Description: "d1", Text: "t1"},
		{Name: "p2", Description: "d2", Text: "t2"},
	})
	if n != 2 {
		t.Fatalf("expected Load to return 2, got %d", n)
	}

	res, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1"}})
	if err != nil {
		t.Fatalf("handle(p1): %v", err)
	}
	if res.Description != "d1" {
		t.Errorf("expected Description %q, got %q", "d1", res.Description)
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
		t.Fatalf("expected exactly 1 user message, got %+v", res.Messages)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "t1" {
		t.Errorf("expected TextContent %q, got %+v", "t1", res.Messages[0].Content)
	}
}

// TestStoreLoadReplacesAndRemoves covers the full-replace convention: a
// second Load call drops any prompt not present in its own list.
func TestStoreLoadReplacesAndRemoves(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{
		{Name: "p1", Description: "d1", Text: "t1"},
		{Name: "p2", Description: "d2", Text: "t2"},
	})
	n := store.Load([]Prompt{
		{Name: "p2", Description: "d2-updated", Text: "t2-updated"},
	})
	if n != 1 {
		t.Fatalf("expected Load to return 1, got %d", n)
	}

	if _, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1"}}); err == nil {
		t.Error("expected p1 to be removed after a Load call that no longer names it")
	}

	res, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p2"}})
	if err != nil {
		t.Fatalf("handle(p2): %v", err)
	}
	if res.Description != "d2-updated" {
		t.Errorf("expected p2's description to be updated, got %q", res.Description)
	}
}

func TestStoreHandleSubstitutesArguments(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{
		{
			Name:        "p1",
			Description: "d1",
			Text:        "Can I find a {{.product}} on sale?",
			Arguments:   []PromptArgument{{Name: "product", Required: true}},
		},
	})

	res, err := store.handle(context.Background(), &mcp.GetPromptRequest{
		Params: &mcp.GetPromptParams{Name: "p1", Arguments: map[string]string{"product": "rake"}},
	})
	if err != nil {
		t.Fatalf("handle(p1): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "Can I find a rake on sale?" {
		t.Errorf("expected substituted text, got %+v", res.Messages[0].Content)
	}
}

func TestStoreHandleMissingRequiredArgument(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{
		{
			Name:      "p1",
			Text:      "Can I find a {{.product}} on sale?",
			Arguments: []PromptArgument{{Name: "product", Required: true}},
		},
	})

	if _, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1"}}); err == nil {
		t.Error("expected an error when a required argument is missing")
	}
}

func TestStoreHandleOptionalArgumentEmptyWhenAbsent(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{
		{
			Name:      "p1",
			Text:      "Can I find a {{.product}} on sale?",
			Arguments: []PromptArgument{{Name: "product", Required: false}},
		},
	})

	res, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1"}})
	if err != nil {
		t.Fatalf("handle(p1): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "Can I find a  on sale?" {
		t.Errorf("expected the missing optional argument to render as empty, got %+v", res.Messages[0].Content)
	}
}

// TestStoreHandleConditionalOnArgumentPresence covers the {{if .name}}
// branching this prompt system exists for: an optional argument's mere
// presence (any non-empty value - a missing key and an empty string both
// evaluate false) selects which sentence is rendered.
func TestStoreHandleConditionalOnArgumentPresence(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{
		{
			Name: "p1",
			Text: "base{{if .flag}} with flag={{.flag}}{{end}}",
			Arguments: []PromptArgument{
				{Name: "flag", Required: false},
			},
		},
	})

	cases := []struct {
		args map[string]string
		want string
	}{
		{args: nil, want: "base"},
		{args: map[string]string{}, want: "base"},
		{args: map[string]string{"flag": "yes"}, want: "base with flag=yes"},
	}
	for _, c := range cases {
		res, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1", Arguments: c.args}})
		if err != nil {
			t.Fatalf("handle(p1, %v): %v", c.args, err)
		}
		text, ok := res.Messages[0].Content.(*mcp.TextContent)
		if !ok || text.Text != c.want {
			t.Errorf("args %v: expected %q, got %+v", c.args, c.want, res.Messages[0].Content)
		}
	}
}

func TestStoreHandleInjectsToday(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	store.Load([]Prompt{{Name: "p1", Text: "today is {{.today}}"}})

	res, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "p1"}})
	if err != nil {
		t.Fatalf("handle(p1): %v", err)
	}
	want := "today is " + time.Now().Format("2006-01-02")
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != want {
		t.Errorf("expected %q, got %+v", want, res.Messages[0].Content)
	}
}

func TestStoreHandleUnknownPrompt(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	store := New(server)

	if _, err := store.handle(context.Background(), &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "nonexistent"}}); err == nil {
		t.Error("expected an error for a prompt that was never loaded")
	}
}
