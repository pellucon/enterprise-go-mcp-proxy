// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// configmap_test.go proves the "unify configuration into a single
// config-map mechanism" design (see CLAUDE.md) end-to-end through the
// real admin-tool-call path: an _admin.update_role_permissions call
// persists via configsync.Writer, pings every subscribed
// configsync.Watcher over NATS, and a Watcher's bounded-retry reload
// picks up the new content once it "lands" on disk - exactly like a
// second replica would react to another replica's admin update.
//
// This test cannot exercise configsync's k8s-ConfigMap code path (that's
// covered directly, with a fake clientset, by configsync's own
// writer_test.go) - configsync.getK8sClientset is an unexported
// sync.Once singleton with no injection seam from another package. Outside
// k8s, Writer.Update/Export fall back to an in-process value - this test
// exercises exactly that path, which is real production behavior (the
// standalone binary), not a test-only stub.
package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	CONFIGMAP_TEST_PROXY_PORT = ":8109"
	CONFIGMAP_TEST_PROXY_URL  = "http://localhost:8109/mcp"

	EXPORT_FULL_CONFIG_TEST_PROXY_PORT = ":8110"
	EXPORT_FULL_CONFIG_TEST_PROXY_URL  = "http://localhost:8110/mcp"

	RATE_LIMITING_CONFIGMAP_TEST_PROXY_PORT = ":8118"
	RATE_LIMITING_CONFIGMAP_TEST_PROXY_URL  = "http://localhost:8118/mcp"

	LOG_LEVEL_CONFIGMAP_TEST_PROXY_PORT = ":8119"
	LOG_LEVEL_CONFIGMAP_TEST_PROXY_URL  = "http://localhost:8119/mcp"

	ROLE_MAPPING_TEST_PROXY_PORT = ":8120"
	ROLE_MAPPING_TEST_PROXY_URL  = "http://localhost:8120/mcp"
)

// exportRolePermissions calls _admin.export_role_permissions and decodes
// its StructuredContent, mirroring triggered-tools-runtime/orchestrate.go's
// decodeAuthorizationOutput pattern.
func exportRolePermissions(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[access_control.AccessConfig], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_role_permissions", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, err
	}
	if res.IsError {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, fmt.Errorf("export_role_permissions returned an error result: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, err
	}
	var out configsync.VersionedConfig[access_control.AccessConfig]
	if err := json.Unmarshal(raw, &out); err != nil {
		return configsync.VersionedConfig[access_control.AccessConfig]{}, err
	}
	return out, nil
}

func updateRolePermissions(ctx context.Context, session *mcp.ClientSession, in configsync.VersionedConfig[access_control.AccessConfig]) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.update_role_permissions", Arguments: args})
}

