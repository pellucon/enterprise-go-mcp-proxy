// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const PROXY_URL = "http://localhost:8081/mcp"
const MCP_SERVER1_PORT = ":8082"
const MCP_SERVER2_PORT = ":8083"

type SayHello struct {
	Language string `json:"language" jsonschema:"user language"`
	Count    int    `json:"count" jsonschema:"some count"`
}

type SayGoodbye struct {
	Greet string `json:"greet" jsonschema:"bye greet"`
	Idnum int    `json:"idnum" jsonschema:"id num"`
}

type GetCustomeName struct {
	Id string `json:"id" jsonschema:"customer id"`
}

type GetCompanyName struct {
	Id string `json:"id" jsonschema:"company id"`
}

var toolList1 []mcp.Tool
var toolList2 []mcp.Tool

func createServers() (*mcp.Server, *mcp.Server) {
	server1 := mcp.NewServer(&mcp.Implementation{Name: "mcp1", Version: "1.0.0"}, nil)
	server2 := mcp.NewServer(&mcp.Implementation{Name: "mcp2", Version: "1.0.0"}, nil)

	mcp.AddTool(server1, &mcp.Tool{
		Name:        "say_hello",
		Description: "Say Hello",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"description": "user language",
				},
				"count": map[string]any{
					"type":        "integer",
					"description": "some count",
				},
			},
			"required": []string{"language", "count"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SayHello) (*mcp.CallToolResult, any, error) {
		log.Println("say_hello called AT BACKEND")

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Hello there mate " + args.Language},
			},
		}, nil, nil
	})

	mcp.AddTool(server1, &mcp.Tool{
		Name:        "get_customer_name",
		Description: "Customer name from Id",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "customer id",
				},
			},
			"required": []string{"id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args GetCustomeName) (*mcp.CallToolResult, any, error) {

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Name_of_" + args.Id},
			},
		}, nil, nil
	})

	mcp.AddTool(server2, &mcp.Tool{
		Name:        "say_goodbye",
		Description: "Say Goddbye",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"greet": map[string]any{
					"type":        "string",
					"description": "bye greet",
				},
				"idnum": map[string]any{
					"type":        "integer",
					"description": "ID number",
				},
			},
			"required": []string{"greet", "count"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SayGoodbye) (*mcp.CallToolResult, any, error) {
		log.Println("say_goodbye called AT BACKEND")

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Hello there mate " + args.Greet},
			},
		}, nil, nil
	})

	mcp.AddTool(server2, &mcp.Tool{
		Name:        "get_company_name",
		Description: "Company name from Id",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "company id",
				},
			},
			"required": []string{"id"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args GetCompanyName) (*mcp.CallToolResult, any, error) {

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Company_" + args.Id},
			},
		}, nil, nil
	})

	server1.AddPrompt(&mcp.Prompt{
		Name:        "greet_customer",
		Description: "Greet a customer by name",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		log.Println("greet_customer prompt called AT BACKEND")
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Greetings, customer!"}},
			},
		}, nil
	})

	server2.AddPrompt(&mcp.Prompt{
		Name:        "farewell_customer",
		Description: "Say farewell to a customer",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		log.Println("farewell_customer prompt called AT BACKEND")
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Farewell, customer!"}},
			},
		}, nil
	})

	// dev_notes exists so a non-admin ("development.*") role has something
	// access_rules_test.json actually grants it, to exercise allow/deny
	// behavior for prompts (see the "development.engineer" checks below).
	server1.AddPrompt(&mcp.Prompt{
		Name:        "dev_notes",
		Description: "Internal dev notes",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		log.Println("dev_notes prompt called AT BACKEND")
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Dev notes: TODO fix bug"}},
			},
		}, nil
	})

	server1.AddResource(&mcp.Resource{
		URI:         "file:///customers.txt",
		Name:        "customers",
		Description: "Customer list",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		log.Println("customers resource read AT BACKEND")
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: "Customer list contents"},
			},
		}, nil
	})

	// dev-notes.txt is the resource counterpart to dev_notes above, matching
	// access_rules_test.json's "mcp1:file:///dev-*" grant for development.*.
	server1.AddResource(&mcp.Resource{
		URI:         "file:///dev-notes.txt",
		Name:        "dev-notes",
		Description: "Dev notes file",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		log.Println("dev-notes resource read AT BACKEND")
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: "Dev notes file contents"},
			},
		}, nil
	})

	server2.AddResource(&mcp.Resource{
		URI:         "file:///companies.txt",
		Name:        "companies",
		Description: "Company list",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		log.Println("companies resource read AT BACKEND")
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: "Company list contents"},
			},
		}, nil
	})

	server1.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "file:///{path}",
		Name:        "files",
		Description: "Arbitrary files",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		log.Println("file template read AT BACKEND: " + req.Params.URI)
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: "Contents of " + req.Params.URI},
			},
		}, nil
	})

	toolList2 = append(toolList2, mcp.Tool{
		Name:        "say_goodbye",
		Description: "Say Goddbye",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"greet": map[string]any{
					"type":        "string",
					"description": "bye greet",
				},
				"idnum": map[string]any{
					"type":        "integer",
					"description": "ID number",
				},
			},
			"required": []string{"greet", "count"},
		},
	},
		mcp.Tool{
			Name:        "get_company_name",
			Description: "Company name from Id",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "company id",
					},
				},
				"required": []string{"id"},
			},
		})
	toolList1 = append(toolList1, mcp.Tool{Name: "say_hello",
		Description: "Say Hello",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"description": "user language",
				},
				"count": map[string]any{
					"type":        "integer",
					"description": "some count",
				},
			},
			"required": []string{"language", "count"},
		}}, mcp.Tool{Name: "get_customer_name",
		Description: "Customer name from Id",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "customer id",
				},
			},
			"required": []string{"id"},
		}})
	return server1, server2
}

