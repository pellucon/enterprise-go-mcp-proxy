// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"fmt"
	"sort"
	"sync"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// problemList collects the problems one discovery pass found, for a
// problemLog to report (each is logged as an error).
type problemList []string

func (l *problemList) errorf(format string, args ...any) {
	*l = append(*l, fmt.Sprintf(format, args...))
}

// problemLog reports problems that persist across discovery passes - a
// misconfigured Service, a namespace go-proxy may not list - without
// repeating them on every pass (every couple of seconds): each distinct
// message is logged when it first appears, and again only if it goes away
// and later comes back.
type problemLog struct {
	mu   sync.Mutex
	last map[string]bool
	// logf replaces the logging package in tests.
	logf func(msg string)
}

// report logs the problems in current that weren't in the previous report.
func (p *problemLog) report(current problemList) {
	p.mu.Lock()
	defer p.mu.Unlock()

	seen := make(map[string]bool, len(current))
	var fresh []string
	for _, msg := range current {
		if seen[msg] {
			continue
		}
		seen[msg] = true
		if !p.last[msg] {
			fresh = append(fresh, msg)
		}
	}
	p.last = seen

	sort.Strings(fresh)
	for _, msg := range fresh {
		p.log(msg)
	}
}

// active reports whether the previous report had any problems.
func (p *problemLog) active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.last) > 0
}

func (p *problemLog) log(msg string) {
	if p.logf != nil {
		p.logf(msg)
		return
	}
	logging.Error(msg)
}

// k8sProblems and scanProblems are the problem logs of k8s and scan
// discovery, which run once per ServerMonitor tick.
var (
	k8sProblems  = &problemLog{}
	scanProblems = &problemLog{}
)
