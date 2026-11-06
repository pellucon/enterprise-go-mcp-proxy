// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestContractSchemasInfer(t *testing.T) {
	cases := []struct {
		name  string
		props []string
	}{
		{"CallAuthorizationInput", []string{"user", "roles", "callRequest"}},
		{"ResultAuthorizationInput", []string{"user", "roles", "callResult"}},
		{"AuthorizationOutput", []string{"authorized"}},
	}

	schemas := map[string]*jsonschema.Schema{}
	s, err := jsonschema.For[CallAuthorizationInput](nil)
	if err != nil {
		t.Fatalf("jsonschema.For[CallAuthorizationInput]: %v", err)
	}
	schemas["CallAuthorizationInput"] = s

	s, err = jsonschema.For[ResultAuthorizationInput](nil)
	if err != nil {
		t.Fatalf("jsonschema.For[ResultAuthorizationInput]: %v", err)
	}
	schemas["ResultAuthorizationInput"] = s

	s, err = jsonschema.For[AuthorizationOutput](nil)
	if err != nil {
		t.Fatalf("jsonschema.For[AuthorizationOutput]: %v", err)
	}
	schemas["AuthorizationOutput"] = s

	for _, c := range cases {
		schema := schemas[c.name]
		for _, p := range c.props {
			if _, ok := schema.Properties[p]; !ok {
				t.Errorf("%s: expected property %q in inferred schema, got keys %v", c.name, p, keysOf(schema.Properties))
			}
		}
	}
}

func keysOf(m map[string]*jsonschema.Schema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
