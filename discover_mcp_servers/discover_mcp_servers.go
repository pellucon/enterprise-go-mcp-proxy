// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

type DiscoveryMode string

type ServerMap map[string]*ServerAttributes

type ServerAttributes struct {
	ServerName string
	ServerUrl  string
	// CredentialsSecret optionally names a Kubernetes Secret, in go-proxy's
	// own namespace, holding the credentials go-proxy presents to this
	// server (an API key/token header, or OAuth client credentials) - see
	// the backendauth package. Empty means the server needs none. It only
	// ever comes from go-proxy's own config (a static entry's
	// credentialsSecret, or k8sCredentials), never from the backend itself.
	CredentialsSecret string
}

// serverNamePattern is what a backend server name may look like. Names are
// joined to their tools/prompts with "." and to their resources with ":",
// and split back at the first one, so a name containing either would make
// "<server>.<tool>" ambiguous and route calls to the wrong server.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ValidateServerName reports whether name can be used as a backend server
// name: non-empty, starting with a letter or digit, and made of letters,
// digits, '-' and '_' only.
func ValidateServerName(name string) error {
	if !serverNamePattern.MatchString(name) {
		return fmt.Errorf("invalid server name %q: must match %s", name, serverNamePattern)
	}
	return nil
}

// validateServiceRef checks a "<namespace>/<service>" reference, as used by
// k8sCredentials and k8sPinnedServers.
func validateServiceRef(ref string) error {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("%q must have the form <namespace>/<service>", ref)
	}
	return nil
}

const (
	ModeScan   DiscoveryMode = "scan"
	ModeStatic DiscoveryMode = "static"
	ModeK8s    DiscoveryMode = "k8s"
)

// defaultK8sLabelSelector is used when a "k8s" mode config omits
// k8sLabelSelector: it matches Services carrying mcp.io/role=backend-mcp-server,
// the label that marks a Service as a discoverable MCP backend (see
// k8s_discovery.go).
const defaultK8sLabelSelector = "mcp.io/role=backend-mcp-server"

type MCPDiscoveryConfig struct {
	Mode                                  DiscoveryMode
	FrequencySeconds                      int
	ToolsPollFrequencySeconds             int
	PromptsPollFrequencySeconds           int
	ResourcesPollFrequencySeconds         int
	ResourceTemplatesPollFrequencySeconds int
	ServerMap                             ServerMap // populated if Mode == ModeStatic
	// K8sNamespaces lists the namespaces k8s discovery looks in, in
	// priority order: when Services in more than one of them claim the same
	// server name, the one in the earliest namespace wins. Required (and
	// non-empty) in k8s mode - discovery never lists across the whole
	// cluster, since every Service it finds is trusted as a backend.
	K8sNamespaces    []string // used if Mode == ModeK8s
	K8sLabelSelector string   // used if Mode == ModeK8s
	// K8sCredentials maps "<namespace>/<service>" to the name of the
	// credentials Secret (see ServerAttributes.CredentialsSecret) go-proxy
	// presents to that Service. Keyed by the Service's real identity, not
	// its server name, which a Service can claim through an annotation.
	K8sCredentials map[string]string // used if Mode == ModeK8s
	// K8sPinnedServers maps a server name to the "<namespace>/<service>"
	// that alone may provide it - e.g. the platform servers orchestration
	// and rate limiting depend on. A pinned name is never accepted from
	// any other Service, even while the pinned one is missing.
	K8sPinnedServers map[string]string // used if Mode == ModeK8s
	// ListedTools is a mandatory, global (not per-role) allowlist of glob
	// patterns (access_control.GlobMatch semantics - "*" matches any
	// characters) gating what a tools/list result may ever include,
	// applied on top of (never instead of) each caller's own role-based
	// authorization: a tool must be BOTH authorized for the caller AND
	// matched by at least one pattern here to be listed. An empty (but
	// present) list means no tool is ever listed; a caller can still call
	// an unlisted tool directly, or via proxy_utility.call_tool, if
	// they're otherwise authorized for it - this only ever narrows
	// tools/list, never CallTool authorization. See main.go's filterTools.
	ListedTools []string
}

