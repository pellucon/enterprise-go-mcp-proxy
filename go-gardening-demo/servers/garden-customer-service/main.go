// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// garden-customer-service is a go-gardening-demo MCP backend server: it
// holds customer service call records in memory. Its modify_customer_sentiment
// tool relays back through the proxy to garden-customer-admin.modify_customer_sentiment
// — the same backend-calls-back-into-the-proxy mechanism demonstrated by
// sample-mcp-servers/misc-demo-server's get_my_account_balance (see
// CLAUDE.md's "Internal identity assertions" section), reused
// unchanged here rather than reimplemented.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const dateLayout = "2006-01-02"

type customerService struct {
	mu         sync.Mutex
	sentiments map[string]entities.CustomerSentiment // local reference only
	calls      map[string]entities.CustomerServiceCall
}

func newCustomerService() *customerService {
	return &customerService{
		sentiments: make(map[string]entities.CustomerSentiment),
		calls:      make(map[string]entities.CustomerServiceCall),
	}
}

// nextCallID finds the lowest-numbered unused "csc-NN" id (matching the
// seed data's own 2-digit format, e.g. "csc-01".."csc-30") — a plain scan
// from 1, not from len(calls)+1: with a gap in the loaded ids (there
// isn't one in the seed data, but a live-mutated server shouldn't assume
// that stays true), starting past the count could skip right over a free
// id and never fill it. Cheap regardless at this dataset's scale.
func (cs *customerService) nextCallID() string {
	for n := 1; ; n++ {
		id := fmt.Sprintf("csc-%02d", n)
		if _, exists := cs.calls[id]; !exists {
			return id
		}
	}
}

type LoadResult struct {
	Loaded int `json:"loaded"`
}

type LoadCustomerSentimentsArgs struct {
	Sentiments []entities.CustomerSentiment `json:"sentiments" jsonschema:"The full customer_sentiments.json list to load."`
}

// LoadPromptsArgs is load_prompts' own bulk-load shape - see
// go-gardening-demo/promptstore's own doc comment for why this
// server's whole prompt set lives in data (prompts.json) rather than
// hand-written Go prompt functions.
type LoadPromptsArgs struct {
	Prompts []promptstore.Prompt `json:"prompts" jsonschema:"This server's entries from prompts.json (server field stripped - already implied by which server's load_prompts you're calling)."`
}

// loadCustomerSentiments is a local-only reference load.
func (cs *customerService) loadCustomerSentiments(ctx context.Context, req *mcp.CallToolRequest, args LoadCustomerSentimentsArgs) (*mcp.CallToolResult, LoadResult, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range args.Sentiments {
		cs.sentiments[s.ID] = s
	}
	return nil, LoadResult{Loaded: len(args.Sentiments)}, nil
}

type LoadCustomerServiceCallsArgs struct {
	Calls []entities.CustomerServiceCall `json:"calls" jsonschema:"The full customer_service_calls.json list to load."`
}

func (cs *customerService) loadCustomerServiceCalls(ctx context.Context, req *mcp.CallToolRequest, args LoadCustomerServiceCallsArgs) (*mcp.CallToolResult, LoadResult, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range args.Calls {
		cs.calls[c.ID] = c
	}
	return nil, LoadResult{Loaded: len(args.Calls)}, nil
}

type AddCustomerServiceCallArgs struct {
	CustomerID           string `json:"customerId"`
	HandledByStafferID   string `json:"handledByStafferId"`
	CallDate             string `json:"callDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
	IssueText            string `json:"issueText"`
	EscalatedToManager   bool   `json:"escalatedToManager,omitempty"`
	EscalatedToStafferID string `json:"escalatedToStafferId,omitempty"`
}

func (cs *customerService) addCustomerServiceCall(ctx context.Context, req *mcp.CallToolRequest, args AddCustomerServiceCallArgs) (*mcp.CallToolResult, entities.CustomerServiceCall, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	callDate := args.CallDate
	if callDate == "" {
		callDate = time.Now().Format(dateLayout)
	}

	call := entities.CustomerServiceCall{
		ID:                 cs.nextCallID(),
		CustomerID:         args.CustomerID,
		HandledByStafferID: args.HandledByStafferID,
		CallDate:           callDate,
		IssueText:          args.IssueText,
		EscalatedToManager: args.EscalatedToManager,
	}
	if args.EscalatedToStafferID != "" {
		esid := args.EscalatedToStafferID
		call.EscalatedToStafferID = &esid
	}
	cs.calls[call.ID] = call
	return nil, call, nil
}

type ResolveCustomerServiceCallArgs struct {
	ID             string `json:"id"`
	ResolutionDate string `json:"resolutionDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
}

func (cs *customerService) resolveCustomerServiceCall(ctx context.Context, req *mcp.CallToolRequest, args ResolveCustomerServiceCallArgs) (*mcp.CallToolResult, entities.CustomerServiceCall, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	call, ok := cs.calls[args.ID]
	if !ok {
		return nil, entities.CustomerServiceCall{}, fmt.Errorf("resolve_customer_service_call: unknown call %q", args.ID)
	}
	date := args.ResolutionDate
	if date == "" {
		date = time.Now().Format(dateLayout)
	}
	call.ResolutionDate = &date
	cs.calls[call.ID] = call
	return nil, call, nil
}

