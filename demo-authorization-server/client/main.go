// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// client is a CLI for demo-authorization-server's admin user-management
// API (add/list/delete users), modeled on the root cli/ tool's original
// subcommand-dispatch structure (see cli/commands.go). All three admin
// endpoints require HTTP
// Basic Auth from a user holding the admin role — -username/-password
// default to admin/admin, this demo server's built-in admin account.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const topLevelUsage = `client: manage demo-authorization-server's users.

Usage:
  client <command> [flags]

Commands:
  add-users     Add or update users/roles from a JSON file
  list-users    List all users and their roles
  delete-user   Delete a user by username

Every command accepts:
  -endpoint string   demo-authorization-server base URL (default "http://localhost:8102")
  -username string   Basic Auth username to authenticate the admin API call with (default "admin")
  -password string   Basic Auth password (default "admin" — this demo's password always equals the username)

Run "client <command> -h" for a command's full flags and examples.

Examples:
  client add-users -file users.json
  client list-users
  client delete-user -name alice
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, topLevelUsage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(topLevelUsage)
		return
	case "add-users":
		runAddUsers(os.Args[2:])
	case "list-users":
		runListUsers(os.Args[2:])
	case "delete-user":
		runDeleteUser(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, topLevelUsage)
		os.Exit(1)
	}
}

type commonFlags struct {
	endpoint string
	username string
	password string
}

func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	cf := &commonFlags{}
	fs.StringVar(&cf.endpoint, "endpoint", "http://localhost:8102", "demo-authorization-server base URL")
	fs.StringVar(&cf.username, "username", "admin", "Basic Auth username to authenticate the admin API call with")
	fs.StringVar(&cf.password, "password", "admin", "Basic Auth password")
	return cf
}

func (cf *commonFlags) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, strings.TrimRight(cf.endpoint, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cf.username, cf.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

func printResponse(resp *http.Response) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		fatal(err)
	}
	var pretty any
	if json.Unmarshal(raw, &pretty) == nil {
		indented, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(indented))
	} else {
		fmt.Println(string(raw))
	}
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}

func printCommandUsage(name string, fs *flag.FlagSet, examples string) {
	fmt.Fprintf(os.Stderr, "Usage: client %s [flags]\n\nFlags:\n", name)
	fs.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nExamples:\n%s", examples)
}

func runAddUsers(args []string) {
	fs := flag.NewFlagSet("add-users", flag.ExitOnError)
	cf := registerCommonFlags(fs)
	var file string
	fs.StringVar(&file, "file", "", `Path to a JSON file holding an array of {"username","roles"} objects (required)`)
	fs.Usage = func() {
		printCommandUsage("add-users", fs, "  client add-users -file users.json\n")
	}
	fs.Parse(args)

	if file == "" {
		fmt.Fprintln(os.Stderr, "add-users: -file is required")
		fs.Usage()
		os.Exit(1)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		fatal(fmt.Errorf("reading -file %q: %w", file, err))
	}

	resp, err := cf.do(http.MethodPost, "/admin/users", strings.NewReader(string(raw)))
	if err != nil {
		fatal(err)
	}
	printResponse(resp)
}

func runListUsers(args []string) {
	fs := flag.NewFlagSet("list-users", flag.ExitOnError)
	cf := registerCommonFlags(fs)
	fs.Usage = func() {
		printCommandUsage("list-users", fs, "  client list-users\n")
	}
	fs.Parse(args)

	resp, err := cf.do(http.MethodGet, "/admin/users", nil)
	if err != nil {
		fatal(err)
	}
	printResponse(resp)
}

func runDeleteUser(args []string) {
	fs := flag.NewFlagSet("delete-user", flag.ExitOnError)
	cf := registerCommonFlags(fs)
	var username string
	// -name, not -username: -username/-password (from registerCommonFlags)
	// are the Basic Auth credentials this command authenticates *as*; -name
	// is the separate, unrelated target user being deleted.
	fs.StringVar(&username, "name", "", "Username to delete (required)")
	fs.Usage = func() {
		printCommandUsage("delete-user", fs, "  client delete-user -name alice\n")
	}
	fs.Parse(args)

	if username == "" {
		fmt.Fprintln(os.Stderr, "delete-user: -name is required")
		fs.Usage()
		os.Exit(1)
	}

	resp, err := cf.do(http.MethodDelete, "/admin/users/"+username, nil)
	if err != nil {
		fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		fmt.Printf("Deleted user %q\n", username)
		return
	}
	printResponse(resp)
}
