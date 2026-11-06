# Discovery example: a hello-world MCP server

This example deploys a tiny MCP server with one tool, `say_hello`, into Kubernetes. go-proxy discovers it automatically and serves the tool as `hello-world.say_hello`, with no restart. The only change go-proxy needs is to trust the example's namespace (one admin command, below). It's the shortest path to seeing dynamic discovery work, and a starting point for adding your own server.

Nothing is compiled or built. The server is a ten-line Python script (`server.py`) on the official [Python MCP SDK](https://github.com/modelcontextprotocol/python-sdk). It runs on the stock `python:3.12-slim` image, which installs the SDK when the pod starts.

## What's here

| File | Purpose |
|---|---|
| `server.py` | The MCP server: one `say_hello(name)` tool, served over Streamable HTTP at `:8080/mcp` |
| `k8s/deployment.yaml` | Runs `server.py` on `python:3.12-slim`; the SDK (`mcp==2.3.0`) is installed at startup |
| `k8s/service.yaml` | The Service go-proxy discovers, through its label and annotation |
| `k8s/networkpolicy.yaml` | Lets only go-proxy reach the server |
| `../../k8s/discovery/go-proxy-discovery-rbac.yaml` | Lets go-proxy list Services in the example's namespace (applied by `deploy.sh`) |
| `deploy.sh` | Deploys (or removes, with `--delete`) the example with `kubectl` |
| `wait-for-discovery.sh` | Waits until the pod is ready and go-proxy lists the tool |
| `call-tool.sh` | Calls `hello-world.say_hello` through go-proxy |

## Prerequisites

- go-proxy deployed to a Kubernetes cluster, with `kubectl` pointing at that cluster. See the [Quick Start](../../README.md#quick-start).
- Internet access from the cluster, so the pod can install the MCP SDK from PyPI.
- `python3` on your machine (`call-tool.sh` uses it to build the tool's JSON arguments).

The example works the same in both environments:
- **Development:** a repo checkout, run from `examples/discovery/`.
- **Release:** an extracted deploy bundle, which includes `examples/discovery/` and the prebuilt CLI.

## Let go-proxy discover the namespace

go-proxy only discovers backends in the namespaces listed in its discovery config's `k8sNamespaces`. The shipped config lists `mcp-proxy` and `gardening-demo`, not `mcp-examples`: every namespace listed there is trusted to provide backends, so each one is an explicit decision.

Add `mcp-examples` once, as an administrator, with the CLI (run from the repo root, or from an extracted deploy bundle):

```sh
./scripts/cli.sh -username admin -password admin export-discovery > discovery.json
# edit discovery.json: add "mcp-examples" to "k8sNamespaces"
./scripts/cli.sh -username admin -password admin update-discovery -file discovery.json
```

The change takes effect within a few seconds, with no restart. `deploy.sh` grants go-proxy access to list Services in the namespace, and tells you if the namespace still isn't in `k8sNamespaces`.

To remove it again, run the same commands, taking `mcp-examples` out of the list.

## Run it

```sh
cd examples/discovery
./deploy.sh
./wait-for-discovery.sh
./call-tool.sh Alice
```

Expected output, in order:

```text
==> Ensuring namespace mcp-examples
...
deployment "hello-world" successfully rolled out
Deployed. Next: ./wait-for-discovery.sh

==> Waiting for the hello-world pod to be ready
deployment "hello-world" successfully rolled out
==> Waiting for go-proxy to discover hello-world.say_hello
Discovered hello-world.say_hello. Next: ./call-tool.sh [name]

Hello, Alice!
```

The first deploy takes about 10 seconds on a local cluster. Most of that is pulling the image and installing the SDK. go-proxy picks the server up within a few seconds of the Service appearing.

## Remove it

```sh
./deploy.sh --delete
```

This deletes the `mcp-examples` namespace. go-proxy notices the Service is gone and drops `hello-world.say_hello` from its tool list within seconds.

## How discovery finds it

go-proxy runs in `k8s` discovery mode (the `discovery` section of `proxy-server.cfg`). Every couple of seconds, it lists the Services that carry the label `mcp.io/role=backend-mcp-server` in each namespace in `k8sNamespaces`. Each match becomes a backend server:

| Setting | Default | This example |
|---|---|---|
| Server name | the Service name | `hello-world`, so tools are named `hello-world.<tool>` |
| Port | the Service's first port | `8080` |
| Path | the root path `/` | `/mcp`, set with the `mcp.io/path` annotation |

To override the name or port, add the `mcp.io/server-name` or `mcp.io/port` annotation to the Service.

After it finds a server, go-proxy fetches the server's tools, prompts and resources and adds them to its own, under the server's name. Who may call them is decided by the role rules (`accessRules` in `proxy-server.cfg`). The demo `admin` role is allowed all tools, which is why the scripts log in as `admin`. To expose the tool to other roles, add `hello-world.say_hello` (or `hello-world.*`) to their `allowed_tools`, for example with the CLI's `update-role-permissions` command.

The `NetworkPolicy` keeps the security model intact: backend servers must be reachable only through go-proxy. It denies all ingress in the namespace, except from go-proxy's pods in `mcp-proxy`.

## Options

All three scripts read these environment variables:

| Variable | Default | Used by |
|---|---|---|
| `NAMESPACE` | `mcp-examples` | `deploy.sh`, `wait-for-discovery.sh` |
| `CLI_FLAGS` | `-username admin -password admin` | `wait-for-discovery.sh`, `call-tool.sh` |
| `TIMEOUT` | `180` (seconds) | `wait-for-discovery.sh` |

`CLI_FLAGS` is passed to `scripts/cli.sh` as-is. Use it to target a different endpoint, or to log in through a real identity provider instead of the demo auth server:

```sh
CLI_FLAGS="-endpoint http://localhost:8081/mcp -username admin -password admin" ./call-tool.sh
```

## Adapt it for your own server

- **Change the tool:** edit `server.py` and run `./deploy.sh` again. The script republishes the script and restarts the pod.
- **Use your own server:** replace the Deployment with your server's image, keeping three things:
  1. The `mcp.io/role: backend-mcp-server` label on the Service.
  2. An `mcp.io/path` annotation if your server isn't served at `/`.
  3. The NetworkPolicy.

  The server must speak MCP over Streamable HTTP.

## Notes

- **Installing at startup is for convenience, not production.** Every pod start downloads the SDK, so it needs internet access and adds a few seconds. For a real server, build an image with its dependencies included.
- **The SDK version is pinned** (`mcp==2.3.0`) so the example doesn't break when a new SDK release changes its API. Version 2 renamed `FastMCP` to `MCPServer`.
- **Discovery follows the Service, not the pod.** A tool can be listed before its pod is ready, which is why `wait-for-discovery.sh` waits for the rollout first.
