# The Event Bus: Who May Publish and Read Events

The gateway's events travel over **NATS**, a lightweight message bus deployed with the proxy. This document explains how the bus is protected, what each component may do on it by default, and how to let another component, such as a new platform server, publish or read events.

For what the events are and how the event aggregator stores them, see [Event Based Observability](../README.md#event-based-observability) in the README.

## Contents

1. [The default permissions](#1-the-default-permissions)
2. [How the bus is protected](#2-how-the-bus-is-protected)
3. [Letting another component use the bus](#3-letting-another-component-use-the-bus)
4. [Reading events](#4-reading-events)
5. [Publishing events: keep the audit trail trustworthy](#5-publishing-events-keep-the-audit-trail-trustworthy)
6. [Checking the result](#6-checking-the-result)

---

## 1. The default permissions

Every component that connects to NATS has its own NATS user, allowed exactly the subjects it needs. Events are published on subjects of the form `events.<type>.<subtype>`. Configuration-change notifications use `_system.config.<section>`.

| Component | Publishes | Subscribes to |
|---|---|---|
| **go-proxy** (the gateway) | All events (`events.>`) and configuration-change notifications | Configuration-change notifications |
| **event-aggregator** | Nothing | All events, and the notifications for its own configuration sections |
| **declarative-guardrail**, **rate-limiter**, **ai-powered-tools** | Nothing | The notifications for their own configuration sections only |

So, **by default, only the gateway can publish events, and only the event aggregator can read them.** No component can send another component's configuration notifications.

## 2. How the bus is protected

Four layers, all set up by `deploy.sh`:

- **No anonymous connections.** Every client must log in with its own user name and password.
- **TLS only.** NATS refuses plain-text connections. Its certificate is signed by a private CA that `deploy.sh` creates once; the CA's private key is discarded after signing, so it's never stored in the cluster.
- **Per-user permissions.** Each user may publish and subscribe only to its own subjects (the table above), defined in [`k8s/nats/nats.conf.tmpl`](../k8s/nats/nats.conf.tmpl).
- **Network isolation.** A NetworkPolicy, [`k8s/networkpolicy-allow-backends-to-nats.yaml`](../k8s/networkpolicy-allow-backends-to-nats.yaml), lets only the components above reach NATS at all, by their `app` label.

On top of that, every event is checked against the [event dictionary](../README.md#the-event-dictionary-defined-in-configuration-not-in-code), both when it's published and when it's received. An event that doesn't match is dropped.

## 3. Letting another component use the bus

Allowing a new component is configuration, not a change to the gateway or NATS. The component must run as a pod in the proxy's namespace (by default `mcp-proxy`). NATS isn't reachable from outside the cluster or from other namespaces. All four steps are needed; without any one of them, the component can't connect.

**1. Give it a NATS user** in [`k8s/nats/nats.conf.tmpl`](../k8s/nats/nats.conf.tmpl), with only the subjects it needs. For example, for a component named `alert-notifier` that reads alerts:

```
{
  user: "alert-notifier"
  password: "@@password:alert-notifier@@"
  permissions: {
    publish: { deny: [">"] }
    subscribe: { allow: ["events.alert.>"] }
  }
}
```

Keep the password placeholder as shown; `deploy.sh` fills it in.

**2. Create its credentials:** add its name to `NATS_COMPONENTS` in [`scripts/lib/deploy-proxy.sh`](../scripts/lib/deploy-proxy.sh). On the next `./scripts/deploy.sh --deploy-proxy`:
- `deploy.sh` creates the `nats-client-alert-notifier` Secret, with the user name, a random password and the CA certificate.
- It renders the NATS configuration with that password.
- It restarts NATS, because its configuration changed.

Existing passwords are kept. If the template names a user that isn't in `NATS_COMPONENTS`, the deploy stops with an error rather than starting NATS with a placeholder password.

**3. Mount the credentials** in the component's Deployment, and set `NATS_CREDENTIALS_DIR` to where they're mounted. [`ai-mcp-servers/ai-powered-tools/k8s/deployment.yaml`](../ai-mcp-servers/ai-powered-tools/k8s/deployment.yaml) is an example:

```yaml
env:
  - name: NATS_CREDENTIALS_DIR
    value: /etc/nats-client
volumeMounts:
  - name: nats-client
    mountPath: /etc/nats-client
    readOnly: true
volumes:
  - name: nats-client
    secret:
      secretName: nats-client-alert-notifier
```

**4. Let it reach NATS:** add its `app` label to [`k8s/networkpolicy-allow-backends-to-nats.yaml`](../k8s/networkpolicy-allow-backends-to-nats.yaml).

**In the component's code,** connect with the `events` package (Go: `events.NewClient(natsURL)`, with `natsURL` `nats://nats:4222`). It reads the credentials and the CA from `NATS_CREDENTIALS_DIR` and connects over TLS. The component also needs the event dictionary, since every event it sends or receives is checked against it. The event aggregator, for example, mounts the `event-dictionary-override` ConfigMap and watches it for changes.

If you change the set of users, also update the user list in `natsclient/natsclient_test.go`, which checks every user's permissions against the real template.

## 4. Reading events

A component that only reads events, such as one that forwards alerts to a chat or paging tool, needs nothing beyond the four steps above:
- **Subscribe** with `events.Client.Subscribe`.
- **Choose the subjects.** Allow only the subjects it needs, for example `events.alert.>` for alerts only, rather than `events.>`.
- **Allow no publishing** (`publish: { deny: [">"] }`).

Events carry the user, their roles, the tool and the call's request, and sometimes its result. Grant read access as you would grant access to an audit log.

## 5. Publishing events: keep the audit trail trustworthy

Publishing needs one decision beyond configuration. Today the audit trail can trust every event on `events.>` because **only the gateway can publish there**. An event doesn't say which component sent it, so the NATS permissions are the only guarantee that a `tool-request-result` or `alert` event came from the gateway.

A second component allowed to publish on `events.>` could therefore send events that look exactly like the gateway's: a fake "call succeeded", a missing "blocked", or alerts for calls that never happened.

To add a publisher without weakening that guarantee:
- **Give it event types of its own** in the event dictionary, for example `notifier-status`, and allow it only those subjects: `publish: { allow: ["events.notifier-status.>"] }`. Never allow it `events.>` or the gateway's own types.
- **Let the aggregator receive them.** The aggregator already subscribes to `events.>`, so it stores the new type with no change. Its ingestion filter can still exclude it.
- **Record the source** in the event's metadata if readers need it. Only the subject is enforced by NATS, so readers should rely on the type, not on a field the publisher sets.

The gateway's own types (`alert`, `tool-request`, `tool-request-result`) should stay the gateway's alone.

## 6. Checking the result

After deploying:
- **NATS restarted with the new configuration:** `kubectl -n mcp-proxy rollout status deployment/nats`.
- **The component connected:** check its logs. A refused login or a missing Secret is logged by the component; a permission violation is logged by NATS (`kubectl -n mcp-proxy logs deployment/nats`).
- **Its events arrive:** for a publisher, query the event aggregator with the CLI's `list-events -type <type>`.