type jsonConfig struct {
	DiscoveryMode                         string            `json:"discoveryMode"`
	FrequencySeconds                      *int              `json:"frequencySeconds,omitempty"`
	ToolsPollFrequencySeconds             *int              `json:"toolsPollFrequencySeconds,omitempty"`
	PromptsPollFrequencySeconds           *int              `json:"promptsPollFrequencySeconds,omitempty"`
	ResourcesPollFrequencySeconds         *int              `json:"resourcesPollFrequencySeconds,omitempty"`
	ResourceTemplatesPollFrequencySeconds *int              `json:"resourceTemplatesPollFrequencySeconds,omitempty"`
	Servers                               []jsonServer      `json:"servers,omitempty"`
	K8sNamespaces                         []string          `json:"k8sNamespaces,omitempty"`
	K8sLabelSelector                      *string           `json:"k8sLabelSelector,omitempty"`
	K8sCredentials                        map[string]string `json:"k8sCredentials,omitempty"`
	K8sPinnedServers                      map[string]string `json:"k8sPinnedServers,omitempty"`
	// ListedTools is a pointer here (unlike everywhere else that's just
	// "optional") specifically so ParseConfig can tell "the key was never
	// given at all" (nil - a config-authoring error, since this field is
	// mandatory) apart from "the key was given as an empty array" (a
	// non-nil pointer to a zero-length slice - a valid, very restrictive
	// value, see MCPDiscoveryConfig.ListedTools's own doc comment).
	ListedTools *[]string `json:"listedTools"`
}

