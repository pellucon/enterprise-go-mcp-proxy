// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Command patchcfg mechanically updates one or more committed *.cfg files'
// own baked-in accessRules/declarativeGuardrails/governanceOrchestration
// sections from go-gardening-demo/initial-data's generated/hand-authored
// JSON files (client_access_rules.json/declarative_guardrails.json/
// orchestration.json), using the exact same merge logic
// go-gardening-demo/cli's own "admin update_*" commands apply live against
// a running proxy (go-gardening-demo/configmerge) — but entirely offline,
// with no proxy involved at all.
//
// Why this exists: go-proxy's own self-seeding (configsync.Writer.
// EnsureSeeded, CLAUDE.md's "Unify configuration into a single
// config-map mechanism") makes a committed *.cfg file the authoritative
// initial value for these sections the moment go-proxy starts, so each cfg
// needs its own complete, up-to-date copy of the generated content — not
// just a reference to the initial-data files, which only get applied
// later, via "cli init", to an already-running cluster. Before this tool
// existed, keeping a cfg's baked-in copy in sync with the initial-data
// files (after regenerating client_access_rules.json via gendata, or
// hand-editing orchestration.json/declarative_guardrails.json, neither of
// which gendata generates at all) was a manual copy-paste step with
// nothing catching drift between the two.
//
// A section absent from a given cfg is left absent — this tool only ever
// patches a section a cfg already opts into, never adds one it doesn't
// already have (e.g. gardening_demo_test.cfg deliberately carries no
// governanceOrchestration section at all, and stays that way). Every other
// top-level key, and its own key order, is preserved untouched; only the
// three patched sections' own byte content changes.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/configmerge"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

const usage = `patchcfg: merge go-gardening-demo/initial-data's generated files into one
or more committed *.cfg files' own baked-in accessRules/
declarativeGuardrails/governanceOrchestration sections.

Usage:
  patchcfg [-data-dir DIR] <cfg-file> [<cfg-file> ...]

  -data-dir DIR   Directory containing client_access_rules.json/
                  declarative_guardrails.json/orchestration.json
                  (default "go-gardening-demo/initial-data")

A section is only patched if the target cfg already has it - this tool
never adds a section a cfg doesn't already opt into (e.g.
gardening_demo_test.cfg has no governanceOrchestration section, and stays
that way). Every other top-level key, and the file's own key order, is
left untouched.

Example:
  go run ./go-gardening-demo/patchcfg \
    go-gardening-demo/gardening-demo.cfg \
    go-gardening-demo/gardening_demo_test.cfg
`

func main() {
	fs := flag.NewFlagSet("patchcfg", flag.ExitOnError)
	dataDir := fs.String("data-dir", "go-gardening-demo/initial-data", "directory containing the generated/hand-authored JSON files")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(os.Args[1:])

	targets := fs.Args()
	if len(targets) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	for _, path := range targets {
		if err := patchConfig(path, *dataDir); err != nil {
			log.Fatalf("%s: %v", path, err)
		}
	}
}

// patchConfig reads path (a combined proxy *.cfg JSON document), merges
// go-gardening-demo/initial-data's files into whichever of accessRules/
// declarativeGuardrails/governanceOrchestration it already has, and writes
// the result back to path in place, preserving every other key's own
// content and the document's original top-level key order.
func patchConfig(path, dataDir string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read: %w", err)
	}
	keys, values, err := orderedDocument(raw)
	if err != nil {
		return fmt.Errorf("failed to parse as a JSON object: %w", err)
	}

	accessRulesPatched, err := patchAccessRules(values, dataDir)
	if err != nil {
		return fmt.Errorf("accessRules: %w", err)
	}
	guardrailsPatched, err := patchDeclarativeGuardrails(values, dataDir)
	if err != nil {
		return fmt.Errorf("declarativeGuardrails: %w", err)
	}
	orchestrationPatched, err := patchOrchestration(values, dataDir)
	if err != nil {
		return fmt.Errorf("governanceOrchestration: %w", err)
	}

	if !accessRulesPatched && !guardrailsPatched && !orchestrationPatched {
		fmt.Printf("%s: no matching sections present, nothing to patch\n", path)
		return nil
	}

	if err := writeOrderedDocument(path, keys, values); err != nil {
		return fmt.Errorf("failed to write patched config: %w", err)
	}
	fmt.Printf("%s: patched (accessRules=%v declarativeGuardrails=%v governanceOrchestration=%v)\n",
		path, accessRulesPatched, guardrailsPatched, orchestrationPatched)
	return nil
}