// writeIntegrationTestConfigWithNatsURL loads the checked-in,
// human-readable integration_test.cfg, injects natsUrl (only known at
// test-run time, once the embedded NATS server has picked a random port),
// and writes the result to a t.TempDir() copy — keeping integration_test.cfg
// itself the single canonical example rather than duplicating its content
// as an inline Go string. Mirrors platform_servers_test.go's
// writeOrchestrationTestConfigWithNatsURL.
func writeIntegrationTestConfigWithNatsURL(t *testing.T, natsURL string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/integration_test.cfg")
	if err != nil {
		t.Fatalf("failed to read integration_test.cfg: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse integration_test.cfg: %v", err)
	}
	raw["natsUrl"] = natsURL
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("failed to re-marshal integration_test.cfg with natsUrl: %v", err)
	}
	path := t.TempDir() + "/integration-test-with-nats.cfg"
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestProxyClient(t *testing.T) {
	server1, server2 := createServers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestBackend(t, ctx, cancel, MCP_SERVER1_PORT, server1)
	startTestBackend(t, ctx, cancel, MCP_SERVER2_PORT, server2)
	natsURL := startTestNATSServer(t)
	cfgPath := writeIntegrationTestConfigWithNatsURL(t, natsURL)
	startTestProxy(t, ctx, cancel, cfgPath, ":8081")

	tools, err := getTools()

	if err != nil {
		cancel()
		t.Errorf("Failed getting tools %v", err.Error())
	}
	// 4 backend tools + 17 admin tools (8 configsync-managed sections x
	// update/export, plus export_full_config - see admin_tools.go's
	// registerAdminTools) + 6 utility tools (proxy_utility.find_relevant_tools,
	// proxy_utility.call_tool, proxy_utility.list_triggered_tools,
	// proxy_utility.get_my_id_details, proxy_utility.get_prompts,
	// proxy_utility.render_prompt - see utility_tools.go's
	// registerUtilityTools).
	if len(tools) != 27 {
		cancel()
		t.Errorf("Should have found 27 tools, got %d: %v", len(tools), tools)
	}

	log.Printf("Success! Found these tools %v", tools)
	params := mcp.CallToolParams{
		Name: "mcp1.say_hello",
		Arguments: map[string]any{
			"language": "English",
			"count":    3,
		},
	}
	testToolCall(t, params, "Hello there mate English", cancel)

	params = mcp.CallToolParams{
		Name: "mcp2.say_goodbye",
		Arguments: map[string]any{
			"greet": "see-you",
			"count": 3,
		},
	}
	testToolCall(t, params, "Hello there mate see-you", cancel)

	params = mcp.CallToolParams{
		Name: "mcp1.get_customer_name",
		Arguments: map[string]any{
			"id": "id1",
		},
	}
	testToolCall(t, params, "Name_of_id1", cancel)

	params = mcp.CallToolParams{
		Name: "mcp2.get_company_name",
		Arguments: map[string]any{
			"id": "c1",
		},
	}
	testToolCall(t, params, "Company_c1", cancel)

	prompts, err := getPrompts()
	if err != nil {
		cancel()
		t.Errorf("Failed getting prompts %v", err.Error())
	}
	if len(prompts) != 3 {
		cancel()
		t.Errorf("Should have found 3 prompts, got %v", prompts)
	}

	log.Printf("Success! Found these prompts %v", prompts)

	testPromptGet(t, &mcp.GetPromptParams{Name: "mcp1.greet_customer"}, "Greetings, customer!", cancel)
	testPromptGet(t, &mcp.GetPromptParams{Name: "mcp2.farewell_customer"}, "Farewell, customer!", cancel)

	resources, err := getResources()
	if err != nil {
		cancel()
		t.Errorf("Failed getting resources %v", err.Error())
	}
	if len(resources) != 3 {
		cancel()
		t.Errorf("Should have found 3 resources, got %v", resources)
	}

	log.Printf("Success! Found these resources %v", resources)

	testResourceRead(t, &mcp.ReadResourceParams{URI: "mcp1:file:///customers.txt"}, "Customer list contents", cancel)
	testResourceRead(t, &mcp.ReadResourceParams{URI: "mcp2:file:///companies.txt"}, "Company list contents", cancel)

	resourceTemplates, err := getResourceTemplates()
	if err != nil {
		cancel()
		t.Errorf("Failed getting resource templates %v", err.Error())
	}
	if len(resourceTemplates) != 1 {
		cancel()
		t.Errorf("Should have found 1 resource template, got %v", resourceTemplates)
	}

	log.Printf("Success! Found these resource templates %v", resourceTemplates)

	// "mcp1:file:///notes.txt" was never registered as an exact resource —
	// it's only reachable through the "mcp1:file:///{path}" template match.
	testResourceRead(t, &mcp.ReadResourceParams{URI: "mcp1:file:///notes.txt"}, "Contents of file:///notes.txt", cancel)

	// Everything above ran as "admin", which access_rules_test.json grants
	// "*" for every kind — it never exercises a deny path. Connect as
	// "development.engineer" (matched by the "development.*" rule, which
	// only allows "mcp1.dev_*" prompts, "mcp1:file:///dev-*" resources, and
	// the "mcp1:file:///*" resource template) to prove both the allow and
	// deny sides of access control for prompts/resources/resource templates.
	devSession, err := connectAsRole(context.Background(), PROXY_URL, "development.engineer")
	if err != nil {
		cancel()
		t.Fatalf("Failed to connect as development.engineer: %v", err)
	}
	defer devSession.Close()

	if result, err := devSession.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "mcp1.dev_notes"}); err != nil {
		t.Errorf("development role should be allowed to get mcp1.dev_notes: %v", err)
	} else if got := result.Messages[0].Content.(*mcp.TextContent).Text; got != "Dev notes: TODO fix bug" {
		t.Errorf("unexpected mcp1.dev_notes content: %v", got)
	}

	if _, err := devSession.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "mcp1.greet_customer"}); err == nil {
		t.Errorf("development role should be denied mcp1.greet_customer, but the call succeeded")
	}

	if result, err := devSession.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp1:file:///dev-notes.txt"}); err != nil {
		t.Errorf("development role should be allowed to read mcp1:file:///dev-notes.txt: %v", err)
	} else if got := result.Contents[0].Text; got != "Dev notes file contents" {
		t.Errorf("unexpected mcp1:file:///dev-notes.txt content: %v", got)
	}

	if _, err := devSession.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp1:file:///customers.txt"}); err == nil {
		t.Errorf("development role should be denied mcp1:file:///customers.txt, but the call succeeded")
	}

	devTemplates, err := devSession.ListResourceTemplates(context.Background(), &mcp.ListResourceTemplatesParams{})
	if err != nil {
		t.Errorf("development role failed listing resource templates: %v", err)
	} else {
		found := false
		for _, tmpl := range devTemplates.ResourceTemplates {
			if tmpl.URITemplate == "mcp1:file:///{path}" {
				found = true
			}
		}
		if !found {
			t.Errorf("development role should see the mcp1:file:///{path} template, got %v", devTemplates.ResourceTemplates)
		}
	}

	// Template-matched reads are still gated per concrete URI by
	// allowed_resources, even though the template itself is listed above:
	// "dev-secret-via-template.txt" matches "mcp1:file:///dev-*" so it's
	// allowed, but plain "secret.txt" doesn't, so it's denied — despite both
	// only existing via the same template match, never as exact resources.
	if result, err := devSession.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp1:file:///dev-secret-via-template.txt"}); err != nil {
		t.Errorf("development role should be allowed mcp1:file:///dev-secret-via-template.txt via template match: %v", err)
	} else if got := result.Contents[0].Text; got != "Contents of file:///dev-secret-via-template.txt" {
		t.Errorf("unexpected template-matched content: %v", got)
	}

	if _, err := devSession.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp1:file:///secret.txt"}); err == nil {
		t.Errorf("development role should be denied mcp1:file:///secret.txt even though it matches the template, but the call succeeded")
	}
}