// TestConfigMapAdminUpdatePropagatesViaNATS drives the full lifecycle: an
// admin exports the current (seeded-at-version-1) role permissions,
// updates them, and a second, independent Watcher - standing in for a
// second replica's own Watcher, subscribed to the exact same NATS subject
// and reading the exact same override path the proxy's own Watcher would
// in k8s - observes the change once the (simulated) override file lands.
// A stale-version second update is then rejected without changing
// anything, proving the optimistic-concurrency check is enforced on the
// real admin-tool-call path, not just in configsync's own unit tests.
func TestConfigMapAdminUpdatePropagatesViaNATS(t *testing.T) {
	// Shortened so the bounded retry/backoff in Watcher.reload (configsync/
	// watcher.go) doesn't make this test slow; restored after.
	oldTimeout, oldInitial, oldMax := configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff
	configsync.ReloadTimeout = 5 * time.Second
	configsync.ReloadInitialBackoff = 20 * time.Millisecond
	configsync.ReloadMaxBackoff = 100 * time.Millisecond
	t.Cleanup(func() {
		configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff = oldTimeout, oldInitial, oldMax
	})

	natsURL := startTestNATSServer(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "configmap-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, CONFIGMAP_TEST_PROXY_PORT)

	adminSession, err := connectAsRole(ctx, CONFIGMAP_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer adminSession.Close()

	exported, err := exportRolePermissions(ctx, adminSession)
	if err != nil {
		t.Fatalf("initial export failed: %v", err)
	}
	if exported.Version != 1 {
		t.Fatalf("expected a freshly-started, never-updated section to export as version 1 (proxy-server.cfg's own value, seeded), got version %d", exported.Version)
	}
	if len(exported.Config.ClientAccessRules) != 1 {
		t.Fatalf("expected the 1 rule from cfg, got %+v", exported.Config.ClientAccessRules)
	}

	// A second, independent Watcher standing in for a second replica: same
	// NATS subject, same override path a real k8s deployment would mount
	// this replica's own Watcher at.
	replicaNC, err := configsync.Connect(natsURL)
	if err != nil {
		t.Fatalf("failed to connect replica watcher to NATS: %v", err)
	}
	defer replicaNC.Close()
	overridePath := overrideMountPath(cfgPath, "access-rules")
	replicaWatcher, err := configsync.NewWatcher("access-rules", overridePath, replicaNC, &exported.Config, access_control.ParseConfig, nil)
	if err != nil {
		t.Fatalf("failed to construct replica watcher: %v", err)
	}
	defer replicaWatcher.Close()
	// Core NATS drops a publish with no registered subscriber yet rather
	// than queuing it (see events/client_test.go's identical Flush
	// rationale) - without this, the update below can race the SUB
	// command that NewWatcher's internal Subscribe just issued.
	if err := replicaNC.Flush(); err != nil {
		t.Fatalf("failed to flush replica watcher's subscription: %v", err)
	}

	newRules := append(append([]access_control.AccessibilityRule{}, exported.Config.ClientAccessRules...),
		access_control.AccessibilityRule{Role: "guest", AllowedTools: []string{"demo.*"}})
	newConfig := access_control.AccessConfig{ClientAccessRules: newRules}

	updateResult, err := updateRolePermissions(ctx, adminSession, configsync.VersionedConfig[access_control.AccessConfig]{Version: exported.Version, Config: newConfig})
	if err != nil {
		t.Fatalf("update_role_permissions transport error: %v", err)
	}
	if updateResult.IsError {
		t.Fatalf("expected the version-1 update to succeed, got error: %v", updateResult.Content)
	}

	// Simulate kubelet syncing the ConfigMap -> volume mount some short
	// while after the write actually happened (writer.Update, inside the
	// update call above, already pinged every subscriber - both the
	// proxy's own Watcher and replicaWatcher are already retrying, finding
	// nothing yet, backing off).
	time.Sleep(50 * time.Millisecond)
	versioned := configsync.VersionedConfig[access_control.AccessConfig]{Version: 2, Config: newConfig}
	raw, err := json.Marshal(versioned)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(overridePath, raw, 0o644); err != nil {
		t.Fatalf("simulating kubelet volume sync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		cur := replicaWatcher.Current()
		if len(cur.ClientAccessRules) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica watcher did not observe the update via NATS within the deadline, current=%+v", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Same-replica sanity check: the admin call's own apply() already
	// updated this replica's live AccessPolicyEngine synchronously (see
	// admin_tools.go's registerSectionAdminTools), independent of the NATS
	// round trip just proven above - guest can now reach a demo.* tool.
	if !accessPolicyEngine.IsToolAuthorized(jwt.MapClaims{"role": "guest"}, "demo.say_hello") {
		t.Error("expected guest to be authorized for demo.* after the update took effect on this replica")
	}

	// Version-mismatch case: export shows version 2 now; a second update
	// submitting the stale version 1 must be rejected, and the config map
	// must remain unchanged.
	reExported, err := exportRolePermissions(ctx, adminSession)
	if err != nil {
		t.Fatalf("re-export failed: %v", err)
	}
	if reExported.Version != 2 {
		t.Fatalf("expected version 2 after the update, got %d", reExported.Version)
	}

	staleResult, err := updateRolePermissions(ctx, adminSession, configsync.VersionedConfig[access_control.AccessConfig]{
		Version: 1, // stale - the config map is actually at version 2
		Config:  access_control.AccessConfig{ClientAccessRules: []access_control.AccessibilityRule{{Role: "should-not-apply", AllowedTools: []string{"*"}}}},
	})
	if err != nil {
		t.Fatalf("stale update transport error: %v", err)
	}
	if !staleResult.IsError {
		t.Fatal("expected a stale-version update to be rejected")
	}

	finalExported, err := exportRolePermissions(ctx, adminSession)
	if err != nil {
		t.Fatalf("final export failed: %v", err)
	}
	if finalExported.Version != 2 {
		t.Errorf("expected the rejected stale update to leave the config map at version 2, got %d", finalExported.Version)
	}
	for _, r := range finalExported.Config.ClientAccessRules {
		if r.Role == "should-not-apply" {
			t.Error("expected the rejected stale update's content to never have been applied")
		}
	}
}

// exportRateLimiting/updateRateLimiting mirror exportRolePermissions/
// updateRolePermissions above, for the "rate_limiting" section.
func exportRateLimiting(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[ratelimit.Config], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_rate_limiting", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[ratelimit.Config]{}, err
	}
	if res.IsError {
		return configsync.VersionedConfig[ratelimit.Config]{}, fmt.Errorf("export_rate_limiting returned an error result: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[ratelimit.Config]{}, err
	}
	var out configsync.VersionedConfig[ratelimit.Config]
	if err := json.Unmarshal(raw, &out); err != nil {
		return configsync.VersionedConfig[ratelimit.Config]{}, err
	}
	return out, nil
}

func updateRateLimiting(ctx context.Context, session *mcp.ClientSession, in configsync.VersionedConfig[ratelimit.Config]) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.update_rate_limiting", Arguments: args})
}

// TestRateLimitingConfigAdminUpdatePropagatesViaNATS mirrors
// TestConfigMapAdminUpdatePropagatesViaNATS above, scoped to the
// "rate_limiting" section (go-proxy's own gate reads it directly via
// rateLimitingWatcher.Current(), the same "Watcher IS the engine" shape as
// discovery, so there's no separate engine.Update to also assert against
// here — the propagation mechanics themselves are what's being proven).
func TestRateLimitingConfigAdminUpdatePropagatesViaNATS(t *testing.T) {
	oldTimeout, oldInitial, oldMax := configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff
	configsync.ReloadTimeout = 5 * time.Second
	configsync.ReloadInitialBackoff = 20 * time.Millisecond
	configsync.ReloadMaxBackoff = 100 * time.Millisecond
	t.Cleanup(func() {
		configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff = oldTimeout, oldInitial, oldMax
	})

	natsURL := startTestNATSServer(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "rate-limiting-configmap-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "rateLimiting": { "timeoutSeconds": 1, "tools": {} },
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, RATE_LIMITING_CONFIGMAP_TEST_PROXY_PORT)

	adminSession, err := connectAsRole(ctx, RATE_LIMITING_CONFIGMAP_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer adminSession.Close()

	exported, err := exportRateLimiting(ctx, adminSession)
	if err != nil {
		t.Fatalf("initial export failed: %v", err)
	}
	if exported.Version != 1 {
		t.Fatalf("expected a freshly-started, never-updated section to export as version 1, got version %d", exported.Version)
	}
	if exported.Config.TimeoutSeconds == nil || *exported.Config.TimeoutSeconds != 1 {
		t.Fatalf("expected the seeded timeoutSeconds=1 from cfg, got %+v", exported.Config)
	}

	replicaNC, err := configsync.Connect(natsURL)
	if err != nil {
		t.Fatalf("failed to connect replica watcher to NATS: %v", err)
	}
	defer replicaNC.Close()
	overridePath := overrideMountPath(cfgPath, "rate-limiting")
	replicaWatcher, err := configsync.NewWatcher("rate-limiting", overridePath, replicaNC, &exported.Config, ratelimit.ParseConfig, nil)
	if err != nil {
		t.Fatalf("failed to construct replica watcher: %v", err)
	}
	defer replicaWatcher.Close()
	if err := replicaNC.Flush(); err != nil {
		t.Fatalf("failed to flush replica watcher's subscription: %v", err)
	}

	newConfig := ratelimit.Config{
		TimeoutSeconds: exported.Config.TimeoutSeconds,
		Tools: map[string]ratelimit.ToolConfig{
			"demo.rate_limited_tool": {GlobalPerMinute: intPtr(10)},
		},
	}
	updateResult, err := updateRateLimiting(ctx, adminSession, configsync.VersionedConfig[ratelimit.Config]{Version: exported.Version, Config: newConfig})
	if err != nil {
		t.Fatalf("update_rate_limiting transport error: %v", err)
	}
	if updateResult.IsError {
		t.Fatalf("expected the version-1 update to succeed, got error: %v", updateResult.Content)
	}

	// Same-replica sanity check: the admin call's own apply()
	// (rateLimitingWatcher.SetLocal) already updated this replica's live
	// Watcher synchronously, independent of the NATS round trip proven
	// below.
	if _, ok := rateLimitingWatcher.Current().Tools["demo.rate_limited_tool"]; !ok {
		t.Error("expected this replica's own rateLimitingWatcher to reflect the update immediately via SetLocal")
	}

	time.Sleep(50 * time.Millisecond)
	versioned := configsync.VersionedConfig[ratelimit.Config]{Version: 2, Config: newConfig}
	raw, err := json.Marshal(versioned)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(overridePath, raw, 0o644); err != nil {
		t.Fatalf("simulating kubelet volume sync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := replicaWatcher.Current().Tools["demo.rate_limited_tool"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica watcher did not observe the update via NATS within the deadline, current=%+v", replicaWatcher.Current())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Stale-version case.
	staleResult, err := updateRateLimiting(ctx, adminSession, configsync.VersionedConfig[ratelimit.Config]{
		Version: 1, // stale - the config map is actually at version 2
		Config:  ratelimit.Config{Tools: map[string]ratelimit.ToolConfig{"should-not-apply": {GlobalPerMinute: intPtr(1)}}},
	})
	if err != nil {
		t.Fatalf("stale update transport error: %v", err)
	}
	if !staleResult.IsError {
		t.Fatal("expected a stale-version update to be rejected")
	}

	finalExported, err := exportRateLimiting(ctx, adminSession)
	if err != nil {
		t.Fatalf("final export failed: %v", err)
	}
	if finalExported.Version != 2 {
		t.Errorf("expected the rejected stale update to leave the config map at version 2, got %d", finalExported.Version)
	}
	if _, ok := finalExported.Config.Tools["should-not-apply"]; ok {
		t.Error("expected the rejected stale update's content to never have been applied")
	}
}

// TestExportFullConfig proves _admin.export_full_config returns every
// section unwrapped (no {"version":N,...} envelope anywhere - the whole
// point being that the result is a valid proxy-server.cfg by itself, not
// something a caller has to post-process first) and that an admin-tool
// change to one configsync-managed section (role permissions) is reflected
// immediately, alongside the static auth section that's never
// configsync-managed at all.
func TestExportFullConfig(t *testing.T) {
	natsURL := startTestNATSServer(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "export-full-config-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "rateLimiting": { "timeoutSeconds": 3, "tools": {} },
  "selfUrl": "http://go-proxy.mcp-proxy.svc.cluster.local/mcp",
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, EXPORT_FULL_CONFIG_TEST_PROXY_PORT)

	adminSession, err := connectAsRole(ctx, EXPORT_FULL_CONFIG_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer adminSession.Close()

	// Give role permissions a real admin-tool change to prove the export
	// reflects current state, not just the original file.
	exportedRules, err := exportRolePermissions(ctx, adminSession)
	if err != nil {
		t.Fatalf("initial role-permissions export failed: %v", err)
	}
	newRules := append(append([]access_control.AccessibilityRule{}, exportedRules.Config.ClientAccessRules...),
		access_control.AccessibilityRule{Role: "guest", AllowedTools: []string{"demo.*"}})
	updateResult, err := updateRolePermissions(ctx, adminSession, configsync.VersionedConfig[access_control.AccessConfig]{
		Version: exportedRules.Version,
		Config:  access_control.AccessConfig{ClientAccessRules: newRules},
	})
	if err != nil {
		t.Fatalf("update_role_permissions transport error: %v", err)
	}
	if updateResult.IsError {
		t.Fatalf("expected the role-permissions update to succeed, got error: %v", updateResult.Content)
	}

	res, err := adminSession.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_full_config", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("export_full_config transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("export_full_config returned an error result: %v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("failed to marshal StructuredContent: %v", err)
	}

	// No "version" key anywhere in the document - if any configsync
	// section were still wrapped in its {"version":N,"config":{...}}
	// envelope, this would catch it (VersionedConfig's own json tag is
	// literally "version").
	if bytesContainsVersionKey(raw) {
		t.Fatalf("expected no \"version\" key anywhere in export_full_config's output, got: %s", raw)
	}

	var out fullConfigExport
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("failed to decode export_full_config's output as a fullConfigExport: %v", err)
	}
	foundGuest := false
	for _, r := range out.AccessRules.ClientAccessRules {
		if r.Role == "guest" {
			foundGuest = true
		}
	}
	if !foundGuest {
		t.Errorf("expected the just-applied role-permissions update to be reflected in export_full_config's accessRules, got %+v", out.AccessRules.ClientAccessRules)
	}
	if out.Auth.PublicKeyPath != "./public.pem" {
		t.Errorf("expected the static auth section (never configsync-managed) to round-trip from the original cfg, got %+v", out.Auth)
	}
	if out.SelfURL != "http://go-proxy.mcp-proxy.svc.cluster.local/mcp" {
		t.Errorf("expected selfUrl to round-trip from the original cfg, got %q", out.SelfURL)
	}
	if out.NatsURL != natsURL {
		t.Errorf("expected natsUrl to round-trip from the original cfg, got %q", out.NatsURL)
	}
	if out.RateLimiting.TimeoutSeconds == nil || *out.RateLimiting.TimeoutSeconds != 3 {
		t.Errorf("expected rateLimiting.timeoutSeconds=3 to round-trip from the original cfg, got %+v", out.RateLimiting)
	}
}

// TestAdministratorRoleMapping proves roleMapping.administrator, not the
// literal role "admin", decides who may call _admin.* tools; that the
// mapping round-trips through _admin.export_full_config; and that no admin
// tool can change it — even an accessRules update granting "admin"
// everything leaves "admin" unable to call _admin.* tools.
func TestAdministratorRoleMapping(t *testing.T) {
	natsURL := startTestNATSServer(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "role-mapping-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "ops-admins" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, ROLE_MAPPING_TEST_PROXY_PORT)

	// Each session carries its own token (bearerRoundTripper) rather than
	// going through connectAsRole, whose shared testToken var would make
	// every earlier session silently switch to the latest role's token.
	connect := func(role string) (*mcp.ClientSession, error) {
		token, err := GenerateTestTokenForRole(role)
		if err != nil {
			return nil, err
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   ROLE_MAPPING_TEST_PROXY_URL,
			HTTPClient: &http.Client{Transport: bearerRoundTripper(token)},
		}, nil)
	}

	adminToolDenied := func(t *testing.T, role, tool string) bool {
		t.Helper()
		session, err := connect(role)
		if err != nil {
			t.Fatalf("failed to connect as %q: %v", role, err)
		}
		defer session.Close()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		return err != nil || res.IsError
	}

	opsSession, err := connect("ops-admins")
	if err != nil {
		t.Fatalf("failed to connect as ops-admins: %v", err)
	}
	defer opsSession.Close()

	t.Run("the mapped administrator role can call _admin tools and the mapping round-trips", func(t *testing.T) {
		res, err := opsSession.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_full_config", Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("export_full_config transport error: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected ops-admins to be allowed to call export_full_config, got: %v", res.Content)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("failed to marshal StructuredContent: %v", err)
		}
		var out fullConfigExport
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("failed to decode export_full_config's output: %v", err)
		}
		if out.RoleMapping.Administrator != "ops-admins" {
			t.Errorf("expected roleMapping.administrator=ops-admins to round-trip, got %+v", out.RoleMapping)
		}
	})

	t.Run("the literal role admin is no longer an administrator", func(t *testing.T) {
		if !adminToolDenied(t, "admin", "_admin.export_full_config") {
			t.Error("expected role \"admin\" to be denied _admin.export_full_config when administrator is mapped to ops-admins")
		}
	})

	t.Run("an accessRules update cannot make another role an administrator", func(t *testing.T) {
		exported, err := exportRolePermissions(ctx, opsSession)
		if err != nil {
			t.Fatalf("role-permissions export failed: %v", err)
		}
		updateResult, err := updateRolePermissions(ctx, opsSession, configsync.VersionedConfig[access_control.AccessConfig]{
			Version: exported.Version,
			Config: access_control.AccessConfig{ClientAccessRules: []access_control.AccessibilityRule{
				{Role: "*", AllowedTools: []string{"*"}, AllowedPrompts: []string{"*"}, AllowedResources: []string{"*"}, AllowedResourceTemplates: []string{"*"}},
			}},
		})
		if err != nil {
			t.Fatalf("update_role_permissions transport error: %v", err)
		}
		if updateResult.IsError {
			t.Fatalf("expected the role-permissions update to succeed, got error: %v", updateResult.Content)
		}

		// The update takes effect via the configsync Watcher; wait until a
		// non-admin tool decision reflects it before checking _admin.*.
		deadline := time.Now().Add(10 * time.Second)
		for {
			snap := accessPolicyEngine.Snapshot()
			if len(snap.ClientAccessRules) == 1 && snap.ClientAccessRules[0].Role == "*" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("access-rules update never reached the engine, still %+v", snap)
			}
			time.Sleep(100 * time.Millisecond)
		}

		if !adminToolDenied(t, "admin", "_admin.export_full_config") {
			t.Error("expected role \"admin\" to stay denied _admin.* tools after an accessRules update granting \"*\" everything")
		}
		if adminToolDenied(t, "ops-admins", "_admin.export_full_config") {
			t.Error("expected ops-admins to remain the administrator after the accessRules update")
		}
	})
}

// exportLogLevel/updateLogLevel mirror exportRolePermissions/
// updateRolePermissions above, for the "log_level" section.
func exportLogLevel(ctx context.Context, session *mcp.ClientSession) (configsync.VersionedConfig[logging.LevelConfig], error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_log_level", Arguments: map[string]any{}})
	if err != nil {
		return configsync.VersionedConfig[logging.LevelConfig]{}, err
	}
	if res.IsError {
		return configsync.VersionedConfig[logging.LevelConfig]{}, fmt.Errorf("export_log_level returned an error result: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return configsync.VersionedConfig[logging.LevelConfig]{}, err
	}
	var out configsync.VersionedConfig[logging.LevelConfig]
	if err := json.Unmarshal(raw, &out); err != nil {
		return configsync.VersionedConfig[logging.LevelConfig]{}, err
	}
	return out, nil
}

func updateLogLevel(ctx context.Context, session *mcp.ClientSession, in configsync.VersionedConfig[logging.LevelConfig]) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.update_log_level", Arguments: args})
}

// TestLogLevelConfigAdminUpdatePropagatesViaNATS mirrors
// TestRateLimitingConfigAdminUpdatePropagatesViaNATS above, scoped to the
// "log_level" section. Unlike rate limiting, this section's "engine" is
// process-global (logging.levelVar, via logging.ApplyConfig) rather than an
// object reachable through a package var — so the same-replica sanity check
// below asserts logging.CurrentLevel() directly instead of reading some
// other package's live state.
func TestLogLevelConfigAdminUpdatePropagatesViaNATS(t *testing.T) {
	oldTimeout, oldInitial, oldMax := configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff
	configsync.ReloadTimeout = 5 * time.Second
	configsync.ReloadInitialBackoff = 20 * time.Millisecond
	configsync.ReloadMaxBackoff = 100 * time.Millisecond
	t.Cleanup(func() {
		configsync.ReloadTimeout, configsync.ReloadInitialBackoff, configsync.ReloadMaxBackoff = oldTimeout, oldInitial, oldMax
	})
	// Restore the process-global level so this test doesn't leak state into
	// whichever test runs next in the same binary.
	t.Cleanup(func() { logging.Init("info") })

	natsURL := startTestNATSServer(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "log-level-configmap-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "logLevel": { "level": "info" },
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, LOG_LEVEL_CONFIGMAP_TEST_PROXY_PORT)

	adminSession, err := connectAsRole(ctx, LOG_LEVEL_CONFIGMAP_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer adminSession.Close()

	exported, err := exportLogLevel(ctx, adminSession)
	if err != nil {
		t.Fatalf("initial export failed: %v", err)
	}
	if exported.Version != 1 {
		t.Fatalf("expected a freshly-started, never-updated section to export as version 1, got version %d", exported.Version)
	}
	if exported.Config.Level != "info" {
		t.Fatalf("expected the seeded level=info from cfg, got %+v", exported.Config)
	}
	if got := logging.CurrentLevel(); got != "info" {
		t.Fatalf("expected the process's own level to start at info, got %q", got)
	}

	replicaNC, err := configsync.Connect(natsURL)
	if err != nil {
		t.Fatalf("failed to connect replica watcher to NATS: %v", err)
	}
	defer replicaNC.Close()
	overridePath := overrideMountPath(cfgPath, "log-level")
	replicaWatcher, err := configsync.NewWatcher("log-level", overridePath, replicaNC, &exported.Config, logging.ParseConfig, nil)
	if err != nil {
		t.Fatalf("failed to construct replica watcher: %v", err)
	}
	defer replicaWatcher.Close()
	if err := replicaNC.Flush(); err != nil {
		t.Fatalf("failed to flush replica watcher's subscription: %v", err)
	}

	newConfig := logging.LevelConfig{Level: "debug"}
	updateResult, err := updateLogLevel(ctx, adminSession, configsync.VersionedConfig[logging.LevelConfig]{Version: exported.Version, Config: newConfig})
	if err != nil {
		t.Fatalf("update_log_level transport error: %v", err)
	}
	if updateResult.IsError {
		t.Fatalf("expected the version-1 update to succeed, got error: %v", updateResult.Content)
	}

	// Same-replica sanity check: the admin call's own apply()
	// (logging.ApplyConfig) already flipped this replica's own process-wide
	// level synchronously, independent of the NATS round trip proven below
	// — this is the actual "no restart needed" behavior the whole feature
	// exists for.
	if got := logging.CurrentLevel(); got != "debug" {
		t.Errorf("expected this replica's own log level to be debug immediately after the update, got %q", got)
	}

	time.Sleep(50 * time.Millisecond)
	versioned := configsync.VersionedConfig[logging.LevelConfig]{Version: 2, Config: newConfig}
	raw, err := json.Marshal(versioned)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(overridePath, raw, 0o644); err != nil {
		t.Fatalf("simulating kubelet volume sync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if replicaWatcher.Current().Level == "debug" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica watcher did not observe the update via NATS within the deadline, current=%q", replicaWatcher.Current().Level)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Stale-version case.
	staleResult, err := updateLogLevel(ctx, adminSession, configsync.VersionedConfig[logging.LevelConfig]{
		Version: 1, // stale - the config map is actually at version 2
		Config:  logging.LevelConfig{Level: "error"},
	})
	if err != nil {
		t.Fatalf("stale update transport error: %v", err)
	}
	if !staleResult.IsError {
		t.Fatal("expected a stale-version update to be rejected")
	}
	if got := logging.CurrentLevel(); got != "debug" {
		t.Errorf("expected the rejected stale update to leave the level unchanged at debug, got %q", got)
	}

	finalExported, err := exportLogLevel(ctx, adminSession)
	if err != nil {
		t.Fatalf("final export failed: %v", err)
	}
	if finalExported.Version != 2 {
		t.Errorf("expected the rejected stale update to leave the config map at version 2, got %d", finalExported.Version)
	}
	if finalExported.Config.Level != "debug" {
		t.Errorf("expected the rejected stale update's content to never have been applied, got %+v", finalExported.Config)
	}
}

// bytesContainsVersionKey reports whether a JSON document has a "version"
// key at any level - used to prove export_full_config never leaves a
// section wrapped in its usual {"version":N,"config":{...}} envelope.
func bytesContainsVersionKey(raw []byte) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return anyKeyIsVersion(v)
}

func anyKeyIsVersion(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			if k == "version" {
				return true
			}
			if anyKeyIsVersion(vv) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if anyKeyIsVersion(vv) {
				return true
			}
		}
	}
	return false
}

const (
	ADMIN_VALIDATION_TEST_PROXY_PORT = ":8121"
	ADMIN_VALIDATION_TEST_PROXY_URL  = "http://localhost:8121/mcp"
)

// TestAdminUpdatesAreValidated checks that an _admin.update_<section> call
// whose config wouldn't load is refused before anything is written, so it
// can neither weaken the running gateway nor leave it unable to restart.
func TestAdminUpdatesAreValidated(t *testing.T) {
	natsURL := startTestNATSServer(t)
	cfgPath := filepath.Join(t.TempDir(), "admin-validation-test.cfg")
	cfg := fmt.Sprintf(`{
  "discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": { "publicKeyPath": "./public.pem", "jwksUrl": "", "issuer": "", "audience": "" },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, natsURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTestProxy(t, ctx, cancel, cfgPath, ADMIN_VALIDATION_TEST_PROXY_PORT)

	session, err := connectAsRole(ctx, ADMIN_VALIDATION_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer session.Close()

	exportVersion := func(section string) float64 {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "_admin.export_" + section, Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("export_%s: err=%v result=%v", section, err, res)
		}
		return res.StructuredContent.(map[string]any)["version"].(float64)
	}

	for section, invalid := range map[string]any{
		"orchestration": map[string]any{
			"orchestrations": map[string]any{},
			"toolMappings":   []any{map[string]any{"pattern": "store.*", "orchestration": "missing"}},
		},
		"declarative_guardrails": map[string]any{
			"actions":      []any{"block"},
			"declarations": []any{map[string]any{"name": "d", "roleExpression": "((", "actions": []any{"block"}}},
		},
		"log_level": map[string]any{"level": "verbose"},
	} {
		t.Run(section, func(t *testing.T) {
			before := exportVersion(section)
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "_admin.update_" + section,
				Arguments: map[string]any{"version": before, "config": invalid},
			})
			if err == nil && !res.IsError {
				t.Fatalf("invalid %s update was accepted: %v", section, res.Content)
			}
			msg := fmt.Sprint(err)
			if res != nil {
				for _, c := range res.Content {
					if text, ok := c.(*mcp.TextContent); ok {
						msg += " " + text.Text
					}
				}
			}
			if !strings.Contains(msg, "not applied") {
				t.Errorf("invalid %s update was refused, but not by validation: %s", section, msg)
			}
			if after := exportVersion(section); after != before {
				t.Errorf("invalid %s update was written: version %v -> %v", section, before, after)
			}
		})
	}
}
