// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import "context"

// NativeCallAuthorizationFunc is the in-process equivalent of a
// call-authorization triggered tool's MCP handler: same input/output
// contract (CallAuthorizationInput/AuthorizationOutput, see contract.go),
// but invoked as a plain Go function call instead of over the wire via
// mcp.ClientSession.CallTool. See RegisterNativeCallAuthorization.
type NativeCallAuthorizationFunc func(ctx context.Context, in CallAuthorizationInput) (AuthorizationOutput, error)

// NativeResultAuthorizationFunc mirrors NativeCallAuthorizationFunc for the
// result-authorization contract.
type NativeResultAuthorizationFunc func(ctx context.Context, in ResultAuthorizationInput) (AuthorizationOutput, error)

// RegisterNativeCallAuthorization installs an in-process call-authorization
// step handler, reachable from an orchestration's callAuthorization list via
// an OrchestrationStep whose Server is dynamic_proxy.UtilityServerName
// ("proxy_utility") and whose Tool is name — see invokeCallAuthorization in
// orchestrate.go. Unlike a step naming a real discovered backend, a native
// step is never dialed over MCP/HTTP: there is no client session, no wire
// format, and so no auth header to attach — the call is a direct in-process
// Go function invocation, which is why this mechanism exists at all (see
// CLAUDE.md's "Triggered tools / orchestration" section for the
// full rationale). Mutex-guarded; intended to be called a fixed number of
// times at proxy startup (main.go, right after NewRuntime), not per
// request. Registering the same name twice overwrites the previous
// registration (last wins).
func (r *Runtime) RegisterNativeCallAuthorization(name string, fn NativeCallAuthorizationFunc) {
	r.nativeMu.Lock()
	defer r.nativeMu.Unlock()
	r.nativeCallAuthorizations[name] = fn
}

// RegisterNativeResultAuthorization mirrors RegisterNativeCallAuthorization
// for the result-authorization contract.
func (r *Runtime) RegisterNativeResultAuthorization(name string, fn NativeResultAuthorizationFunc) {
	r.nativeMu.Lock()
	defer r.nativeMu.Unlock()
	r.nativeResultAuthorizations[name] = fn
}

// NativeToolInfo describes one native triggered tool for enumeration
// purposes — see NativeTools. A bare name alone isn't enough to build a
// meaningful synthesized Tool object for it (go-proxy's own
// "proxy_utility.list_triggered_tools" tool, main.go's
// registerUtilityTools): StepType is what determines which of the two
// fixed contracts (contract.go) — and so which InputSchema — applies.
type NativeToolInfo struct {
	Name string
	// StepType is StepTypeCallAuthorization or StepTypeResultAuthorization
	// (config.go) — whichever RegisterNative*Authorization call registered
	// Name. Pass directly to InputSchemaFor.
	StepType string
}

// NativeTools returns every native call-/result-authorization function
// currently registered via RegisterNativeCallAuthorization/
// RegisterNativeResultAuthorization, each tagged with which contract it
// implements. A name registered under both contracts (unusual, but not
// disallowed) appears twice, once per contract — see NativeToolInfo.
func (r *Runtime) NativeTools() []NativeToolInfo {
	r.nativeMu.RLock()
	defer r.nativeMu.RUnlock()
	infos := make([]NativeToolInfo, 0, len(r.nativeCallAuthorizations)+len(r.nativeResultAuthorizations))
	for name := range r.nativeCallAuthorizations {
		infos = append(infos, NativeToolInfo{Name: name, StepType: StepTypeCallAuthorization})
	}
	for name := range r.nativeResultAuthorizations {
		infos = append(infos, NativeToolInfo{Name: name, StepType: StepTypeResultAuthorization})
	}
	return infos
}

func (r *Runtime) nativeCallAuthorization(name string) (NativeCallAuthorizationFunc, bool) {
	r.nativeMu.RLock()
	defer r.nativeMu.RUnlock()
	fn, ok := r.nativeCallAuthorizations[name]
	return fn, ok
}

func (r *Runtime) nativeResultAuthorization(name string) (NativeResultAuthorizationFunc, bool) {
	r.nativeMu.RLock()
	defer r.nativeMu.RUnlock()
	fn, ok := r.nativeResultAuthorizations[name]
	return fn, ok
}
