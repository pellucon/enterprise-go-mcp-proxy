// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
)

func TestCustomerServiceLoadAndAddAndResolve(t *testing.T) {
	ctx := context.Background()
	cs := newCustomerService()

	_, res, err := cs.loadCustomerSentiments(ctx, nil, LoadCustomerSentimentsArgs{Sentiments: []entities.CustomerSentiment{
		{ID: "neutral", Name: "Neutral"},
	}})
	if err != nil || res.Loaded != 1 {
		t.Fatalf("loadCustomerSentiments: res=%+v err=%v", res, err)
	}

	_, res, err = cs.loadCustomerServiceCalls(ctx, nil, LoadCustomerServiceCallsArgs{Calls: []entities.CustomerServiceCall{
		{ID: "csc-01", CustomerID: "cust-0001", IssueText: "seed call"},
	}})
	if err != nil || res.Loaded != 1 {
		t.Fatalf("loadCustomerServiceCalls: res=%+v err=%v", res, err)
	}

	_, call, err := cs.addCustomerServiceCall(ctx, nil, AddCustomerServiceCallArgs{
		CustomerID: "cust-0002", HandledByStafferID: "staff-02", IssueText: "New issue", CallDate: "2026-01-01",
	})
	if err != nil {
		t.Fatalf("addCustomerServiceCall: %v", err)
	}
	// nextCallID must skip the already-loaded csc-01 and produce the next
	// free 2-digit id, csc-02.
	if call.ID != "csc-02" {
		t.Errorf("expected id csc-02, got %q", call.ID)
	}
	if call.ResolutionDate != nil {
		t.Error("expected a freshly added call to be unresolved")
	}

	_, resolved, err := cs.resolveCustomerServiceCall(ctx, nil, ResolveCustomerServiceCallArgs{ID: call.ID, ResolutionDate: "2026-01-05"})
	if err != nil {
		t.Fatalf("resolveCustomerServiceCall: %v", err)
	}
	if resolved.ResolutionDate == nil || *resolved.ResolutionDate != "2026-01-05" {
		t.Errorf("expected resolutionDate 2026-01-05, got %v", resolved.ResolutionDate)
	}

	if _, _, err := cs.resolveCustomerServiceCall(ctx, nil, ResolveCustomerServiceCallArgs{ID: "csc-does-not-exist"}); err == nil {
		t.Error("expected resolveCustomerServiceCall to fail for an unknown id")
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestCustomerServiceGetCustomerServiceCalls(t *testing.T) {
	ctx := context.Background()
	cs := newCustomerService()

	cs.calls["csc-01"] = entities.CustomerServiceCall{
		ID: "csc-01", CallDate: "2026-01-01", IssueText: "Received a damaged Spade and would like a replacement.",
		EscalatedToManager: false, ResolutionDate: strPtr("2026-01-05"), HandledByStafferID: "staff-09",
	}
	cs.calls["csc-02"] = entities.CustomerServiceCall{
		ID: "csc-02", CallDate: "2026-01-02", IssueText: "Asking about the delivery status of my order.",
		EscalatedToManager: true, ResolutionDate: nil, HandledByStafferID: "staff-10",
	}
	cs.calls["csc-03"] = entities.CustomerServiceCall{
		ID: "csc-03", CallDate: "2026-01-01", IssueText: "General question about upcoming seasonal deals.",
		EscalatedToManager: false, ResolutionDate: nil, HandledByStafferID: "staff-09",
	}

	// No filter: all 3.
	_, res, err := cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{})
	out := res.Calls
	if err != nil {
		t.Fatalf("getCustomerServiceCalls: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(out), out)
	}

	// callDate exact match.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{CallDate: "2026-01-02"})
	out = res.Calls
	if err != nil || len(out) != 1 || out[0].ID != "csc-02" {
		t.Fatalf("expected only csc-02 for callDate filter, got %+v (err %v)", out, err)
	}

	// resolved=true.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{Resolved: boolPtr(true)})
	out = res.Calls
	if err != nil || len(out) != 1 || out[0].ID != "csc-01" {
		t.Fatalf("expected only csc-01 for resolved=true, got %+v (err %v)", out, err)
	}

	// resolved=false.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{Resolved: boolPtr(false)})
	out = res.Calls
	if err != nil || len(out) != 2 {
		t.Fatalf("expected 2 unresolved calls, got %+v (err %v)", out, err)
	}

	// issueText substring, case-insensitive.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{IssueText: "SPADE"})
	out = res.Calls
	if err != nil || len(out) != 1 || out[0].ID != "csc-01" {
		t.Fatalf("expected only csc-01 for issueText filter, got %+v (err %v)", out, err)
	}

	// escalated=true.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{Escalated: boolPtr(true)})
	out = res.Calls
	if err != nil || len(out) != 1 || out[0].ID != "csc-02" {
		t.Fatalf("expected only csc-02 for escalated=true, got %+v (err %v)", out, err)
	}

	// callDate + escalated=false ANDed.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{CallDate: "2026-01-01", Escalated: boolPtr(false)})
	out = res.Calls
	if err != nil || len(out) != 2 {
		t.Fatalf("expected 2 calls for callDate+escalated=false, got %+v (err %v)", out, err)
	}

	// handledByStafferId exact match.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{HandledByStafferID: "staff-09"})
	out = res.Calls
	if err != nil || len(out) != 2 {
		t.Fatalf("expected 2 calls for handledByStafferId=staff-09, got %+v (err %v)", out, err)
	}

	// handledByStafferId + resolved=false ANDed.
	_, res, err = cs.getCustomerServiceCalls(ctx, nil, GetCustomerServiceCallsArgs{HandledByStafferID: "staff-09", Resolved: boolPtr(false)})
	out = res.Calls
	if err != nil || len(out) != 1 || out[0].ID != "csc-03" {
		t.Fatalf("expected only csc-03 for handledByStafferId=staff-09+resolved=false, got %+v (err %v)", out, err)
	}
}

func TestCustomerServiceNextCallIDAvoidsCollisions(t *testing.T) {
	cs := newCustomerService()
	// Simulate a gap: csc-01 and csc-03 exist but not csc-02 - nextCallID
	// must find and use the actual gap, csc-02, not blindly use
	// len(calls)+1 = 3 (which would collide with csc-03).
	cs.calls["csc-01"] = entities.CustomerServiceCall{ID: "csc-01"}
	cs.calls["csc-03"] = entities.CustomerServiceCall{ID: "csc-03"}
	got := cs.nextCallID()
	if got != "csc-02" {
		t.Errorf("expected csc-02, got %q", got)
	}
}
