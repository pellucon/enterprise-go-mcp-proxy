#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# curl-based equivalent of demo-authorization-server/client — manages users
# via the same admin API (POST/GET/DELETE /admin/users), for anyone who
# doesn't want to build/run the Go client. All three admin endpoints
# require HTTP Basic Auth from a user holding the admin role; this demo
# server's password always equals the username, so admin/admin is the
# default.
set -euo pipefail

ENDPOINT="${DEMO_AUTH_ENDPOINT:-http://localhost:8102}"
USERNAME="${DEMO_AUTH_USERNAME:-admin}"
PASSWORD="${DEMO_AUTH_PASSWORD:-admin}"

usage() {
  cat <<EOF
Usage: $(basename "$0") <command> [args]

Commands:
  add <file.json>     Add or update users/roles from a JSON file (array of
                       {"username","roles"} objects)
  list                 List all users and their roles
  delete <username>    Delete a user by username

Environment overrides:
  DEMO_AUTH_ENDPOINT   demo-authorization-server base URL (default: $ENDPOINT)
  DEMO_AUTH_USERNAME   Basic Auth username (default: $USERNAME)
  DEMO_AUTH_PASSWORD   Basic Auth password (default: $PASSWORD)

Examples:
  $(basename "$0") add users.json
  $(basename "$0") list
  $(basename "$0") delete alice
EOF
}

if [[ $# -lt 1 ]]; then
  usage
  exit 1
fi

command="$1"
shift

case "$command" in
  add)
    if [[ $# -lt 1 ]]; then
      echo "add: a JSON file path is required" >&2
      usage
      exit 1
    fi
    curl -sS -u "$USERNAME:$PASSWORD" -X POST \
      -H 'Content-Type: application/json' \
      --data @"$1" \
      "$ENDPOINT/admin/users"
    echo
    ;;
  list)
    curl -sS -u "$USERNAME:$PASSWORD" "$ENDPOINT/admin/users"
    echo
    ;;
  delete)
    if [[ $# -lt 1 ]]; then
      echo "delete: a username is required" >&2
      usage
      exit 1
    fi
    curl -sS -u "$USERNAME:$PASSWORD" -X DELETE \
      -w '\nHTTP %{http_code}\n' \
      "$ENDPOINT/admin/users/$1"
    ;;
  -h|--help)
    usage
    ;;
  *)
    echo "Unknown command: $command" >&2
    usage
    exit 1
    ;;
esac
