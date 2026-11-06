// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// stressNamespaces is every namespace this driver's setup phase deploys
// into and therefore must health-check afterward — mcp-proxy (go-proxy +
// the 3 platform-mcp-servers), gardening-demo (the 5 garden-* backends),
// and demo-auth (demo-authorization-server). Matches CLAUDE.md's
// "Namespace layout" fixed three; ai-mcp-servers/ai-powered-tools is
// deliberately out of scope (not deployed or exercised by this stress
// test).
var stressNamespaces = []string{"mcp-proxy", "gardening-demo", "demo-auth"}

// PodStatus is one pod's health-relevant fields, snapshotted before and
// after a run so podhealth.go can diff them.
type PodStatus struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
}

// snapshotPods runs `kubectl get pods -n <ns> -o json` for every stress
// namespace and flattens the result into one sorted []PodStatus — sorted
// so pods-baseline.json/pods-final.json are stable/diffable byte-for-byte
// across runs when nothing actually changed.
func snapshotPods() ([]PodStatus, error) {
	var all []PodStatus
	for _, ns := range stressNamespaces {
		out, err := exec.Command("kubectl", "get", "pods", "-n", ns, "-o", "json").Output()
		if err != nil {
			return nil, fmt.Errorf("kubectl get pods -n %s: %w", ns, err)
		}
		var list corev1.PodList
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, fmt.Errorf("parsing pod list for namespace %s: %w", ns, err)
		}
		for _, pod := range list.Items {
			ready := pod.Status.Phase == corev1.PodRunning
			var restarts int32
			for _, cs := range pod.Status.ContainerStatuses {
				restarts += cs.RestartCount
				if !cs.Ready {
					ready = false
				}
			}
			all = append(all, PodStatus{
				Namespace:    ns,
				Name:         pod.Name,
				Phase:        string(pod.Status.Phase),
				Ready:        ready,
				RestartCount: restarts,
			})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Namespace != all[j].Namespace {
			return all[i].Namespace < all[j].Namespace
		}
		return all[i].Name < all[j].Name
	})
	return all, nil
}

// HealthFinding is one concrete problem podhealth.Check found, in the
// final pod-health verdict.
type HealthFinding struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Problem   string `json:"problem"`
}

// checkPodHealth diffs a post-setup baseline snapshot against a teardown
// snapshot, per README.md's health-check rules:
//   - a pod present in the baseline but missing from final (or vice
//     versa) — a name-set diff, since a crash-and-recreate under a
//     Deployment can produce an entirely new pod name even with a fresh
//     "0" restart count on the replacement pod, which a restart-count-only
//     diff would miss entirely
//   - a pod whose restartCount increased beyond its own baseline value
//   - a pod that isn't Running/Ready at teardown time
//
// The baseline is NOT expected to be all-zero restart counts: the
// driver's own setup phase (scripts/deploy.sh) legitimately restarts
// every pre-existing Deployment before reseeding, so the baseline
// snapshot is taken AFTER that, right before the client-run phase starts.
func checkPodHealth(baseline, final []PodStatus) []HealthFinding {
	var findings []HealthFinding

	baselineByKey := make(map[string]PodStatus, len(baseline))
	for _, p := range baseline {
		baselineByKey[p.Namespace+"/"+p.Name] = p
	}
	finalByKey := make(map[string]PodStatus, len(final))
	for _, p := range final {
		finalByKey[p.Namespace+"/"+p.Name] = p
	}

	for key, b := range baselineByKey {
		f, ok := finalByKey[key]
		if !ok {
			findings = append(findings, HealthFinding{
				Namespace: b.Namespace, Pod: b.Name,
				Problem: "present at setup baseline but missing at teardown (crashed and was recreated under a new pod name, or the Deployment was scaled down)",
			})
			continue
		}
		if f.RestartCount > b.RestartCount {
			findings = append(findings, HealthFinding{
				Namespace: b.Namespace, Pod: b.Name,
				Problem: fmt.Sprintf("restart count increased during the run: %d -> %d", b.RestartCount, f.RestartCount),
			})
		}
		if !f.Ready || f.Phase != "Running" {
			findings = append(findings, HealthFinding{
				Namespace: f.Namespace, Pod: f.Name,
				Problem: fmt.Sprintf("not healthy at teardown: phase=%s ready=%v", f.Phase, f.Ready),
			})
		}
	}
	for key, f := range finalByKey {
		if _, ok := baselineByKey[key]; !ok {
			findings = append(findings, HealthFinding{
				Namespace: f.Namespace, Pod: f.Name,
				Problem: "present at teardown but was not part of the setup baseline (an unexpected pod appeared during the run)",
			})
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Namespace != findings[j].Namespace {
			return findings[i].Namespace < findings[j].Namespace
		}
		return findings[i].Pod < findings[j].Pod
	})
	return findings
}