// patchAccessRules merges go-gardening-demo/initial-data/client_access_rules.json
// into values["accessRules"] via configmerge.MergeAccessRules, in place.
// Returns false, nil if the target document has no "accessRules" key at
// all (see this package's own doc comment on why that's left alone rather
// than added).
func patchAccessRules(values map[string]json.RawMessage, dataDir string) (bool, error) {
	raw, ok := values["accessRules"]
	if !ok {
		return false, nil
	}
	var current access_control.AccessConfig
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("failed to parse existing section: %w", err)
	}
	newRules, err := readJSONFile[[]access_control.AccessibilityRule](filepath.Join(dataDir, "client_access_rules.json"))
	if err != nil {
		return false, err
	}
	merged := configmerge.MergeAccessRules(current.ClientAccessRules, newRules)
	patched, err := marshalNoEscape(access_control.AccessConfig{ClientAccessRules: merged})
	if err != nil {
		return false, fmt.Errorf("failed to marshal merged section: %w", err)
	}
	values["accessRules"] = patched
	return true, nil
}

// patchDeclarativeGuardrails merges go-gardening-demo/initial-data/
// declarative_guardrails.json into values["declarativeGuardrails"] via
// configmerge.MergeDeclarativeGuardrails, in place. Returns false, nil if
// the target document has no "declarativeGuardrails" key at all.
func patchDeclarativeGuardrails(values map[string]json.RawMessage, dataDir string) (bool, error) {
	raw, ok := values["declarativeGuardrails"]
	if !ok {
		return false, nil
	}
	var current guardrailcfg.RawConfig
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("failed to parse existing section: %w", err)
	}
	newDeclarations, err := readJSONFile[[]guardrailcfg.RawDeclaration](filepath.Join(dataDir, "declarative_guardrails.json"))
	if err != nil {
		return false, err
	}
	actions, declarations := configmerge.MergeDeclarativeGuardrails(current, newDeclarations)
	patched, err := marshalNoEscape(guardrailcfg.RawConfig{Actions: actions, Declarations: declarations})
	if err != nil {
		return false, fmt.Errorf("failed to marshal merged section: %w", err)
	}
	values["declarativeGuardrails"] = patched
	return true, nil
}

// patchOrchestration merges go-gardening-demo/initial-data/orchestration.json
// into values["governanceOrchestration"] via configmerge.MergeOrchestration,
// in place. Returns false, nil if the target document has no
// "governanceOrchestration" key at all (e.g. gardening_demo_test.cfg).
func patchOrchestration(values map[string]json.RawMessage, dataDir string) (bool, error) {
	raw, ok := values["governanceOrchestration"]
	if !ok {
		return false, nil
	}
	var current triggered_tools_runtime.OrchestrationConfig
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("failed to parse existing section: %w", err)
	}
	newConfig, err := readJSONFile[triggered_tools_runtime.OrchestrationConfig](filepath.Join(dataDir, "orchestration.json"))
	if err != nil {
		return false, err
	}
	merged := configmerge.MergeOrchestration(current, newConfig)
	patched, err := marshalNoEscape(merged)
	if err != nil {
		return false, fmt.Errorf("failed to marshal merged section: %w", err)
	}
	values["governanceOrchestration"] = patched
	return true, nil
}

// marshalNoEscape marshals v without HTML-escaping "&"/"<"/">" (the
// default json.Marshal behavior, meant for embedding JSON in HTML) - these
// sections routinely carry roleExpression strings like
// "!admin & !business-manager", which should stay human-readable rather
// than turning into "!admin & !business-manager" every time this tool
// runs.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func readJSONFile[T any](path string) (T, error) {
	var v T
	raw, err := os.ReadFile(path)
	if err != nil {
		return v, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return v, nil
}

// orderedDocument parses raw as a top-level JSON object, returning its keys
// in their original on-disk order alongside a map of each key's own raw
// (still-encoded) value — the combination writeOrderedDocument needs to
// reproduce the document with only specific keys' values changed and every
// other key, and the object's own key order, left exactly as found.
func orderedDocument(raw []byte) (keys []string, values map[string]json.RawMessage, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("expected a top-level JSON object")
	}
	values = make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected a string key, got %v", keyTok)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, nil, fmt.Errorf("failed to read value for key %q: %w", key, err)
		}
		keys = append(keys, key)
		values[key] = val
	}
	return keys, values, nil
}

// writeOrderedDocument writes a JSON object to path with keys in exactly
// the given order, each value taken from values and re-indented (2 spaces
// per level, matching every other JSON file in this repo) to nest properly
// one level under the top-level object.
func writeOrderedDocument(path string, keys []string, values map[string]json.RawMessage) error {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range keys {
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return err
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, values[k], "  ", "  "); err != nil {
			return fmt.Errorf("failed to format value for key %q: %w", k, err)
		}
		buf.WriteString("  ")
		buf.Write(keyJSON)
		buf.WriteString(": ")
		buf.Write(indented.Bytes())
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
