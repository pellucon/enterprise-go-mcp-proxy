# Enterprise deployment: nginx ingress, a port-less HTTPS URL, and Okta

This guide deploys go-proxy the way an enterprise typically runs it:

- clients reach it at a port-less HTTPS URL such as **`https://mcp.acme.com/mcp`**;
- an **nginx ingress controller** terminates TLS and routes to go-proxy;
- **Okta** is the identity provider (IdP) — users log in there, and go-proxy only verifies the JWT access tokens Okta issues.

Nothing in this guide changes the local Colima/k3s workflow. The default `./scripts/deploy.sh` invocation, `proxy-server.cfg`, `k8s/go-proxy-service.yaml` and the CLI defaults are all unchanged; everything below is opt-in, through `--ingress-host` and a separate config file.

## Contents

1. [Architecture](#1-architecture)
2. [What differs from the local setup](#2-what-differs-from-the-local-setup)
3. [Configure Okta](#3-configure-okta)
4. [Write the proxy config](#4-write-the-proxy-config)
5. [Prepare the cluster](#5-prepare-the-cluster)
6. [Deploy](#6-deploy)
7. [Connect clients](#7-connect-clients)
8. [Verify](#8-verify)
9. [Operations: scaling, network policy, config updates](#9-operations)
10. [Troubleshooting](#10-troubleshooting)

---

## 1. Architecture

```
                      ┌──────────────────────── Kubernetes cluster ────────────────────────┐
                      │                                                                    │
 MCP client ──HTTPS──▶│ nginx ingress (TLS for mcp.acme.com)                               │
   │                  │     │  /mcp, /.well-known/oauth-protected-resource  (HTTP)         │
   │                  │     ▼                                                              │
   │                  │ Service go-proxy (ClusterIP :80) ──▶ go-proxy pods (:8081, N reps) │
   │                  │                                         │        │                 │
   │                  │                                         │        └──▶ backend MCP  │
   │                  │                                         │             servers      │
   │                  └─────────────────────────────────────────┼──────────────────────────┘
   │                                                            │ JWKS fetch (HTTPS, egress)
   └──── OAuth login (authorize / token, HTTPS) ──▶  Okta custom authorization server
```

The login flow for a generic MCP client:

1. The client calls `https://mcp.acme.com/mcp` with no token and gets **HTTP 401** with
   `WWW-Authenticate: Bearer resource_metadata="https://mcp.acme.com/.well-known/oauth-protected-resource"`.
2. It fetches that RFC 9728 document, which names the Okta issuer under `authorization_servers`.
3. It fetches Okta's authorization-server metadata, runs an authorization-code + PKCE login in the user's browser, and receives a JWT access token.
4. It calls `/mcp` again with `Authorization: Bearer <token>`. go-proxy verifies the signature against Okta's JWKS, checks `iss`, `aud` and `exp`, reads the user's groups as roles, and applies `accessRules`.

go-proxy never forwards the Okta token to backends; they receive a short-lived proxy-signed identity assertion instead (see `docs/architecture/auth.md`).

## 2. What differs from the local setup

| Concern | Local Colima/k3s (default) | Enterprise (this guide) |
|---|---|---|
| Client URL | `http://localhost/mcp` | `https://mcp.acme.com/mcp` |
| Exposure | `go-proxy` Service `type: LoadBalancer` on :80 on a local cluster (k3s klipper); `ClusterIP` on any other unless `--public-load-balancer` | Service `type: ClusterIP` + an `Ingress` (`k8s/overlays/ingress/`) |
| TLS | none | terminated at nginx (or in front of it) |
| IdP | `demo-authorization-server` (`--deploy-auth-server`) | Okta custom authorization server; the demo server is **not** deployed |
| Config file | `proxy-server.cfg` | your copy of `docs/enterprise-deployment/proxy-server.okta.cfg` |
| Client registration | dynamic (DCR) against the demo server | a **pre-registered** Okta app (`-client-id`); Okta has no anonymous DCR |
| Administrator role (`roleMapping.administrator`) | `admin` | your Okta admin group, e.g. `mcp-gateway-admins` |
| Images | `:local` from the Colima docker daemon | `--release VERSION` from GHCR (or your own registry, see §5.4) |
| `deploy.sh` flags | `--deploy-all` | `--deploy-proxy --deploy-platform-servers --release … --config … --ingress-host …` |

Things that do **not** change: `selfUrl` (`http://go-proxy.mcp-proxy.svc.cluster.local/mcp` — the in-cluster Service still listens on port 80), `natsUrl`, `internalAuth`, discovery, and every other config section.

## 3. Configure Okta

You need an Okta admin. Okta's console layout changes over time; the names below are the concepts to look for.

### 3.1 Use a custom authorization server

Under **Security → API → Authorization Servers**, use the built-in `default` server or create a dedicated one (recommended, so its claims and policies only affect this gateway).

- **Do not use the Okta org authorization server** (issuer `https://acme.okta.com` with no `/oauth2/...` path). Its access tokens are meant for Okta's own APIs and are not supported for validation by third-party resource servers.
- Note its **Issuer URI**, e.g. `https://acme.okta.com/oauth2/default` (or `https://acme.okta.com/oauth2/aus1abc...` for a dedicated one). This exact string goes into `auth.issuer`.
- Set its **Audience** to an identifier for this gateway, e.g. `api://mcp.acme.com`. This exact string goes into `auth.audience`.

The JWKS URL is `<issuer>/v1/keys`; the metadata document is at `<issuer>/.well-known/oauth-authorization-server`.

### 3.2 Add a scope

Under the authorization server's **Scopes**, add a custom scope such as `mcp.access`. Okta custom authorization servers reject an authorization request that names no scope, which is why this repo's CLI has a `-scope` flag (§7).

### 3.3 Put the user's groups in the access token

Under **Claims**, add a claim:

| Setting | Value |
|---|---|
| Name | `groups` (must match `auth.roleClaim`) |
| Include in token type | **Access Token** (go-proxy only sees the access token, never the ID token) |
| Value type | Groups |
| Filter | only the groups relevant to the gateway, e.g. *Starts with* `mcp-`, or a regex matching your list |
| Include in | any scope, or just `mcp.access` |

Every value of this claim becomes a **role** that `accessRules.clientAccessRules[].role` patterns are matched against (`filepath.Match` syntax, so `mcp-*` works).

Decide which Okta group holds the **gateway administrators**, e.g. `mcp-gateway-admins`, and make sure the filter lets it through. You will map it to the gateway's administrator role in the config (§4.2); it does not need to be named `admin`.

### 3.4 Create an access policy

Under **Access Policies**, add a policy assigned to the client app(s) below, with a rule that allows the **Authorization Code** grant (and **Refresh Token** if wanted), for the users/groups who may use the gateway, granting the `mcp.access` scope (plus `openid`/`offline_access` if requested).

### 3.5 Register client applications

Okta does **not** offer anonymous Dynamic Client Registration, so every MCP client needs a pre-registered app. Under **Applications → Create App Integration**, choose **OIDC** and **Native Application** (a public client using PKCE, no client secret):

| Setting | Value |
|---|---|
| Grant types | Authorization Code (+ Refresh Token if wanted) |
| Sign-in redirect URIs | for this repo's CLI: `http://127.0.0.1:8765/callback` (the CLI binds `127.0.0.1` and serves `/callback`; pick any free port and pass the same one as `-redirect-port`). For other MCP clients: whatever loopback redirect URI that client documents. |
| Assignments | the groups allowed to use the gateway |

Note the app's **Client ID**. Users run the CLI with `-client-id <that ID>`.

A generic MCP client that can *only* do Dynamic Client Registration cannot log in against Okta; use a client that lets you configure a pre-registered client ID.

## 4. Write the proxy config

Copy the template, then edit three parts of it: `auth` (§4.1), `roleMapping` (§4.2) and `accessRules` (§4.3).

```sh
cp docs/enterprise-deployment/proxy-server.okta.cfg ./proxy-server.okta.cfg
```

### 4.1 `auth`

```json
"auth": {
  "publicKeyPath": "",
  "jwksUrl": "https://acme.okta.com/oauth2/default/v1/keys",
  "issuer": "https://acme.okta.com/oauth2/default",
  "audience": "api://mcp.acme.com",
  "roleClaim": "groups",
  "resourceUrl": "https://mcp.acme.com/mcp"
}
```

| Field | Value | Notes |
|---|---|---|
| `publicKeyPath` | `""` | Unused when `jwksUrl` is set. With it empty, `deploy.sh` creates no `jwt-public-key` Secret. |
| `jwksUrl` | `<issuer>/v1/keys` | Fetched **synchronously at startup**: if go-proxy can't reach it, the pod fails fast (see §9.2 on egress). Keys are refreshed in the background, so Okta key rotation needs no restart. |
| `issuer` | Okta Issuer URI, exactly | Compared byte-for-byte with the token's `iss`. Also published as `authorization_servers` in the protected-resource metadata, which is how generic clients find Okta. |
| `audience` | the authorization server's Audience | Compared with the token's `aud`. |
| `roleClaim` | `groups` | The claim from §3.3. |
| `resourceUrl` | `https://mcp.acme.com/mcp` | The public URL. Enables the 401 `resource_metadata` hint and the `/.well-known/oauth-protected-resource` document. Must be exactly `https://<--ingress-host>/mcp`: with `--deploy-proxy --ingress-host`, `deploy.sh` checks this before deploying anything and stops on a mismatch. |

### 4.2 `roleMapping`: who is the gateway administrator

```json
"roleMapping": { "administrator": "mcp-gateway-admins" }
```

The gateway has one built-in role, **administrator**. It is the only role allowed to call the gateway's own `_admin.*` tools: the live config updates (`_admin.update_*`), their exports, and `_admin.export_full_config`. `roleMapping.administrator` says which role name, as it appears in the token's role claim, *is* that administrator: here, members of the Okta group `mcp-gateway-admins`.

| Rule | Detail |
|---|---|
| Exact match | The value is compared literally with each role in the token, with no wildcards. `mcp-gateway-*` matches only a role literally called `mcp-gateway-*`. |
| Required | There is no default. If the `roleMapping` section is missing, or its `administrator` is missing or empty, go-proxy fails to start. Every deployment states its administrator role explicitly; the repo's own config files map it to `admin`. |
| Fixed at startup | It is read once, when go-proxy starts, and is **not** one of the live-updatable sections: no admin tool can change it, and an `accessRules` update cannot make another role an administrator. To change it, edit the config and redeploy (§9.3). This keeps an administrator from handing admin rights to another role at runtime, or locking every administrator out. |
| Only `_admin.*` | It does not grant access to backend tools, prompts or resources. Give the administrator group those in `accessRules` like any other role (§4.3). |

go-proxy logs the active mapping at startup:

```
administrator role (authorizes _admin.* tools): "mcp-gateway-admins"
```

`_admin.export_full_config` includes `roleMapping`, so an exported config can be redeployed as-is, but there is no `_admin.update_role_mapping`.

### 4.3 `accessRules`

Adjust `accessRules` to your Okta group names, e.g.:

```json
"clientAccessRules": [
  { "role": "mcp-gateway-admins", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] },
  { "role": "mcp-finance",        "allowed_tools": ["ledger.*"], "allowed_prompts": [], "allowed_resources": [], "allowed_resource_templates": [] },
  { "role": "*",                  "allowed_tools": ["proxy_utility.call_tool", "proxy_utility.get_my_id_details"], "allowed_prompts": [], "allowed_resources": [], "allowed_resource_templates": [] }
]
```

The first rule gives the administrator group every backend tool; `roleMapping` alone would only give it the `_admin.*` tools.

### 4.4 Everything else

Leave everything else as in the template: `selfUrl` and `natsUrl` are in-cluster addresses and are the same in every deployment.

`auth` and `roleMapping` (like `internalAuth`, `selfUrl` and `natsUrl`) are read only at startup: changing them means a redeploy. `accessRules` and the other sections are live-updatable later through the `_admin.update_*` tools (§9.3).

## 5. Prepare the cluster

### 5.1 Ingress controller

Install an NGINX-based ingress controller if the cluster doesn't already have one. `k8s/overlays/ingress/ingress.yaml` carries annotations for both common ones — the community **ingress-nginx** (`nginx.ingress.kubernetes.io/*`) and **F5 NGINX Ingress Controller** (`nginx.org/*`); each ignores the other's. Note its **IngressClass** name (`kubectl get ingressclass`); the default assumed by `deploy.sh` is `nginx`.

### 5.2 DNS

Point `mcp.acme.com` at the ingress controller's external address (`kubectl get svc -n <controller namespace>`), usually via your DNS provider or an external load balancer.

### 5.3 TLS certificate

Choose one:

- **TLS at nginx:** create a `kubernetes.io/tls` Secret in the **`mcp-proxy`** namespace and pass its name as `--tls-secret`:
  ```sh
  kubectl create namespace mcp-proxy --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n mcp-proxy create secret tls mcp-acme-com-tls --cert=mcp.acme.com.crt --key=mcp.acme.com.key
  ```
- **TLS at nginx, certificate from cert-manager:** if the cluster runs [cert-manager](https://cert-manager.io) with a `ClusterIssuer` (e.g. Let's Encrypt via ACME HTTP-01, or your internal CA), pass `--cluster-issuer <issuer-name>` instead of creating the Secret yourself. `deploy.sh` adds cert-manager's annotation and a `tls` block to the Ingress, so cert-manager issues the certificate into Secret `go-proxy-tls` (or the name you pass with `--tls-secret`) and renews it automatically. It also applies `k8s/overlays/networkpolicy-acme-http01-solver.yaml`: cert-manager answers HTTP-01 challenges with a short-lived solver pod in the `mcp-proxy` namespace, which is default-deny for ingress, so without this policy the challenge would never be reachable. (The policy only matters for HTTP-01; a DNS-01 issuer doesn't need it.)
- **TLS in front of nginx** (a cloud load balancer, or the controller's default certificate): omit `--tls-secret`.

In both cases go-proxy itself still speaks plain HTTP inside the cluster. It builds the 401 hint from the `X-Forwarded-Proto`/`X-Forwarded-Host` headers nginx adds, so clients are told `https://mcp.acme.com/...`, not the internal address. If something in front of nginx terminates TLS, make sure nginx passes the original `X-Forwarded-Proto: https` through (for ingress-nginx: `use-forwarded-headers: "true"` in its ConfigMap).

### 5.4 Images

`deploy.sh --release VERSION --github-owner pellucon` pulls the public images `ghcr.io/pellucon/<image>:VERSION`, with no credentials — the simplest option if the cluster can reach GHCR. For private packages (a fork's releases), add `--github-token-file FILE`, a token with `read:packages`; `deploy.sh` then creates the pull Secret itself.

If images must come from an internal registry, mirror them there (`docker pull` / `docker tag` / `docker push`, or your registry's proxy-cache feature). `deploy.sh` only knows GHCR, so in that case render the manifests yourself (§6.3) and set the image with `kustomize edit set image go-proxy=registry.acme.com/mcp/go-proxy:VERSION` plus your own `imagePullSecrets`.

## 6. Deploy

### 6.1 Which targets

| Target | Deploy? | Why |
|---|---|---|
| `--deploy-proxy` | **yes** | go-proxy, NATS, RBAC, NetworkPolicies, config |
| `--deploy-platform-servers` | **yes** | event-aggregator, declarative-guardrail, rate-limiter — referenced by the config's orchestration |
| `--deploy-server DIR` | for your own backends | any backend whose Service carries `mcp.io/role=backend-mcp-server` is discovered automatically |
| `--deploy-ai` | optional | needs `--ai-api-key-file` and `--ai-model` |
| `--deploy-auth-server` | **no** | demo IdP; Okta replaces it |
| `--deploy-gardening-demo` | **no** | demo data; its admin servers sync roles to the demo IdP's user API, which Okta doesn't have |
| `--deploy-all` | **no** | includes both demo targets above |

### 6.2 With `deploy.sh`

From a checkout, or from an extracted release deploy-bundle (which includes this guide, the template and `deploy.sh`):

```sh
./scripts/deploy.sh --deploy-proxy --deploy-platform-servers \
  --release v0.9.0 --github-owner pellucon \
  --config ./proxy-server.okta.cfg \
  --ingress-host mcp.acme.com --tls-secret mcp-acme-com-tls \
  --proxy-replicas 3
```

| Flag | Meaning |
|---|---|
| `--ingress-host HOST` | Switches go-proxy's exposure to the Ingress path. Without it, go-proxy's Service is a LoadBalancer on a local cluster (its API server on this machine) and `ClusterIP` on any other — reachable only inside the cluster — unless you also pass `--public-load-balancer`, which gives it a public plain-HTTP address on a cloud cluster. The demo auth server is never made public on a non-local cluster. |
| `--ingress-class NAME` | IngressClass to use (default `nginx`). |
| `--tls-secret NAME` | Adds a `tls` block for HOST using that Secret. Omit when TLS is terminated before nginx. |
| `--cluster-issuer NAME` | cert-manager `ClusterIssuer` that issues and renews the certificate (into `--tls-secret`, or `go-proxy-tls`), §5.3. Only with `--ingress-host`. |
| `--config PATH` | Your Okta config; its content becomes the `proxy-server-config` ConfigMap. |
| `--proxy-replicas N` | go-proxy is stateless — any N works (§9.1). |

On the Ingress path `deploy.sh` applies `k8s/go-proxy-deployment.yaml` and `k8s/go-proxy-service.yaml` through a Kustomize overlay that patches the Service to `ClusterIP` (`k8s/overlays/ingress/service-clusterip-patch.yaml`) and adds `k8s/overlays/ingress/ingress.yaml` with the host, class and TLS filled in. It skips every LoadBalancer wait.

### 6.3 Without `deploy.sh` (GitOps / Helm-wrapped)

The overlay files are plain YAML, so they can be vendored into your own tooling. The equivalent of what `deploy.sh` renders for go-proxy itself:

```yaml
# kustomization.yaml in your GitOps repo
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: mcp-proxy
resources:
  - go-proxy-deployment.yaml          # from k8s/
  - go-proxy-service.yaml             # from k8s/
  - ingress.yaml                      # from k8s/overlays/ingress/, with __INGRESS_HOST__/__INGRESS_CLASS__ replaced
patches:
  - path: service-clusterip-patch.yaml  # from k8s/overlays/ingress/
images:
  - name: go-proxy
    newName: registry.acme.com/mcp/go-proxy
    newTag: v0.9.0
```

Add a `tls:` block to the Ingress yourself. The rest of `deploy.sh --deploy-proxy` (namespace, RBAC, NetworkPolicies, NATS, the `proxy-server-config` ConfigMap from your cfg file) must be reproduced too — read `deploy_proxy` in `scripts/lib/deploy-proxy.sh` for the exact objects. With cert-manager, add the `cert-manager.io/cluster-issuer: <issuer>` annotation alongside the `tls:` block, and (for an HTTP-01 issuer) include `k8s/overlays/networkpolicy-acme-http01-solver.yaml`.

### 6.4 Ingress annotations, and why

| Annotation (ingress-nginx / F5) | Value | Reason |
|---|---|---|
| `proxy-buffering` / `nginx.org/proxy-buffering` | `off` / `False` | MCP's streamable-HTTP transport can answer a POST with a `text/event-stream`; buffering would hold events back until the response ends. |
| `proxy-request-buffering` | `off` | Pass requests through as they arrive. |
| `proxy-read-timeout`, `proxy-send-timeout` | `3600` / `3600s` | nginx's 60 s default would cut off slow tool calls (orchestrations, AI-backed tools) mid-stream. Lower it if your longest tool call is shorter. |
| `proxy-body-size` / `client-max-body-size` | `10m` | Large tool arguments; nginx's default is 1 MB. |

No session affinity is needed: go-proxy runs the MCP transport in stateless mode (§9.1).

Only `/mcp` and `/.well-known/oauth-protected-resource` (plus its path-inserted variant `/.well-known/oauth-protected-resource/mcp`) are routed. `/healthz` and `/.well-known/jwks-internal.json` remain in-cluster.

## 7. Connect clients

| What | URL |
|---|---|
| MCP endpoint | `https://mcp.acme.com/mcp` |
| Authorization server (issuer) | `https://acme.okta.com/oauth2/default` |

**This repo's CLI** (from a checkout via `./scripts/cli.sh`, or `bin/cli` from a deploy-bundle):

```sh
./scripts/cli.sh \
  -endpoint https://mcp.acme.com/mcp \
  -auth-server https://acme.okta.com/oauth2/default \
  -client-id 0oa1b2c3d4EXAMPLE \
  -redirect-port 8765 \
  -scope "openid mcp.access" \
  list-tools
```

- `-auth-server` is the **issuer**, including `/oauth2/<id>`; the CLI appends `/.well-known/oauth-authorization-server` to it. The CLI checks that the metadata it gets back names exactly that issuer (RFC 8414 §3.3), so a login against a different URL for the same server fails with "names issuer …". If you must reach the server at another address than its issuer, pass the issuer as `-auth-issuer`.
- `-redirect-port` must match the port registered in §3.5 (`http://127.0.0.1:8765/callback`).
- The CLI prints a login URL; open it in a browser and sign in with Okta.
- `-username`/`-password` do not work against Okta (they are a demo-authorization-server shortcut).
- `-audience` is not needed for Okta (the authorization server's Audience is fixed); it exists for Auth0.

**Generic MCP clients** (IDEs, agent frameworks): configure the server URL `https://mcp.acme.com/mcp`. A spec-compliant client discovers Okta on its own from the 401 (§1). It must allow a pre-registered **client ID** (§3.5), and that app's redirect URIs must include the client's own callback URL. If the client asks for scopes, use `openid mcp.access`.

## 8. Verify

```sh
# 1. Unauthenticated → 401 with an https resource_metadata hint
curl -si -X POST https://mcp.acme.com/mcp | grep -i '^www-authenticate'
#   WWW-Authenticate: Bearer resource_metadata="https://mcp.acme.com/.well-known/oauth-protected-resource"

# 2. Protected-resource metadata names Okta
curl -s https://mcp.acme.com/.well-known/oauth-protected-resource
#   {"resource":"https://mcp.acme.com/mcp","authorization_servers":["https://acme.okta.com/oauth2/default"],...}

# 3. Okta metadata is reachable at the issuer
curl -s https://acme.okta.com/oauth2/default/.well-known/oauth-authorization-server | head -c 300

# 4. A real login
./scripts/cli.sh -endpoint https://mcp.acme.com/mcp -auth-server https://acme.okta.com/oauth2/default \
  -client-id <id> -redirect-port 8765 -scope "openid mcp.access" \
  call-tool -name proxy_utility.get_my_id_details
#   shows your subject and the roles (Okta groups) go-proxy derived from the token

# 5. Cluster state
kubectl -n mcp-proxy get ingress go-proxy
kubectl -n mcp-proxy get svc go-proxy        # TYPE should be ClusterIP
kubectl -n mcp-proxy logs deploy/go-proxy | head
```

## 9. Operations

### 9.1 Scaling

go-proxy serves MCP in stateless mode, so any replica can serve any request and nginx needs no sticky sessions. Scale with `--proxy-replicas N` (or `kubectl scale`). Replicas share their internal signing key through a Secret they create themselves, and config updates reach every replica through the configsync ConfigMaps.

### 9.2 Network policy

- `k8s/networkpolicy-allow-ingress-to-proxy.yaml` admits traffic to go-proxy's port 8081 from any source, which already includes the ingress controller. To tighten it so only the controller can reach go-proxy, replace its rule with one scoped to the controller's namespace, e.g.:
  ```yaml
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
      ports:
        - protocol: TCP
          port: 8081
  ```
  Backends calling back into go-proxy (relayed calls) and the platform servers also need to keep access, so add `podSelector`/`namespaceSelector` entries for them too if you do this.
- By default the repo's policies restrict **ingress** only. **`--egress-policies`** (with `--deploy-proxy`) opts in to restricting go-proxy's outbound connections too: the `go-proxy-egress` policy allows DNS, the Kubernetes API server (its address is looked up at deploy time), go-proxy's own namespace (NATS, platform servers, its own loopback), the discovery namespaces and an in-cluster `auth.jwksUrl`. Anything off-cluster needs **`--egress-allow CIDR[:PORT]`**: for Okta, `--egress-allow 0.0.0.0/0:443` (Okta's addresses change, so a narrower range rarely works without an egress proxy), plus any off-cluster static backends and OAuth token URLs of backend credentials. Pass the same flags on every `--deploy-proxy` (a deploy without `--egress-policies` removes the policy), and re-run after changing discovery namespaces. A blocked connection usually shows up as a timeout, not an error. If your cluster already manages egress itself (Calico/Cilium cluster-wide policies, a service mesh, an egress gateway), leave this off and allow the same destinations there.
- Backends trust that they are reachable only through go-proxy. Make sure no Ingress or LoadBalancer exposes a backend directly.

### 9.3 Changing configuration later

- `accessRules`, `governanceOrchestration`, `discovery`, `eventDictionary`, `declarativeGuardrails`, `eventAggregation`, `rateLimiting`, `logLevel`: live, via the `_admin.update_*` tools (requires the administrator role from `roleMapping`) — e.g. `cli ... update-role-permissions -file rules.json`. Redeploying never overwrites these once they've been seeded, and go-proxy refuses to start with a cfg whose sections were edited so that they differ from the live config. After live updates, regenerate the cfg with `cli ... export-full-config > proxy-server.okta.cfg` before the next deploy; see `docs/development-and-deployment.md` §4.
- `auth`, `roleMapping`, `internalAuth`, `selfUrl`, `natsUrl`: edit the cfg and redeploy. No admin tool can change these. `roleMapping` in particular is deliberately outside the admin tools' reach (§4.2).
- **Adding a backend that relays, or a guardrail step on a new server:** pin that server in `discovery.k8sPinnedServers` first (`_admin.update_discovery`), then add it to `internalAuth.relay` (redeploy) or the orchestration. The gateway refuses to start, and refuses the orchestration update, while a server it trusts that way is unpinned (`docs/architecture/discovery.md`, "Discovery trust rules").

## 10. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| go-proxy pod crash-loops with `failed to load JWKS from …` | `jwksUrl` wrong, or egress to Okta blocked | Check the URL with `curl`; allow egress on 443 (§9.2). |
| Every call fails with `token has invalid issuer` | `auth.issuer` differs from the token's `iss` (e.g. trailing slash, org vs custom server) | Decode a token (`jwt.io` or `cut -d. -f2 \| base64 -d`) and copy `iss` exactly. |
| `token has invalid audience` | `auth.audience` ≠ the authorization server's Audience | Make them identical. |
| Logged in, but `list-tools` is empty or calls are denied | No `groups` claim in the **access** token, or the filter excludes the group | Add the claim to the Access Token (§3.3); check with `proxy_utility.get_my_id_details`. |
| `_admin.*` tools denied for an Okta admin | The token's roles don't include `roleMapping.administrator` exactly (spelling, case, or the group filtered out of the claim) | Compare the startup log line `administrator role (authorizes _admin.* tools): "…"` with the roles `proxy_utility.get_my_id_details` reports (§4.2). |
| Administrator can call `_admin.*` tools but sees no backend tools | `roleMapping` only covers `_admin.*` | Add an `accessRules` rule for the administrator group (§4.3). |
| go-proxy fails to start: `failed to parse "roleMapping" section` | The `roleMapping` section is missing, or its `administrator` is missing or empty | Add `"roleMapping": { "administrator": "<your admin group>" }` to the cfg (§4.2). There is no default. |
| Okta error page: `invalid_scope` / "One or more scopes are not configured…" | No `-scope`, or the scope isn't granted by the access policy | Pass `-scope "openid mcp.access"`; check the policy rule (§3.4). |
| Okta error: `redirect_uri` mismatch | Registered URI ≠ `http://127.0.0.1:<redirect-port>/callback` | Register exactly that, with the same port as `-redirect-port`. |
| CLI: `no -client-id given and … does not support dynamic client registration` | Expected with Okta | Pass `-client-id` (§3.5). |
| 401 hint says `http://…` instead of `https://…` | `X-Forwarded-Proto` not reaching go-proxy as `https` | When TLS terminates before nginx, enable forwarded-header trust in the controller (§5.3). |
| Tool calls hang, then fail after ~60 s, or streamed output arrives all at once | Ingress buffering/timeouts | Check the annotations were applied (`kubectl get ingress go-proxy -o yaml`) and are the right family for your controller (§6.4). |
| `kubectl get svc go-proxy` shows `LoadBalancer` | Deployed without `--ingress-host` to a local cluster, or with `--public-load-balancer` | Redeploy with `--ingress-host` (and without `--public-load-balancer`). |
| `deploy.sh`: `…auth.resourceUrl is "…", but this run publishes go-proxy at "…"` | The cfg's `auth.resourceUrl` doesn't match `--ingress-host` (or `http`/`https` differs: it's `https` only with `--tls-secret` or `--cluster-issuer`) | Fix `auth.resourceUrl` (§4.1), or the flags. |
| `kubectl get certificate -n mcp-proxy` stays `READY False` (with `--cluster-issuer`) | The ACME challenge can't reach the cluster: DNS for HOST not pointing at the ingress controller, port 80 blocked, or wrong issuer name | `kubectl describe certificate,challenge -n mcp-proxy`; check §5.2's DNS and that the `ClusterIssuer` exists (`kubectl get clusterissuer`). |