type jsonServer struct {
	ServerName        string `json:"serverName"`
	ServerUrl         string `json:"serverUrl"`
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// UnmarshalJSON delegates to ParseConfig, so MCPDiscoveryConfig accepts
// exactly the discoveryMode/servers/... wire shape a real proxy-server.cfg
// "discovery" section uses (and the same validation) wherever it's decoded
// via the standard encoding/json path - notably _admin.update_discovery's
// typed tool input (admin_tools.go), which has no other route to
// ParseConfig. Without this, MCPDiscoveryConfig's un-tagged fields would
// fall back to encoding/json's default (Go field name as JSON key, e.g.
// "Mode"/"ServerMap") - internally self-consistent with MarshalJSON below,
// but incompatible with every real config file and with ParseConfig itself.
func (c *MCPDiscoveryConfig) UnmarshalJSON(data []byte) error {
	parsed, err := ParseConfig(data)
	if err != nil {
		return err
	}
	*c = *parsed
	return nil
}

// MarshalJSON is UnmarshalJSON's inverse: produces the same wire shape
// ParseConfig accepts, rather than encoding/json's default (see
// UnmarshalJSON's doc comment) - so _admin.export_discovery's output (and
// _admin.export_full_config's discovery section) is both valid input for
// _admin.update_discovery and a valid "discovery" section for a real
// proxy-server.cfg file, not just internally self-consistent with itself.
// Only fields relevant to c.Mode are emitted, matching how a hand-written
// config would look (e.g. static mode never emits k8sNamespaces). Servers
// are sorted by name for deterministic output - ServerMap is a Go map,
// whose iteration order is randomized.
func (c MCPDiscoveryConfig) MarshalJSON() ([]byte, error) {
	listedTools := c.ListedTools
	if listedTools == nil {
		// ListedTools is mandatory (ParseConfig rejects a missing key), so
		// this always emits a real array - never the bare `null` a nil
		// slice would otherwise marshal to - even for a zero-value/
		// never-configured MCPDiscoveryConfig.
		listedTools = []string{}
	}
	raw := jsonConfig{DiscoveryMode: string(c.Mode), ListedTools: &listedTools}
	if c.ToolsPollFrequencySeconds != 0 {
		raw.ToolsPollFrequencySeconds = &c.ToolsPollFrequencySeconds
	}
	if c.PromptsPollFrequencySeconds != 0 {
		raw.PromptsPollFrequencySeconds = &c.PromptsPollFrequencySeconds
	}
	if c.ResourcesPollFrequencySeconds != 0 {
		raw.ResourcesPollFrequencySeconds = &c.ResourcesPollFrequencySeconds
	}
	if c.ResourceTemplatesPollFrequencySeconds != 0 {
		raw.ResourceTemplatesPollFrequencySeconds = &c.ResourceTemplatesPollFrequencySeconds
	}

	switch c.Mode {
	case ModeScan:
		if c.FrequencySeconds != 0 {
			raw.FrequencySeconds = &c.FrequencySeconds
		}
	case ModeK8s:
		if c.FrequencySeconds != 0 {
			raw.FrequencySeconds = &c.FrequencySeconds
		}
		raw.K8sNamespaces = c.K8sNamespaces
		if c.K8sLabelSelector != "" && c.K8sLabelSelector != defaultK8sLabelSelector {
			raw.K8sLabelSelector = &c.K8sLabelSelector
		}
		raw.K8sCredentials = c.K8sCredentials
		raw.K8sPinnedServers = c.K8sPinnedServers
	case ModeStatic:
		raw.Servers = make([]jsonServer, 0, len(c.ServerMap))
		for _, s := range c.ServerMap {
			raw.Servers = append(raw.Servers, jsonServer{ServerName: s.ServerName, ServerUrl: s.ServerUrl, CredentialsSecret: s.CredentialsSecret})
		}
		sort.Slice(raw.Servers, func(i, j int) bool { return raw.Servers[i].ServerName < raw.Servers[j].ServerName })
	}

	return json.Marshal(raw)
}

// DiscoverMCPs reads and parses filePath, then discovers servers per its
// configured mode. See DiscoverMCPsFromConfig for the mode-dispatch logic
// on an already-parsed config (e.g. one extracted from a larger combined
// config file).
func DiscoverMCPs(filePath string) (ServerMap, error) {
	config, err := ParseConfigFile(filePath)
	if err != nil {
		return nil, err
	}
	return DiscoverMCPsFromConfig(config, nil)
}

// DiscoverMCPsFromConfig discovers servers per an already-parsed config's
// mode. Split out of DiscoverMCPs so a caller that already has a parsed
// *MCPDiscoveryConfig (e.g. main.go's combined-config loader) doesn't need
// to round-trip through a discovery-only file on disk. In k8s mode, a name
// in mustPin is accepted only from the Service k8sPinnedServers pins it to
// (see MustPin).
func DiscoverMCPsFromConfig(config *MCPDiscoveryConfig, mustPin MustPin) (ServerMap, error) {
	switch config.Mode {
	case ModeScan:
		return discoverMCPServers()
	case ModeStatic:
		return config.ServerMap, nil
	case ModeK8s:
		return discoverK8sServers(config, mustPin)
	default:
		return nil, fmt.Errorf("Bad mode %v", config.Mode)
	}
}

// MustPin maps the server names go-proxy trusts, by name alone, for a
// security decision - a guardrail's verdict, a rate-limit decision, a relay
// grant - to why each is trusted. In k8s mode any Service in a listed
// namespace can claim any server name, so such a name is accepted only from
// the Service k8sPinnedServers pins it to: an unpinned one is never
// discovered (it's skipped and logged), and the decision that depends on it
// fails closed as if its server were unreachable. Static mode's names come
// from the config itself and need no pin.
type MustPin map[string]string

// UnpinnedNames reports, for a k8s-mode config, each name in mustPin that
// k8sPinnedServers doesn't pin, as an error naming them and why each must
// be pinned. Other modes always pass.
func UnpinnedNames(config *MCPDiscoveryConfig, mustPin MustPin) error {
	if config.Mode != ModeK8s {
		return nil
	}
	var missing []string
	for name, why := range mustPin {
		if _, ok := config.K8sPinnedServers[name]; !ok {
			missing = append(missing, fmt.Sprintf("%q (%s)", name, why))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("k8sPinnedServers must pin every server go-proxy trusts for a security decision, but doesn't pin %s", strings.Join(missing, ", "))
}

// ParseConfigFile reads filePath and parses it. See ParseConfig for the
// parsing logic on already-read bytes.
func ParseConfigFile(filePath string) (*MCPDiscoveryConfig, error) {
	// Read the raw file bytes
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return ParseConfig(fileData)
}

// ParseConfig parses already-read discovery-config JSON bytes and
// structurally validates the result (see validate) before returning it —
// a malformed static server entry fails the load outright rather than
// silently vanishing from ServerMap, so a bad _admin.update_discovery call
// is rejected rather than accepted with a silently missing server. Split
// out of ParseConfigFile so a caller extracting this section from a larger
// combined config file (e.g. main.go's combined-config loader) can parse it
// directly, without writing it back out to a temporary file first.
func ParseConfig(data []byte) (*MCPDiscoveryConfig, error) {
	// Parse raw JSON into our exposed JSON helper struct
	var raw jsonConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid json format: %w", err)
	}

	// listedTools is mandatory (see MCPDiscoveryConfig.ListedTools) -
	// checked before anything else, regardless of discovery mode, the same
	// way servers[i]'s required fields are checked below for "static" mode.
	if raw.ListedTools == nil {
		return nil, fmt.Errorf("listedTools is required (use [] to list no tools, or [\"*\"] to list everything the caller is otherwise authorized for)")
	}

	// Build the runtime config object
	config := &MCPDiscoveryConfig{
		ServerMap:   make(ServerMap),
		ListedTools: *raw.ListedTools,
	}

	// Parsed unconditionally (regardless of discovery mode), since tool-list
	// polling applies to any already-known backend server.
	if raw.ToolsPollFrequencySeconds != nil {
		config.ToolsPollFrequencySeconds = *raw.ToolsPollFrequencySeconds
	}

	// Parsed unconditionally (regardless of discovery mode), since
	// prompt-list polling applies to any already-known backend server,
	// symmetric to ToolsPollFrequencySeconds above.
	if raw.PromptsPollFrequencySeconds != nil {
		config.PromptsPollFrequencySeconds = *raw.PromptsPollFrequencySeconds
	}

	// Parsed unconditionally (regardless of discovery mode), since
	// resource-list polling applies to any already-known backend server,
	// symmetric to ToolsPollFrequencySeconds/PromptsPollFrequencySeconds above.
	if raw.ResourcesPollFrequencySeconds != nil {
		config.ResourcesPollFrequencySeconds = *raw.ResourcesPollFrequencySeconds
	}

	// Parsed unconditionally (regardless of discovery mode), since
	// resource-template-list polling applies to any already-known backend
	// server, symmetric to the other PollFrequencySeconds fields above.
	if raw.ResourceTemplatesPollFrequencySeconds != nil {
		config.ResourceTemplatesPollFrequencySeconds = *raw.ResourceTemplatesPollFrequencySeconds
	}

	// Map strings safely to explicit enums
	switch raw.DiscoveryMode {
	case "scan":
		config.Mode = ModeScan
		if raw.FrequencySeconds != nil {
			config.FrequencySeconds = *raw.FrequencySeconds
		}
	case "static":
		config.Mode = ModeStatic

		// Populate ServerMap with pointers matching the key to the name -
		// every entry must be complete; a malformed one fails the whole
		// parse (see ParseConfig's doc comment for why this changed from
		// silently skipping).
		for i, s := range raw.Servers {
			if s.ServerName == "" {
				return nil, fmt.Errorf("servers[%d]: serverName must not be empty", i)
			}
			if err := ValidateServerName(s.ServerName); err != nil {
				return nil, fmt.Errorf("servers[%d]: %w", i, err)
			}
			if s.ServerUrl == "" {
				return nil, fmt.Errorf("servers[%d] (%q): serverUrl must not be empty", i, s.ServerName)
			}
			if _, exists := config.ServerMap[s.ServerName]; exists {
				return nil, fmt.Errorf("servers[%d]: duplicate serverName %q", i, s.ServerName)
			}

			config.ServerMap[s.ServerName] = &ServerAttributes{
				ServerName:        s.ServerName,
				ServerUrl:         s.ServerUrl,
				CredentialsSecret: s.CredentialsSecret,
			}
		}
	case "k8s":
		config.Mode = ModeK8s
		if raw.FrequencySeconds != nil {
			config.FrequencySeconds = *raw.FrequencySeconds
		}
		if len(raw.K8sNamespaces) == 0 {
			return nil, fmt.Errorf("k8sNamespaces is required in k8s mode: list the namespaces to discover backends in (discovery never spans the whole cluster)")
		}
		seen := make(map[string]bool, len(raw.K8sNamespaces))
		for i, ns := range raw.K8sNamespaces {
			if strings.TrimSpace(ns) == "" {
				return nil, fmt.Errorf("k8sNamespaces[%d] must not be empty", i)
			}
			if seen[ns] {
				return nil, fmt.Errorf("k8sNamespaces[%d]: duplicate namespace %q", i, ns)
			}
			seen[ns] = true
		}
		config.K8sNamespaces = raw.K8sNamespaces
		config.K8sLabelSelector = defaultK8sLabelSelector
		if raw.K8sLabelSelector != nil && *raw.K8sLabelSelector != "" {
			config.K8sLabelSelector = *raw.K8sLabelSelector
		}
		for ref, secret := range raw.K8sCredentials {
			if err := validateServiceRef(ref); err != nil {
				return nil, fmt.Errorf("k8sCredentials: %w", err)
			}
			if secret == "" {
				return nil, fmt.Errorf("k8sCredentials[%q]: secret name must not be empty", ref)
			}
		}
		config.K8sCredentials = raw.K8sCredentials
		for name, ref := range raw.K8sPinnedServers {
			if err := ValidateServerName(name); err != nil {
				return nil, fmt.Errorf("k8sPinnedServers: %w", err)
			}
			if err := validateServiceRef(ref); err != nil {
				return nil, fmt.Errorf("k8sPinnedServers[%q]: %w", name, err)
			}
		}
		config.K8sPinnedServers = raw.K8sPinnedServers
	default:
		return nil, fmt.Errorf("unknown discovery mode: %s (expected 'scan', 'static', or 'k8s')", raw.DiscoveryMode)
	}

	return config, nil
}

// checkScanProcess decides whether scan discovery trusts a process claiming
// a server: there are no pins in scan mode, and any local user can start a
// process with any command line, so only go-proxy's own user's processes
// count (every one of uids, the process's user IDs, must be selfUID), and
// only with a loopback URL, so a claim can't send calls off the machine.
func checkScanProcess(uids []uint32, selfUID int, rawURL string) error {
	if len(uids) == 0 {
		return fmt.Errorf("can't read its owner")
	}
	for _, uid := range uids {
		if int64(uid) != int64(selfUID) {
			return fmt.Errorf("it's run by user %d, not go-proxy's user %d", uid, selfUID)
		}
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("--mcpServerUrl %q isn't an http(s) URL", rawURL)
	}
	if host := u.Hostname(); host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("--mcpServerUrl %q isn't on this machine (localhost or a loopback address)", rawURL)
		}
	}
	return nil
}