type GetCustomerServiceCallsArgs struct {
	CallDate           string `json:"callDate,omitempty" jsonschema:"Exact match (YYYY-MM-DD)."`
	Resolved           *bool  `json:"resolved,omitempty" jsonschema:"true = has a resolutionDate, false = still unresolved. Omit for no filter."`
	IssueText          string `json:"issueText,omitempty" jsonschema:"Case-insensitive substring match against issueText. No wildcards."`
	Escalated          *bool  `json:"escalated,omitempty" jsonschema:"Filter on escalatedToManager. Omit for no filter."`
	HandledByStafferID string `json:"handledByStafferId,omitempty" jsonschema:"Exact match against handledByStafferId. Omit for no filter."`
}

// GetCustomerServiceCallsResult wraps get_customer_service_calls' list in
// a named field rather than returning a bare array: some MCP clients
// (e.g. Claude Desktop) require every tool's inferred outputSchema to be
// a JSON object at the top level, and a bare []entities.CustomerServiceCall
// Out infers as an array instead.
type GetCustomerServiceCallsResult struct {
	Calls []entities.CustomerServiceCall `json:"calls"`
}

// getCustomerServiceCalls ANDs together whichever filters are non-empty;
// no filters at all returns every call - same convention as
// garden-staff-admin.getStaffers.
func (cs *customerService) getCustomerServiceCalls(ctx context.Context, req *mcp.CallToolRequest, args GetCustomerServiceCallsArgs) (*mcp.CallToolResult, GetCustomerServiceCallsResult, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	issueQuery := strings.ToLower(args.IssueText)
	out := make([]entities.CustomerServiceCall, 0, len(cs.calls))
	for _, c := range cs.calls {
		if args.CallDate != "" && c.CallDate != args.CallDate {
			continue
		}
		if args.Resolved != nil && (c.ResolutionDate != nil) != *args.Resolved {
			continue
		}
		if issueQuery != "" && !strings.Contains(strings.ToLower(c.IssueText), issueQuery) {
			continue
		}
		if args.Escalated != nil && c.EscalatedToManager != *args.Escalated {
			continue
		}
		if args.HandledByStafferID != "" && c.HandledByStafferID != args.HandledByStafferID {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetCustomerServiceCallsResult{Calls: out}, nil
}

type ModifyCustomerSentimentArgs struct {
	CustomerID string `json:"customerId"`
	Sentiment  string `json:"sentiment"`
}

// modifyCustomerSentiment relays to garden-customer-admin.modify_customer_sentiment
// — see this file's doc comment. No local state is touched here.
func modifyCustomerSentiment(ctx context.Context, req *mcp.CallToolRequest, args ModifyCustomerSentimentArgs) (*mcp.CallToolResult, any, error) {
	incoming := relayheaders.Extract(req.GetExtra().Header)
	if incoming.Assertion == "" || incoming.CallbackURL == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "missing identity assertion or callback URL — is internalAuth and selfUrl configured on the proxy?"}},
		}, nil, nil
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "garden-customer-service", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   incoming.CallbackURL,
		HTTPClient: incoming.Client(nil),
	}, nil)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to connect back to proxy at %s: %v", incoming.CallbackURL, err)}},
		}, nil, nil
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "garden-customer-admin.modify_customer_sentiment",
		Arguments: map[string]any{"id": args.CustomerID, "sentiment": args.Sentiment},
	})
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("relayed call failed: %v", err)}},
		}, nil, nil
	}
	return &mcp.CallToolResult{IsError: result.IsError, Content: result.Content, StructuredContent: result.StructuredContent}, nil, nil
}

func createServer(cs *customerService) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garden-customer-service", Version: "1.0.0"}, nil)
	prompts := promptstore.New(server)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_customer_sentiments",
		Description: "Bulk-load the customer sentiment reference list (customer_sentiments.json's shape). Local reference only.",
	}, cs.loadCustomerSentiments)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_customer_service_calls",
		Description: "Bulk-load customer service call records (customer_service_calls.json's shape).",
	}, cs.loadCustomerServiceCalls)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_customer_service_call",
		Description: "Record a new customer service call.",
	}, cs.addCustomerServiceCall)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "resolve_customer_service_call",
		Description: "Mark a customer service call resolved.",
	}, cs.resolveCustomerServiceCall)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "modify_customer_sentiment",
		Description: "Update a customer's sentiment by relaying back through the proxy to garden-customer-admin.",
	}, modifyCustomerSentiment)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customer_service_calls",
		Description: "List customer service calls, optionally filtered by callDate (exact), resolved (resolutionDate presence), issueText (substring), escalated (escalatedToManager), and handledByStafferId (exact). All given filters are ANDed.",
	}, cs.getCustomerServiceCalls)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_prompts",
		Description: "Bulk-load this server's MCP prompts (prompts.json's shape, filtered to this server's entries). Replaces the current set - a prompt loaded before but not named this time is removed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args LoadPromptsArgs) (*mcp.CallToolResult, LoadResult, error) {
		return nil, LoadResult{Loaded: prompts.Load(args.Prompts)}, nil
	})

	return server
}

func main() {
	var port, logLevel string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this server has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	cs := newCustomerService()
	server := createServer(cs)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting garden-customer-service on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
