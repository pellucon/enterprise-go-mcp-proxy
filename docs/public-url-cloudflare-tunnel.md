# Giving the gateway a public URL with Cloudflare Tunnel

Some AI applications connect to MCP servers **from their vendor's cloud**, not from your computer. Claude Desktop and claude.ai custom connectors are examples. They can't reach `http://localhost/mcp`, so they need the gateway at a **public HTTPS URL**.

The local development and demo setup doesn't provide one. A production deployment does, through the nginx ingress (see the [enterprise deployment guide](enterprise-deployment/enterprise-deployment.md)). This guide shows how to give a local gateway a public URL with a [Cloudflare Tunnel](https://developers.cloudflare.com/tunnel/), for trying such applications without a production deployment.

> ⚠️ **Not yet tested end to end.** These steps follow Cloudflare's and Anthropic's documentation and this project's configuration. They haven't yet been run against Claude Desktop with this gateway. Please report what you find.

## Contents

1. [What has to be public](#1-what-has-to-be-public)
2. [Option A: a named tunnel (recommended)](#2-option-a-a-named-tunnel-recommended)
3. [Option B: a quick tunnel (limited)](#3-option-b-a-quick-tunnel-limited)
4. [Point the gateway at its public URL](#4-point-the-gateway-at-its-public-url)
5. [Check the tunnel](#5-check-the-tunnel)
6. [Connect Claude Desktop](#6-connect-claude-desktop)
7. [Security notes](#7-security-notes)

---

## 1. What has to be public

Two things must be reachable from the internet, not one:

| What | Why | How |
|---|---|---|
| **The gateway's MCP endpoint** | The application sends every MCP request to it | A Cloudflare Tunnel to `http://localhost:80`, which is this guide |
| **The identity provider used to log in** | The application's servers run the OAuth login and receive the result at their own callback address, `https://claude.ai/api/mcp/auth_callback` for Claude | A hosted identity provider such as Okta, Auth0 or Microsoft Entra ID |

**The demo authorization server can't be used here,** for two reasons:
- **It only accepts local callback addresses.** It registers clients only if their callback address points to the local machine (`localhost`, `127.0.0.1`). That's a deliberate anti-phishing safeguard, so it refuses Claude's `https://claude.ai/...` callback.
- **It's only reachable locally.** Its issuer is `http://localhost:8102`.

So configure the gateway with a hosted identity provider, as described for Okta in the [enterprise deployment guide](enterprise-deployment/enterprise-deployment.md#3-configure-okta). In that provider:
- **Allow Claude's callback URL** `https://claude.ai/api/mcp/auth_callback`. Also allow `https://claude.com/api/mcp/auth_callback`, which Anthropic says it may switch to.
- **Choose how Claude registers.** Either let Claude register itself through dynamic client registration, if your provider supports it, or create an application for Claude and note its client ID and secret. Claude accepts a client ID and secret under the connector's **Advanced settings**.

## 2. Option A: a named tunnel (recommended)

A named tunnel gives a stable hostname on a domain you control, and it supports streaming responses (Server-Sent Events), which MCP uses.

**Requirements:** a free Cloudflare account, and a domain whose DNS is managed by Cloudflare.

1. **Install `cloudflared`** and log in. Logging in opens your browser so you can authorize a domain:
   ```sh
   brew install cloudflared
   cloudflared tunnel login
   ```
2. **Create the tunnel.** This writes a credentials file, `~/.cloudflared/<TUNNEL-ID>.json`:
   ```sh
   cloudflared tunnel create mcp-gateway
   ```
3. **Describe what it serves,** in `~/.cloudflared/config.yml`. The local gateway listens on `http://localhost:80`:
   ```yaml
   tunnel: mcp-gateway
   credentials-file: /Users/<you>/.cloudflared/<TUNNEL-ID>.json
   ingress:
     - hostname: mcp.example.com
       service: http://localhost:80
     - service: http_status:404
   ```
4. **Create the DNS record** that points the hostname at the tunnel:
   ```sh
   cloudflared tunnel route dns mcp-gateway mcp.example.com
   ```
5. **Run the tunnel.** Keep it running while you use the public URL:
   ```sh
   cloudflared tunnel run mcp-gateway
   ```

The gateway is now reachable at **`https://mcp.example.com/mcp`**. Cloudflare provides the HTTPS certificate.

## 3. Option B: a quick tunnel (limited)

Cloudflare also offers "quick tunnels", which need no account or domain:

```sh
cloudflared tunnel --url http://localhost:80
# prints a URL such as https://random-words-here.trycloudflare.com
```

> ⚠️ **Probably not usable for MCP.** Quick tunnels [don't support Server-Sent Events](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/), and the gateway streams its MCP responses that way. They're also limited to 200 concurrent requests, come with no uptime guarantee, and get a new random URL every time they start. That last point means reconfiguring the gateway and the connector each time. Use a quick tunnel only to check that the gateway is reachable, e.g. with the commands in [section 5](#5-check-the-tunnel).

## 4. Point the gateway at its public URL

Clients find where to log in through metadata that the gateway publishes about itself. That metadata must name the **public** URL, and the gateway must trust tokens from your hosted identity provider. Both are startup settings in the `auth` section, so prepare a config file and redeploy the proxy.

1. **Copy a config template:**
   ```sh
   cp docs/enterprise-deployment/proxy-server.okta.cfg ./proxy-server.tunnel.cfg
   ```
2. **Edit its `auth` section:**
   - `resourceUrl`: `https://mcp.example.com/mcp`, your tunnel hostname plus `/mcp`.
   - `jwksUrl`, `issuer`, `audience` and `roleClaim`: your identity provider's values. See [Authentication and Authorization](authentication-and-authorization.md#10-production-using-your-identity-provider).
   - `roleMapping.administrator` and `accessRules`: your provider's group names.
3. **Redeploy the proxy:**
   ```sh
   ./scripts/deploy.sh --deploy-proxy --config ./proxy-server.tunnel.cfg
   ```

Don't pass `--ingress-host`: the tunnel replaces the ingress and its TLS certificate.

The gateway's other live configuration (guardrails, orchestration and so on) is kept on a running cluster, because redeploying only seeds sections that don't exist yet (see [Dynamic Configuration](../README.md#dynamic-configuration)).

## 5. Check the tunnel

```sh
# 1. Unauthenticated: expect 401, with a resource_metadata hint naming the public hostname
curl -si https://mcp.example.com/mcp | grep -i -E '^HTTP|www-authenticate'

# 2. The metadata names your identity provider
curl -s https://mcp.example.com/.well-known/oauth-protected-resource
```

The `resource_metadata` hint is built from the request's forwarding headers. If it shows `http://` or `localhost` instead of your public hostname, check how the tunnel forwards the `Host` and `X-Forwarded-Proto` headers.

## 6. Connect Claude Desktop

1. In Claude Desktop (or claude.ai), open **Settings → Connectors → Add custom connector**.
2. Enter the URL, `https://mcp.example.com/mcp`.
3. If your identity provider doesn't support dynamic client registration, open **Advanced settings** and enter the client ID and secret of the application you created for Claude.
4. Click **Connect** and log in at your identity provider. Your roles, from the provider's groups, decide which tools and prompts you see.

Anthropic's own documentation: [building custom connectors via remote MCP servers](https://support.claude.com/en/articles/11503834).

## 7. Security notes

- **The gateway is now on the internet.** It still refuses any request without a valid token from your identity provider. Your backend MCP servers stay private behind it. Only the gateway is exposed, and the cluster's network policies still allow only the gateway to reach them.
- **Stop the tunnel** (`Ctrl+C`, or stop the `cloudflared` service) when you no longer need public access.
- **Don't expose real data through a quick tunnel.** It comes with no guarantees, and its URL is easy to share by accident.
- **Use the enterprise deployment for anything lasting.** It runs the nginx ingress with your own domain and certificate, as in the [enterprise deployment guide](enterprise-deployment/enterprise-deployment.md).
