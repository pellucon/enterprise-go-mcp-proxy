// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"errors"
	"reflect"
	"testing"
)

func recordingProblemLog(logged *[]string) *problemLog {
	return &problemLog{logf: func(msg string) { *logged = append(*logged, msg) }}
}

func problemsOf(msgs ...string) problemList {
	var l problemList
	for _, m := range msgs {
		l.errorf("%s", m)
	}
	return l
}

// TestProblemLog_LogsEachProblemOnceWhilePersisting checks that a problem
// found on every discovery pass is logged once, and again only after it
// has cleared and come back.
func TestProblemLog_LogsEachProblemOnceWhilePersisting(t *testing.T) {
	var logged []string
	p := recordingProblemLog(&logged)

	steps := []struct {
		report problemList
		want   []string
	}{
		{problemsOf("b", "a"), []string{"a", "b"}},
		{problemsOf("a", "b"), nil},
		{problemsOf("a", "a"), nil},
		{problemsOf("a", "b"), []string{"b"}},
		{nil, nil},
		{problemsOf("a"), []string{"a"}},
	}
	for i, step := range steps {
		logged = nil
		p.report(step.report)
		if !reflect.DeepEqual(logged, step.want) {
			t.Errorf("step %d: logged %q, want %q", i, logged, step.want)
		}
	}
}

func TestProblemLog_Active(t *testing.T) {
	var logged []string
	p := recordingProblemLog(&logged)
	if p.active() {
		t.Fatal("active before any report")
	}
	p.report(problemsOf("a"))
	if !p.active() {
		t.Fatal("not active after a problem was reported")
	}
	p.report(nil)
	if p.active() {
		t.Fatal("still active after an empty report")
	}
}

// TestMonitorLogsRepeatedFailureOnce checks that a discovery error that
// repeats on every tick (e.g. a namespace go-proxy may not list) is logged
// once, and again after discovery has recovered and failed anew.
func TestMonitorLogsRepeatedFailureOnce(t *testing.T) {
	fail := errors.New("forbidden")
	var discoverErr error = fail
	sm := NewServerMonitor(60, func() (ServerMap, error) {
		if discoverErr != nil {
			return nil, discoverErr
		}
		return ServerMap{}, nil
	}, func(ServerMap) {}, func(ServerMap) {})
	var logged []string
	sm.failures.logf = func(msg string) { logged = append(logged, msg) }

	sm.checkAndDiff()
	sm.checkAndDiff()
	sm.checkAndDiff()
	if len(logged) != 1 {
		t.Fatalf("expected the failure to be logged once over 3 ticks, got %q", logged)
	}

	discoverErr = nil
	sm.checkAndDiff()
	discoverErr = fail
	sm.checkAndDiff()
	if len(logged) != 2 {
		t.Fatalf("expected the failure to be logged again after recovering, got %q", logged)
	}
}