func discoverMCPServers() (ServerMap, error) {
	var problems problemList
	defer func() { scanProblems.report(problems) }()

	serverMap := make(ServerMap)

	processes, err := process.Processes()
	if err != nil {
		return nil, err
	}

	for _, p := range processes {

		args, err := p.CmdlineSlice()
		if err != nil {
			continue // Skip permission-protected or dead processes
		}

		var name, serverURL string
		isMCPServer := false

		for _, arg := range args {
			if strings.HasPrefix(arg, "--mcpServerName=") {
				name = strings.TrimPrefix(arg, "--mcpServerName=")
				isMCPServer = true
			}
			if strings.HasPrefix(arg, "--mcpServerUrl=") {
				serverURL = strings.TrimPrefix(arg, "--mcpServerUrl=")
			}
		}

		if isMCPServer {
			if err := ValidateServerName(name); err != nil {
				problems.errorf("scan discovery: skipping process %d: %v", p.Pid, err)
				continue
			}
			uids, err := p.Uids()
			if err != nil {
				problems.errorf("scan discovery: skipping process %d: can't read its owner: %v", p.Pid, err)
				continue
			}
			if err := checkScanProcess(uids, os.Geteuid(), serverURL); err != nil {
				problems.errorf("scan discovery: skipping process %d (%s): %v", p.Pid, name, err)
				continue
			}
			if _, exists := serverMap[name]; exists {
				problems.errorf("scan discovery: skipping process %d: server name %q is already claimed by another process", p.Pid, name)
				continue
			}
			server := &ServerAttributes{
				ServerName: name,
				ServerUrl:  serverURL,
			}
			serverMap[name] = server

		}
	}

	return serverMap, nil
}