func testPromptGet(t *testing.T, params *mcp.GetPromptParams, expectedOutput string, cancel context.CancelFunc) {

	result, err := callPrompt(PROXY_URL, params)
	if err != nil {
		cancel()
		t.Errorf("Failed calling prompt %v", err.Error())
	}
	log.Printf("Result from prompt [%v]", result)
	if result != expectedOutput {
		cancel()
		t.Errorf("Got wrong result from testPromptGet: [%v]", result)
	}

}

func testResourceRead(t *testing.T, params *mcp.ReadResourceParams, expectedOutput string, cancel context.CancelFunc) {

	result, err := callResource(PROXY_URL, params)
	if err != nil {
		cancel()
		t.Errorf("Failed calling resource %v", err.Error())
	}
	log.Printf("Result from resource [%v]", result)
	if result != expectedOutput {
		cancel()
		t.Errorf("Got wrong result from testResourceRead: [%v]", result)
	}

}

func testToolCall(t *testing.T, params mcp.CallToolParams, expectedOutput string, cancel context.CancelFunc) {

	result, err := callTool(PROXY_URL, params)
	if err != nil {
		cancel()
		t.Errorf("Failed calling tool %v", err.Error())
	}
	log.Printf("Result from tool [%v]", result)
	if result != expectedOutput {
		cancel()
		t.Errorf(" Got wrong result from testRunSayHello: [%v]", result)
	}

}
