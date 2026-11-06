// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var cycle int
var cancel context.CancelFunc
var serversAdded atomic.Int32
var serversDeleted atomic.Int32

const serversAddedTarget int = 3
const serversDeletedTarget int = 1

func TestMonitoring(t *testing.T) {

	ctx, c := context.WithCancel(context.Background())
	cancel = c
	monitor := NewServerMonitor(1, getCurrentRunningServers, monitorAdded, monitorDeleted)
	monitor.Start(ctx)

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			serversAdded := serversAdded.Load()
			serversDeleted := serversDeleted.Load()
			t.Fatalf("Timeout reached! Expected (A:%d, B:%d) but got (A:%d, B:%d)",
				serversAddedTarget, serversDeletedTarget, serversAdded, serversDeleted)

		case <-ticker.C:
			serversAdded := serversAdded.Load()
			serversDeleted := serversDeleted.Load()

			if serversAdded == int32(serversAddedTarget) && serversDeleted == int32(serversDeletedTarget) {
				t.Logf("Success! Counters reached targets")
				return
			}
		}
	}
}

func monitorAdded(s ServerMap) {
	added := int32(len(s))
	serversAdded.Add(added)
}
func monitorDeleted(s ServerMap) {
	deleted := int32(len(s))
	serversDeleted.Add(deleted)
}

func getCurrentRunningServers() (ServerMap, error) {
	cycle++
	if cycle == 4 {
		time.After(time.Duration(2))
		cancel()
		return createConfig3().ServerMap, nil
	}
	switch cycle {
	case 1:
		return createConfig1().ServerMap, nil
	case 2:
		return createConfig2().ServerMap, nil
	case 3:
		return createConfig3().ServerMap, nil
	default:
		return createConfig3().ServerMap, nil
	}

}

func createConfig1() MCPDiscoveryConfig {
	config := MCPDiscoveryConfig{
		Mode: ModeStatic,
		ServerMap: ServerMap{
			"mcp1": &ServerAttributes{
				ServerName: "mcp1",
				ServerUrl:  "http://localhost:8081",
			},
			"mcp2": &ServerAttributes{
				ServerName: "mcp2",
				ServerUrl:  "http://localhost:8082",
			},
		},
	}

	return config
}

func createConfig2() MCPDiscoveryConfig {
	config := MCPDiscoveryConfig{
		Mode: ModeStatic,
		ServerMap: ServerMap{
			"mcp1": &ServerAttributes{
				ServerName: "mcp1",
				ServerUrl:  "http://localhost:8081",
			},
		},
	}

	return config
}

func createConfig3() MCPDiscoveryConfig {
	config := MCPDiscoveryConfig{
		Mode: ModeStatic,
		ServerMap: ServerMap{
			"mcp1": &ServerAttributes{
				ServerName: "mcp1",
				ServerUrl:  "http://localhost:8081",
			},
			"mcp2": &ServerAttributes{
				ServerName: "mcp2",
				ServerUrl:  "http://localhost:8082",
			},
		},
	}

	return config
}

func TestMonitorFirstCheckDone(t *testing.T) {
	release := make(chan struct{})
	sm := NewServerMonitor(60, func() (ServerMap, error) {
		<-release
		return ServerMap{}, nil
	}, func(ServerMap) {}, func(ServerMap) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Start(ctx)

	time.Sleep(20 * time.Millisecond)
	if sm.FirstCheckDone() {
		t.Fatal("FirstCheckDone before the first discovery returned")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for !sm.FirstCheckDone() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !sm.FirstCheckDone() {
		t.Fatal("FirstCheckDone not set after the first check")
	}
}

// TestMonitorChangedServerIsRemovedThenAdded checks that a server whose
// URL or credentials change under the same name is reported as removed
// (with its old attributes) and then added (with its new ones), in that
// order, so callers tear down connections to the old address.
func TestMonitorChangedServerIsRemovedThenAdded(t *testing.T) {
	current := ServerMap{
		"crm":   {ServerName: "crm", ServerUrl: "http://crm-a:8080"},
		"other": {ServerName: "other", ServerUrl: "http://other:8080"},
	}
	var events []string
	sm := NewServerMonitor(60, func() (ServerMap, error) { return current, nil },
		func(s ServerMap) {
			for name, attr := range s {
				events = append(events, "added "+name+" "+attr.ServerUrl+" "+attr.CredentialsSecret)
			}
		},
		func(s ServerMap) {
			for name, attr := range s {
				events = append(events, "removed "+name+" "+attr.ServerUrl+" "+attr.CredentialsSecret)
			}
		})

	sm.checkAndDiff()
	events = nil

	current = ServerMap{
		"crm":   {ServerName: "crm", ServerUrl: "http://crm-b:8080"},
		"other": {ServerName: "other", ServerUrl: "http://other:8080", CredentialsSecret: "other-creds"},
	}
	sm.checkAndDiff()

	removedIdx := map[string]int{}
	addedIdx := map[string]int{}
	for i, e := range events {
		var verb, name string
		fmt.Sscanf(e, "%s %s", &verb, &name)
		if verb == "removed" {
			removedIdx[name] = i
		} else {
			addedIdx[name] = i
		}
	}
	want := map[string][2]string{
		"crm":   {"removed crm http://crm-a:8080 ", "added crm http://crm-b:8080 "},
		"other": {"removed other http://other:8080 ", "added other http://other:8080 other-creds"},
	}
	for name, w := range want {
		r, rok := removedIdx[name]
		a, aok := addedIdx[name]
		if !rok || !aok {
			t.Fatalf("%s: expected both a removal and an addition, got events %q", name, events)
		}
		if r > a {
			t.Errorf("%s: removal fired after addition: %q", name, events)
		}
		if events[r] != w[0] || events[a] != w[1] {
			t.Errorf("%s: got %q / %q, want %q / %q", name, events[r], events[a], w[0], w[1])
		}
	}

	events = nil
	sm.checkAndDiff()
	if len(events) != 0 {
		t.Errorf("unchanged servers fired events: %q", events)
	}
}
