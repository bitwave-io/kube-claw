# kube-claw — Kubernetes-Native Agent Runner Design

Status: draft v0.2
Supersedes: v0.1 (2026-06-20)
Date: 2026-06-20
Target implementation: Go, Kubernetes, Helm, controller-runtime, SQLite (modernc pure-Go), Google Tink
Audience: implementation agent or engineer

---

## 0. What changed from v0.1 (and why)

v0.1 designed a broad platform. v0.2 narrows to a **fully baked MVP for one use case** (Slack → GCP cost bot) while keeping the differentiated core (human-approved, image-digest-bound secret release). Decisions locked during eng review:

| Area | v0.1 | v0.2 | Why |
|---|---|---|---|
| Name | Claw | **kube-claw** (CLI/API stay `claw` / `claw.run`) | Match repo; short command like `kubectl` |
| CRD surface | 10 CRDs | **1 CRD: `Agent`** | Everything else folds into Agent spec, SQLite rows, or Helm config |
| State store | BadgerDB + hand-rolled indexes | **Pluggable `Store` iface; SQLite default** | Free indexes/joins/migrations; SQLite for v0, Postgres/Spanner conformant later (also the HA path) |
| Secret leases | Timed leases + renewal + expiry | **Dropped entirely** | No lease timers (idle + approval timeouts still exist). Access bounded by run/pod lifetime + revocation |
| Grant scope | Time-bounded | **Durable until revoked / image-or-spec change** | Simpler; digest+spec hash is the safety boundary |
| Approval | CLI first, Slack later | **Slack PAM approval is the MVP path** (CLI = break-glass) | PAM-through-Slack is the product UX |
| Granters | implicit | **Configurable per secret** | PAM: each secret declares who may approve |
| Crypto | hand-rolled envelope (AES-GCM + DEK) | **Google Tink** (AEAD + envelope + KMS) | Boring-by-default; eliminates nonce/DEK bug class; KMS path free |
| Secret backends | interface + 2 backends | **concrete local store only** | No abstraction before a second impl exists |
| Delivery | `claw-bootstrap` wrapper | **two binaries + subprocess + CLI contract** | Generic pluggable runner; 100% failure pass-through |
| Runtime | scaleToZeroSession default | **scaleToZeroSession (kept)** | Persistent session/workspace wanted |
| Agent identity | implicit (raw SA token per call) | **First-class, pluggable `/login` token exchange** | Runner gets a scoped claw token, not the SA token; external auth sources (OIDC/SPIFFE/IAM) later |

Deferred (not deleted — see §16): `Connector` / `BaseImage` / `StorageProfile` / `ScheduledAction` / `DevEnvironment` / `SecretPolicy` CRDs, `SecretBackend` abstraction, `kubernetes.secret` backend, env-var delivery, warmPool, controller HA / multi-replica.

---

## 1. Executive summary

kube-claw is a Kubernetes-native agent runner control plane, installed and upgraded with Helm. An always-running controller receives external events (Slack), creates agent runs, wakes scale-to-zero agent pods, releases approved secrets to attested pods, and scales pods back down when idle.

The differentiated core is the **secret authority**: kube-claw is not a thin wrapper around Kubernetes Secrets. The controller owns an encrypted secret store (SQLite + Tink envelope encryption), human-in-the-loop **PAM-style approval through Slack**, image-digest-bound grants, attested runtime delivery, and an append-only audit log.

First use case: a Slack-connected GCP cost monitoring agent. A Slack message wakes a sleeping agent pod. The agent receives only the approved GCP read-only billing key, uses normal GCP SDKs/CLIs, and replies to Slack.

---

## 2. Goals

1. Helm-deployed Kubernetes-native runner for AI agents.
2. Always-running control plane that wakes, routes to, and manages agent pods.
3. Treat the **Agent** as the single first-class CRD; treat secrets, grants, runs, sessions, and audit as controller-owned state.
4. Scale-to-zero agent pods while keeping the Slack connector alive.
5. Allow agents to receive raw credentials, but only when explicitly approved by a configured granter.
6. Ensure agents receive exactly the secrets they need, no more.
7. Store sensitive control-plane state in an embedded SQL database (SQLite, pure-Go).
8. Make secret release explicit, scoped to an agent + image digest, auditable, and revocable.
9. Provide a clear CRD and CLI/API surface so the system is automatable and debuggable.
10. Build the MVP in Go with a clean path to production hardening (KMS, HA).

---

## 3. Non-goals for v0

1. No full admin UI. A **minimal secret-intake web page** is in scope (§8.3); broader inspect/admin UI is deferred.
2. No hard dependency on Knative, KEDA, Argo, Vault, or External Secrets.
3. No brokered tools for every external service — raw secret delivery is supported.
4. Credentials are not required to remain hidden from the agent.
5. No active-active controller replicas in v0 (single replica, fail-closed).
6. No multi-tenant billing, orgs, or complex RBAC UI.
7. No broad Kubernetes API permissions for agent pods.
8. No materialization of kube-claw-managed secrets into Kubernetes Secrets.
9. **No timed secret leases** — access is bounded by run/pod lifetime and explicit revocation.

---

## 4. High-level architecture

```text
                    Slack (Socket Mode: events + interactive approvals)
                                  │
                                  ▼
   ┌──────────────────────────────────────────────────────────────┐
   │  claw-controller   (StatefulSet, 1 replica, fail-closed)       │
   │                                                                │
   │  ┌──────────────┐   ┌───────────────────┐                      │
   │  │ router pkg   │ → │ run engine        │   AgentRun = SQLite   │
   │  │ (slack)      │   │ (reconcile/wake)  │   row (NOT a CRD)     │
   │  └──────┬───────┘   └─────────┬─────────┘                      │
   │         │ approval callbacks  │                                 │
   │  ┌──────▼─────────────────────▼─────────────────────────────┐  │
   │  │ secret authority                                          │  │
   │  │   request → Slack approval (granters) → durable grant     │  │
   │  │   encrypt/decrypt via Tink (KMS-pluggable master key)     │  │
   │  └───────────────────────────┬──────────────────────────────┘  │
   │  ┌───────────────────────────▼──────────────────────────────┐  │
   │  │ store: SQLite (modernc) — one file on RWO PVC             │  │
   │  │   agents-runtime, secrets, grants, requests, audit,       │  │
   │  │   sessions, dedupe, schema_version                        │  │
   │  └──────────────────────────────────────────────────────────┘  │
   │  HTTP API:  /v1/workloads/attest   /v1/workloads/{run}/secrets   │
   │             /v1/runs ...  /v1/secret-requests ...               │
   └──────────────────────────────┬─────────────────────────────────┘
                                   │ creates/scales Deployment 0↔1
                                   ▼
              ┌──────────────────────────────────────┐
              │  Agent pod (scaleToZeroSession)        │
              │  PID1: /claw/bootstrap                 │
              │    attest → materialize → exec runner  │
              │  child: /claw/runner (or yours)        │
              │  secret in tmpfs (memory emptyDir)     │
              └──────────────────────────────────────┘
```

One long-lived service: `claw-controller`. The Slack router is an internal package in the controller binary (no separate deployment in v0). One active replica; SQLite is embedded and single-writer. HA is deferred (leader election + warm standby, or a networked store).

---

## 5. Deployment model

Two Helm charts:

```text
charts/
  crds/              claw.run_agents.yaml     # the ONE CRD (raw manifest, kubectl-applied)
  claw/
    templates/controller-statefulset.yaml
    templates/service.yaml
    templates/rbac.yaml
    templates/networkpolicy.yaml
    templates/ingress.yaml                      # ONLY /ui/secret-intake/*, TLS (§8.3)
    templates/pvc.yaml                          # via volumeClaimTemplate
    values.yaml                                 # slack tokens ref, master key ref, ui host, etc.
```

```bash
kubectl apply -f ./charts/crds/          # CRDs: kubectl, not Helm (Helm never upgrades crds/)
helm upgrade --install claw ./charts/claw
```

Controller is a StatefulSet, `replicas: 1`, `volumeClaimTemplate` for `/var/lib/claw` (SQLite + master key material refs). Ports: `8443` API, `8080` metrics.

> **Blast radius (accepted for MVP):** single replica + SQLite on RWO PVC is a deliberate SPOF. Controller down ⇒ all runs blocked (fail-closed for secrets, by design). Recovery story: StatefulSet reschedules, PVC reattaches (RWO same-node or after detach). HA deferred.

> **Superseded post-MVP by §24 (self-update plane):** from chart ≥0.4.0 the chart no longer templates the controller StatefulSet directly — it installs `claw-supervisor`, which owns and reconciles it. This section describes the MVP as built (chart ≤0.3.x).

---

## 6. The Agent CRD (the only CRD)

`Agent` is the single user-facing resource. BaseImage, storage, and secret refs are inline fields, not separate CRDs.

```yaml
apiVersion: claw.run/v1alpha1
kind: Agent
metadata:
  name: gcp-cost
  namespace: claw-agents
spec:
  displayName: GCP Cost Monitor

  image: ghcr.io/example/claw/gcp-cost@sha256:abc123   # MUST be a digest
  digestRequired: true

  runtime:
    mode: scaleToZeroSession
    idleTimeout: 15m
    coldStartReply: "Checking GCP cost data..."

  command:                       # entrypoint = bootstrap, then runner
    - /claw/bootstrap
    - --runner=/claw/runner
    - --

  model:
    providerRef: default-llm
    systemPrompt: |
      You are a read-only GCP cost monitoring assistant.
      Explain costs, summarize trends, identify anomalies. Do not modify resources.

  storage:                       # inline (no StorageProfile CRD)
    workspace: { size: 10Gi, mountPath: /workspace }
    memory:    { size: 5Gi,  mountPath: /memory }
    cache:     { type: emptyDir, mountPath: /cache }

  secrets:                       # names + delivery only — values live in the store
    - name: gcp-billing-readonly
      delivery:
        type: file
        path: /var/run/claw/secrets/gcp-billing.json
        mode: "0400"
        env:
          GOOGLE_APPLICATION_CREDENTIALS: /var/run/claw/secrets/gcp-billing.json

  network:
    egressAllowHosts:
      - bigquery.googleapis.com
      - billingbudgets.googleapis.com
      - cloudbilling.googleapis.com

status:
  phase: Sleeping                # Sleeping|Waking|Running|Blocked|Failed
  selectedImageDigest: sha256:abc123
  agentSpecHash: sha256:def456
  conditions:
    - { type: Ready, status: "True" }
    - { type: SecretGrantsReady, status: "False", reason: ApprovalRequired }
```

The `delivery` block is defined **once here** (DRY). Grants store only a content hash of the approved delivery spec, which also powers the "delivery changed → re-approve" rule.

The CRD enforces that `image` contains an `@sha256:` digest via a CEL validation rule (`x-kubernetes-validations`) — a tag is rejected at apply time, so the digest trust boundary holds without an admission webhook.

---

## 7. State store — pluggable, SQLite default

Controller state lives behind a `Store` interface (typed repository methods, not raw SQL
passthrough) so a more sophisticated shop can swap the engine. The v0 default is **SQLite**
(modernc.org/sqlite, pure-Go, no cgo), one file at `/var/lib/claw/claw.db`. Postgres and Cloud
Spanner are conformant targets — and a networked store is also the HA path, since it removes the
single-replica/RWO-PVC SPOF of §5.

```go
type Store interface {
    // Secret-state mutations write their audit row in the SAME tx (invariant below).
    Tx(ctx context.Context, fn func(Tx) error) error   // serializable isolation
    // typed repository methods hang off Tx: CreateSecret, PutVersion, CreateGrant,
    // RevokeGrant, ListGrantsByAgent, AppendAudit, ... implemented per backend
    Close() error
}
```

MVP builds **only** the SQLite implementation. The interface stays portable: no SQLite-specific
SQL leaks past it; schema + migrations are per-implementation; SQLite tuning (WAL, `busy_timeout`)
lives inside the SQLite impl, never in the interface.

Core tables (illustrative — the SQLite implementation):

```sql
schema_version(version INTEGER)

secrets(
  id TEXT PRIMARY KEY, namespace TEXT, name TEXT,
  type TEXT, labels JSON, created_at, UNIQUE(namespace,name))
secret_granters(secret_id TEXT, principal TEXT)          -- PAM: who may approve
secret_versions(
  id TEXT PRIMARY KEY, secret_id TEXT, ciphertext BLOB,   -- Tink-encrypted
  created_at, created_by, checksum TEXT)

grants(
  id TEXT PRIMARY KEY, agent_ns TEXT, agent_name TEXT,
  service_account TEXT, image_digest TEXT, agent_spec_hash TEXT,
  secret_id TEXT, secret_version TEXT, delivery_hash TEXT,
  approved_by TEXT, approved_at, reason TEXT,
  revoked_at, revoked_reason)                              -- NO expiry column
secret_requests(
  id TEXT PRIMARY KEY, status TEXT, agent_ns, agent_name,
  run_id, secret_id, image_digest, context JSON, created_at)

runs(
  id TEXT PRIMARY KEY, agent_ns, agent_name, session_id,
  phase TEXT, source JSON, input JSON, assigned_pod, pod_uid,
  started_at, completed_at)
sessions(id TEXT PRIMARY KEY, agent_ns, agent_name, key TEXT)
dedupe(source TEXT, event_id TEXT, seen_at, PRIMARY KEY(source,event_id))
intake_tokens(token_hash TEXT PRIMARY KEY, secret_id TEXT, created_at, expires_at, consumed_at)  -- §8.3
audit(id TEXT PRIMARY KEY, ts, type TEXT, run_id, grant_id, secret_id, actor, detail JSON,
      prev_hash TEXT, row_hash TEXT)        -- hash-chained → tamper-evident

CREATE INDEX grants_by_agent  ON grants(agent_ns, agent_name);
CREATE INDEX grants_by_secret ON grants(secret_id);
CREATE INDEX audit_by_ts      ON audit(ts);
```

Store API exposes secret-state mutations only through methods that write the state row **and** the audit row in the same transaction (see §11) — "forgot to audit" can't compile.

Never store plaintext secrets. Never store secret values in CRD specs/status.

---

## 8. Secret authority

```text
ClawSecret (store row + Tink-encrypted version) — NOT a CRD
  ├── granters: [slack-user | slack-group]   ← PAM: configurable per secret
  └── value: envelope-encrypted blob (Tink)

Run needs secret X:
  ├── valid grant exists?  → reuse, materialize to attested pod
  └── no valid grant?      → SecretRequest → router posts interactive Slack
                              approval to the secret's granters
        ├── approve  → durable SecretGrant (agent + ns + SA + image digest
        │              + spec hash + delivery hash). NO expiry. NO lease.
        └── deny     → run → Failed, audited

Grant is INVALID when:  explicitly revoked  OR  image digest changed  OR  agent spec
                        hash changed  OR  delivery hash changed  OR  secret VERSION changed
Access ENDS when:       pod dies  OR  grant invalidated (running pod actively killed)
Honest bound:           revocation stops FUTURE materialization within a propagation delay;
                        it does NOT claw back a credential the agent already loaded or used.
                        /materialize serves the grant's PINNED version; a new version re-approves.
```

Rule: agents may receive raw credentials, but only if those exact credentials are explicitly granted to that exact agent/digest by a configured granter. No brokered-tool requirement.

### 8.1 Slack PAM approval (MVP path)

The router posts an interactive Slack message (approve/deny buttons) to the secret's granters over
**Socket Mode** (outbound WebSocket — no public Slack endpoint; the only public surface is the §8.3
intake page). Interaction callbacks arrive as WebSocket envelopes the router acks; there is **no
per-message HMAC signature** (that exists only on Slack's HTTP Events API, which we do not use). The
action-callback handler (security-sensitive — kept separate from plain event handling) must independently:

1. Accept callbacks only from the authenticated (app-token) Socket Mode connection; reject anything else.
2. Confirm the clicking `user.id` is in that secret's `granters`.
3. Be idempotent against double-click / replay (request still `Pending`).

Trust note: Slack identity is the root of release authority. Slack group membership is managed outside
kube-claw, and a compromised Slack account carries release authority — there is no second factor in v0
(TODO: optional second factor for high-sensitivity secrets). CLI approval (`claw secret approve/deny`)
remains a break-glass + e2e path, not the primary UX.

### 8.2 Encryption (Tink)

Envelope encryption via Google Tink: per-secret-version data key, wrapped by a master key behind a **KMS interface from day 1** — local keyset for dev, Cloud KMS / Vault transit for prod. No hand-rolled AEAD, nonces, or DEK wrapping.

```text
plaintext → Tink AEAD (DEK) → ciphertext blob in SQLite
DEK       → wrapped by master key (KMS or local keyset)
```

Logging rule: never log plaintext, decrypted keys, full token values, or file contents.

### 8.3 Secret intake (one-time link UI)

Secret values are never passed on the CLI (shell history / process args / scrollback leak them).
`claw secret create` writes metadata + granters and mints a single-use intake token; the value
is submitted through a minimal web form.

```text
claw secret create NAME --type T --granter @alex     # metadata only, NO value
  → mint intake token (256-bit CSPRNG; store only its hash; TTL 15m; single-use)
  → return  https://<claw-ui>/ui/secret-intake/{token}

GET  /ui/secret-intake/{token}  → minimal HTML form (CSRF token; value never echoed)
POST /ui/secret-intake/{token}  → value over TLS
     verify token exists + unexpired + unconsumed
     → Tink-encrypt → write secret_version → mark token consumed
     → audit secret.version_added (value NEVER logged) → render "stored" (no echo)
```

Reachability: a dedicated **Ingress with TLS** — a real link, DM-able to a non-kubectl human
who owns the credential. Because the one-time token is the ONLY guard on a public endpoint,
these controls are load-bearing (and tested, §18.x):

- Token: 256-bit CSPRNG, stored as hash only, single-use, 15m TTL.
- The public page is served by a **separate net listener / mux** with zero `/v1/*` routes registered — defense beyond the Ingress rule, so a routing fat-finger cannot reach the internal API.
- The Ingress exposes **only** `/ui/secret-intake/*` — never `/v1/*` or any other path.
- Rate-limit the endpoint; generic 404 for invalid/expired/consumed tokens (no oracle).
- TLS required; CSRF on the form POST; the value never appears in logs or error bodies.
- The submitter is **unauthenticated**: the one-time token guards *who can write*, not *what value or who submits* — an attacker with the link could submit their own credential. Mitigation (deferred): bind the link to a verified granter.

`claw secret put --from-file` remains as a break-glass / CI path. Broader inspect/admin UI is deferred (§16).

---

## 9. Agent identity & login (pluggable)

Agent identity is first-class and pluggable. A pod does not authenticate with its raw
Kubernetes token on every call; the bootstrapper performs a one-time **`/login` token
exchange** and hands a short-lived, claw-issued session token to the runner.

```text
IdentityProvider (interface)              default: KubernetesSAProvider
  Verify(ctx, credential) → Principal      future: OIDC / SPIFFE / cloud IAM
```

Login flow:

```text
1. Router creates AgentRun.  Run engine computes required secrets.
2. No valid grant → SecretRequest → Slack approval (§8.1) → durable SecretGrant.
3. Controller wakes the agent Deployment (0→1) with labels/annotations
   (claw.run/agent, run-id, session-id; annotation claw.run/image-digest) and a
   projected ServiceAccount token (audience claw-controller).
4. bootstrap reads the platform credential and calls POST /v1/login {credential, run-id}.
5. IdentityProvider.Verify (KubernetesSAProvider): TokenReview the token, then take the
   pod name+UID from the token's BOUND claims (kubernetes.io/serviceaccount.pod.{name,uid})
   — NOT from the run row or a bootstrap arg — and confirm that pod is a kube-claw-owned
   pod for this run.
6. Controller verifies grant constraints: namespace, ServiceAccount, image digest,
   agent-spec hash, delivery hash, and a valid (non-revoked) grant per requested secret.
7. Controller issues a CLAW SESSION TOKEN: claw-signed, short TTL, scoped to this run +
   exactly the approved secrets. Returns it to bootstrap.
8. bootstrap hands CLAW_TOKEN to the runner (env) and materializes approved secrets to tmpfs.
9. runner uses CLAW_TOKEN (bearer) for /materialize + /runs/{id}/outputs — never the raw token.
```

Why first-class identity:
- **Decoupling:** the runner holds a narrowly-scoped claw token, not the pod's full SA credential.
- **Pluggable:** the credential verifier is an interface — a future agent can authenticate against an external identity source without changing the rest of the system.
- **Replay-safe:** the pod UID comes from the token's bound claims, so a co-resident pod of the same Agent cannot present another run's id (closes the same-namespace replay gap).

Threat-model note: the runner shares the pod and *can* read the raw SA token on disk. The design accepts this and relies on the scoped claw session token + grant constraints, not on the runner being unable to see the platform credential.

---

## 10. Runtime mode — scaleToZeroSession

```text
event → AgentRun → scale Deployment 0→1 → process → idle timeout → scale 1→0
```

Each Slack thread maps to a session; the session pod keeps `/workspace` + `/memory` (PVC) warm across messages. Idle-timeout reconciler scales the Deployment back to 0.

> Because the pod is long-lived, a grant revoked (or invalidated by image/spec/version/delivery change)
> while a session pod is alive must **kill the pod** — there is no lease to expire. An **active reconcile
> loop** scans live pods against grant validity and enqueues an immediate pod delete (not lazy at the next
> `/login`), with SIGKILL fallback after the grace period for runners that ignore SIGTERM. bootstrap **wipes
> the tmpfs secret on exit**. Honest bound: this stops *future* use within a propagation delay; a credential
> already in the agent's memory or already used cannot be clawed back. State that plainly rather than
> implying instant revocation — it is acceptable for a read-only billing key, less so as a general claim.

`oneShotJob` and `warmPool` modes are deferred (§16).

### 10.1 Agent memory (design note, not MVP-blocking)

The agent's own SQLite memory lives on its `/memory` PVC and survives scale-to-zero (RWO, single pod at a time). Snapshotting the memory DB to object storage (GCS) on a pre-stop hook is a future option for cross-node / cross-PVC durability — not needed for MVP.

---

## 11. Secret delivery — two binaries, subprocess, CLI contract

```text
TWO default Go binaries, separate processes, documented contract.

/claw/bootstrap   (container entrypoint, PID 1)
  --run-id --controller-url --secrets-dir=/var/run/claw/secrets
  --runner=/claw/runner  --  [args passed verbatim to runner]
    ├─ login: POST /v1/login (token exchange, §9) → CLAW session token
    ├─ materialize (with CLAW token): write secret files to tmpfs secrets-dir, build env
    ├─ fork/exec /claw/runner [args] with env:
    │     CLAW_TOKEN (scoped claw session token — runner uses this, not the SA token),
    │     CLAW_RUN_ID, CLAW_AGENT_NAME, CLAW_AGENT_NAMESPACE, CLAW_SESSION_ID,
    │     CLAW_CONTROLLER_URL, CLAW_SECRETS_DIR, CLAW_INPUT_FILE,
    │     CLAW_WORKSPACE_DIR=/workspace, CLAW_MEMORY_DIR=/memory,
    │     GOOGLE_APPLICATION_CREDENTIALS=/var/run/claw/secrets/gcp-billing.json
    ├─ wait(child) → 100% pass-through: forward exit code + signals
    ├─ wipe secrets-dir (tmpfs)
    └─ POST run-completed audit → exit(childExitCode)

/claw/runner      (default generic agent loop — reference implementation)
  reads CLAW_INPUT_FILE + creds from CLAW_SECRETS_DIR/env, runs the loop,
  POSTs outputs to CLAW_CONTROLLER_URL /v1/runs/{id}/outputs,
  exits 0 (ok) / non-zero (failure → passed through to the pod).
```

The contract is the **CLI flags + `CLAW_*` env + tmpfs paths**, documented in the OSS repo so anyone can write a conforming runner in any language. The default Go runner is one reference implementation. `cmd/claw-bootstrap` and `cmd/claw-runner` share one attest/materialize client package (DRY).

Implementation invariants:
- **bootstrap is PID 1** → forwards SIGTERM/SIGINT to the child and reaps zombies (or pods won't terminate cleanly).
- **secret wipe on exit** → bootstrap removes/zeroes the tmpfs secret files after the runner exits.
- **failure handling** → 100% pass-through in v0; configurable retry/handling deferred (TODO).

> Security note: because grants bind to the image digest, **runner code must be baked into the image** (compile-in, COPY into the image at a pinned digest). A runner that fetches code from git at runtime defeats the digest binding (the digest no longer attests what code touches the secret). Git-pointer runners belong to the deferred `DevEnvironment`, not the secret path.

---

## 12. Slack connector

Slack config lives in Helm values / controller config (no `Connector` CRD in v0):

```yaml
slack:
  delivery: socketMode
  appTokenSecretRef:  { name: slack-app-token }   # kube-claw store, router-owned
  botTokenSecretRef:  { name: slack-bot-token }
  events: [app_mention, message.im]
  routes:
    - match: { channels: ["#cloud-costs"], mentionRequired: true }
      agentRef: { namespace: claw-agents, name: gcp-cost }
      session: { key: slackThread }
```

The router owns Slack tokens. The agent owns the GCP billing grant. The agent submits reply payloads to the controller; the router posts them to Slack. This minimizes Slack token exposure while letting the agent use arbitrary GCP libraries with a normal credential file.

Socket Mode carries both **events** and **interactive approval callbacks** (§8.1).

---

## 13. Internal HTTP API

```text
POST /v1/secrets                 PUT /v1/secrets/{name}/versions
GET  /v1/secrets/{name}/metadata

GET  /v1/secret-requests         GET  /v1/secret-requests/{id}
POST /v1/secret-requests/{id}/approve   POST /v1/secret-requests/{id}/deny
GET  /v1/secret-grants           POST /v1/secret-grants/{id}/revoke

POST /v1/login                                    # §9 identity-provider token exchange
POST /v1/workloads/{runID}/secrets/materialize    # authed by claw session token

POST /v1/runs   GET /v1/runs/{runID}   POST /v1/runs/{runID}/outputs   # authed by claw session token

POST /v1/connectors/slack/events     POST /v1/connectors/slack/interactions

GET  /ui/secret-intake/{token}       POST /ui/secret-intake/{token}    # §8.3, Ingress-exposed
```

Internal auth: Kubernetes ServiceAccount projected-token validation (TokenReview) + strict NetworkPolicy. mTLS deferred.
The `/ui/secret-intake/*` paths are the only Ingress-exposed routes and are guarded solely by the one-time token (§8.3); everything else is cluster-internal.

---

## 14. CLI

```bash
claw install check
claw secret create NAME --type TYPE --granter SLACK_ID [--granter ...]   # prints one-time intake link (§8.3)
claw secret put NAME --from-file PATH                                    # break-glass / CI only
claw secret metadata NAME
claw secret requests list
claw secret approve REQUEST_ID --reason TEXT     # break-glass; Slack is primary
claw secret deny    REQUEST_ID --reason TEXT
claw secret grants list
claw secret grant revoke GRANT_ID --reason TEXT
claw run create --agent NAME --input TEXT   # trigger a run directly (POST /v1/runs) — no Slack
claw runs list | show RUN_ID | logs RUN_ID
claw agents list | wake AGENT | sleep AGENT
```

---

## 15. Go repository layout

```text
cmd/
  claw-controller/main.go
  claw/main.go              # CLI
  claw-bootstrap/main.go    # PID1 entrypoint
  claw-runner/main.go       # default reference runner

api/v1alpha1/
  agent_types.go            # the ONLY CRD type

internal/
  controller/
    agent_controller.go     # resolve image, ensure SA/RBAC/PVC/NetworkPolicy, scale 0↔1, status
  runengine/
    engine.go               # AgentRun lifecycle (SQLite-backed), compute secrets, wake/idle
  store/
    store.go
    sqlite/  db.go  migrations.go  queries.go
  secrets/
    service.go  encryption_tink.go  requests.go  grants.go  audit.go
  identity/
    provider.go  kubernetes.go  token.go   # §9 pluggable identity + claw session token
  workloads/
    materialize.go  pod_builder.go  lifecycle.go
  router/
    router.go  slack/socketmode.go  events.go  interactions.go  replies.go
  workloadclient/           # shared by bootstrap + runner: login + materialize
  apihttp/
    server.go  auth.go  handlers.go  ui_intake.go   # §8.3 one-time secret-intake page

charts/  crds/  claw/
examples/ gcp-cost-slack/  { agent.yaml, secrets.yaml, values-slack.yaml }
```

---

## 16. Deferred (designed in v0.1, out of scope for the MVP)

Kept for reference; build when a real need appears. Each is a one-line rationale.

- `Connector` CRD — Slack config is Helm values for one connector; CRD when multi-connector.
- `BaseImage` CRD — Agent has an inline image digest; CRD when image catalogs/capability selection matter.
- `StorageProfile` CRD — inline on Agent; CRD when storage is shared across many agents.
- `ScheduledAction` CRD + internal scheduler — daily digest; add after the core loop (then likely K8s CronJob).
- `DevEnvironment` CRD — human dev pods + git-pointer runners; not the secret path.
- `SecretPolicy` CRD — approval rules are a hardcoded default first.
- `SecretBackend` abstraction + `kubernetes.secret` / cloud backends — concrete local Tink store only.
- External identity providers (OIDC / SPIFFE / cloud IAM) — MVP ships only the Kubernetes SA provider (§9); the interface exists, the extra impls do not.
- Non-SQLite `Store` backends (Postgres / Spanner) — interface exists (§7), MVP implements SQLite only.
- Env-var delivery mode — file delivery covers GCP; env is higher-risk and unneeded.
- `warmPool` / `oneShotJob` runtime modes — only scaleToZeroSession for MVP.
- Controller HA / multi-replica — single replica, fail-closed; HA via leader election later.
- Configurable runner failure handling / retry — 100% pass-through for now.

---

## 17. MVP implementation plan

```text
Phase 0  Repo + build skeleton: Go module, controller-runtime, Agent CRD gen,
         SQLite store + migrations, Helm skeleton, CLI skeleton.
         AC: make test passes; make manifests gens Agent CRD; helm template renders.

Phase 1  Agent CRD + AgentReconciler: resolve image digest, ensure SA/RBAC/PVC/
         NetworkPolicy, status conditions.
         AC: applying Agent produces meaningful status; bad refs → clear conditions.

Phase 2  SQLite store + audit: schema_version, migrations, tx helpers, audit-in-tx
         mutation methods, dedupe helpers, unit tests.
         AC: state survives restart; audit rows are written in the same tx as the change.

Phase 3  Secret store + Tink: encrypt/decrypt via Tink, KMS-interface master key
         (local keyset dev), CLI create/put, version listing, no plaintext in logs.
         AC: secret round-trips through the store; restart-safe; wrong master key cannot decrypt.

Phase 4  Approval + durable grants: SecretRequest, Slack interactive approval
         (signature verify, granter check, idempotent), durable SecretGrant,
         revoke + image/spec/delivery-hash invalidation. CLI break-glass approve/deny.
         AC: run needing an ungranted secret → Blocked; approval → grant; revoke blocks future.

Phase 5  Pod lifecycle + attestation + delivery: scaleToZeroSession 0↔1, idle reconciler,
         /v1/workloads/attest (TokenReview + Pod read + checks 1-9), materialize to tmpfs,
         claw-bootstrap (PID1, signal forward, exec, wipe), claw-runner contract.
         AC: approved run wakes pod; pod gets exactly the approved file; unapproved/wrong-UID/
             wrong-SA/wrong-digest cannot materialize.

Phase 6  Slack connector: socket-mode events + interactions, route match, dedupe,
         AgentRun creation, reply delivery from run outputs.
         AC: mention creates run; duplicate event → no duplicate run; reply in thread; sleeping pod wakes.

Phase 7  GCP cost example + hardening: example Agent + secrets + Slack values, reference runner
         querying billing (or mock), envtest controller tests, kind e2e, NetworkPolicy, RBAC review.
         AC: full §19 scenario green in kind; agent SA cannot read K8s Secrets.
```

**Testability — Slack is intentionally LAST.** The entire secret loop is exercisable
without Slack via `claw run create --agent NAME --input TEXT` → `POST /v1/runs` (a
first-class CLI command, not just test scaffolding) plus the CLI break-glass
`claw secret approve`. The shortest path to a testable end-to-end loop is Phases 1-5 +
the run-trigger CLI; Phase 6 (Slack) then adds a second *trigger* and *approval channel*
on top without changing the core. Build in that order — prove the trust loop before Slack.

---

## 18. Testing strategy

_(Coverage diagram, security matrix, and per-path test plan are produced by the eng-review Test section and appended below in §18.x. This section is the implementation-time test contract.)_

Unit: `store/sqlite`, `secrets/encryption_tink`, `secrets/grants`, `secrets/requests`,
`router/dedupe`, `router/interactions` (signature + granter + idempotency),
`identity/provider` (TokenReview + pod-UID-from-bound-claims), `identity/token` (issue/validate/expiry/scope),
`workloads/materialize`, `workloads/pod_builder`, `workloadclient`,
`apihttp/ui_intake` (token single-use, expiry, garbage→404, CSRF, rate-limit, value-never-logged).

Controller (envtest): Agent status reconciliation; run Blocked-on-approval; run wakes pod
after grant; idle timeout scales to zero; PVC/NetworkPolicy creation; grant invalidation
on image/spec/delivery change kills the live pod.

E2E (kind): helm install → apply example Agent → upload fake secret → fake Slack event →
run Blocked → approve (Slack interaction sim) → pod wakes → secret file present (content
never in logs) → output delivered to fake Slack sink → idle scale-down.

Security (must-have, see §18.x matrix): unapproved run cannot materialize; wrong pod UID;
wrong SA; wrong image digest; changed spec hash; changed delivery hash; revoked grant;
Slack approver not in granters; replayed/double-clicked approval; agent SA cannot list K8s Secrets;
bootstrap forwards SIGTERM and wipes tmpfs on exit.

### 18.x Coverage matrix (from eng review)

Greenfield: every path below ships its test alongside the feature, not after. The
16 security-critical paths ARE the product's safety claim — with no lease/expiry, the
whole trust boundary is attestation checks (1-9) + grant invalidation + Slack approver
authorization. Any one untested = unverified guarantee.

```
secret trust loop (unit unless noted)
  encryption_tink: round-trip | wrong-master-key-cannot-decrypt(SEC) | KMS-iface
  grants:          approve→grant | revoke-blocks(SEC) | image/spec/delivery-hash-change→invalid(SEC x3)
  interactions:    happy(E2E) | bad-signature-reject(SEC) | non-granter-reject(SEC) | replay-idempotent(SEC)
  login/identity:  happy(E2E) | pod-UID-from-bound-token-claims-not-run-row(SEC)
                   | co-resident-pod-replay→deny(SEC) | unapproved/wrong-SA/wrong-digest/revoked/bad-token→deny(SEC x5)
                   | claw-session-token expiry + scope enforced on /materialize(SEC)
controller (envtest)
  status | ensure SA/RBAC/PVC/NetworkPolicy | scale 0→1 | idle→1→0 | grant-invalidated→kill-live-pod
router
  dedupe: duplicate-event→no-dup-run
bootstrap (PID1)
  attest→materialize→exec→passthrough | forward-SIGTERM+reap(SEC) | wipe-tmpfs-on-exit(SEC)
runner loop
  prompt→cost-answer-quality  [EVAL suite, not a unit assert]
secret intake UI (§8.3)
  ui_intake: happy(form→store) | single-use-2nd-submit-fails(SEC) | expired-token→404(SEC)
             | garbage/consumed→generic-404-no-oracle(SEC) | CSRF-reject(SEC) | rate-limit(SEC)
             | value-never-in-logs(SEC) | Ingress-exposes-only-/ui/secret-intake(SEC)
isolation (kind e2e)
  agent ServiceAccount CANNOT list/read Kubernetes Secrets (SEC)
user flows (e2e)
  cold cost question | warm 2nd question same thread | question-while-grant-revoked | non-granter-approval-rejected
```

Sharpest no-lease edges (both must have a test): bootstrap-must-forward-SIGTERM (else
session pods hang on scale-down/revoke and the secret lingers in memory); and
grant-invalidation-must-kill-the-live-pod (else revoked access keeps working until idle
timeout). The second is the price of dropping leases — make it loud in tests.

---

## 19. MVP acceptance criteria

The MVP is complete when this works in kind or a real cluster:

1. Install with Helm.
2. Add Slack app+bot tokens and a (fake or real) GCP read-only billing key: `claw secret create` prints a one-time intake link; open it over TLS and paste each value (value never on the CLI). `--from-file` used in CI.
3. Apply the example `Agent` (with inline image digest, storage, secret ref, granters).
4. Send/simulate a Slack mention of the cost bot.
5. Router creates an AgentRun.
6. Controller detects the required GCP secret, finds no grant, blocks the run, posts a Slack approval to a configured granter.
7. Granter approves in Slack (signature + granter verified, idempotent).
8. Controller writes a durable grant and wakes the agent pod.
9. bootstrap attests; controller validates pod identity (UID/SA/digest/spec); returns only the approved key.
10. Runner uses the credential file, queries billing, posts a reply output.
11. Router posts the reply to the Slack thread (or fake sink).
12. Pod scales to zero after idle timeout.
13. Audit shows request, approval, grant, materialization, run start/complete.
14. Rebuilding the image with a new digest invalidates the grant → next use re-requires approval.
15. Agent ServiceAccount cannot list/read Kubernetes Secrets.

---

## 20. Failure behavior

- **Controller restart:** SQLite state persists; pending runs/requests remain; router reconnects to Slack.
- **SQLite unavailable/corrupt:** fail closed — do not release secrets; surface readiness failure.
- **Approval never comes:** run stays Blocked until a (configurable) timeout, then `Failed` reason `ApprovalTimeout`.
- **Approval pending UX:** after `coldStartReply`, if the run blocks on approval the router posts an "approval pending" follow-up so the user isn't left on "Checking…" during a long (possibly hours) approval.
- **Pod cannot attest:** bootstrap exits non-zero → run `Failed` (pass-through).
- **Grant revoked / image-or-spec changed during a live session pod:** controller deletes the pod; bootstrap wipes tmpfs on exit.
- **Slack signature invalid / approver not a granter:** reject the interaction, audit `workload.approval_rejected`, run stays Blocked.

---

## 21. Observability

Metrics: `claw_agent_runs_total{agent,phase}`, `claw_secret_requests_total{status}`,
`claw_secret_grants_total{agent,secret}`, `claw_secret_materializations_total{agent,secret,status}`,
`claw_router_events_total{type,status}`, `claw_router_deduped_total`, `claw_db_size_bytes`.

Audit events: `secret.created`, `secret.version_added`, `secret.requested`,
`secret.request.approved|denied`, `secret.grant.created|revoked`, `secret.materialized`,
`workload.login`, `workload.login_failed`, `workload.approval_rejected`,
`agentrun.created|started|completed`, `connector.event_received|deduped`.

Audit rows are **hash-chained** (`prev_hash`/`row_hash`, §7) so the log is tamper-evident, not merely insert-only — a compromised controller cannot silently rewrite history.

Logs: structured JSON with run/agent/namespace/request/grant IDs. Never secret values.

---

## 22. Open questions

1. Slack approval timeout default and re-prompt behavior?
2. Image digest resolution for private registries (the controller resolves tag→digest, or require digests in the Agent spec)? v0 assumes digest in spec.
3. GKE Workload Identity as an alternative agent identity mode — later.
4. Minimum viable UI for humans to inspect a requested secret safely (CLI `request show` only for v0)?
5. Direct Pods vs Deployment for session mode — v0 uses a Deployment scaled 0↔1.

---

## 23. Design principles

1. Fail closed for secrets.
2. Secret release is explicit, scoped to agent+digest, approved by a configured granter, auditable.
3. Agent pods are disposable; workspace/memory persist on PVC independently of pods.
4. The image digest is part of the trust boundary — runner code is baked into the image.
5. The `Agent` CRD is the user-facing contract; SQLite is the sensitive runtime source of truth.
6. Boring by default: SQLite, Tink, controller-runtime, Helm. Spend innovation tokens only on the approval/attestation model.
7. The first version is boring and understandable; optional integrations stay deferred.
8. Every major action is visible through status, CLI, logs, or audit.
9. kube-claw targets **any** Kubernetes, not just GKE. Raw-secret delivery exists precisely because identity federation (e.g. Workload Identity) is not universally available — so the secret authority earns its keep even when delivering a GCP key.
10. Agent identity is first-class and pluggable: the runner authenticates with a scoped, claw-issued session token, never the raw platform credential, and the credential verifier can be swapped for an external identity source.

---

## 24. Self-update plane (`claw-supervisor`)

_(Designed 2026-07-16, post-MVP. Nothing here blocks Phase 7. Implementation is
tracked as Phases 8a–8e below / TODOS T-8.)_

> **Status (2026-07-16): Phases 8a–8e implemented** — `cmd/claw-supervisor` +
> `internal/supervisor` (reconciler, poller, watchdog, notifier),
> `internal/upgrade` (controller-side coordinator), the ControlPlane CRD, chart
> 0.4.0, the CI manifest-publishing job, and unit + envtest coverage. Not yet
> live-tested on a cluster (kind/k3d e2e of the rollback path remains); manifest
> signing is T-9.

### 24.1 Problem & shape

kube-claw should detect new releases, ask a human in Slack for permission,
apply the update itself, and roll back if the new version is unhealthy. A
controller that patches its own StatefulSet cannot roll itself back — **whatever
performs the update must survive it.**

So: the supervisor pattern (the k3s `system-upgrade-controller` / OLM shape).
Helm installs a tiny, boring, always-running **`claw-supervisor`** that owns the
controller's lifecycle; the controller keeps every interesting feature.

> **Naming:** not "claw-base" — *base* already means agent **base image**
> (`claw baseimage`, the registry, the UI tab). The supervisor is a different
> concept and gets a non-colliding name.

```text
helm chart (rarely changes)             claw-supervisor (Deployment, tiny)
  CRDs (agents + controlplanes)           reconciles the ControlPlane CR:
  claw-supervisor Deployment       ──▶      · renders/patches the controller StatefulSet
  static SA/RBAC (both planes)              · polls the release manifest
  Secrets wiring                            · health-watches rollouts, rolls back
  ControlPlane CR (policy)                  · bare chat.postMessage on failure
                                                     │ owns
                                                     ▼
                                          claw-controller StatefulSet
                                          (Slack, router, secret authority — all of today)
```

Division of labor — the supervisor's value is being **too boring to break**:

- **supervisor:** reconcile + watchdog only. No socket mode, no LLM, no store,
  no Tink key, no secret-authority powers. Its only Slack capability is one
  plain `chat.postMessage` HTTPS call (bot token read from the existing Secret)
  to report a failed/rolled-back update when the controller is down.
- **controller:** watches the ControlPlane CR it runs under; conducts the Slack
  conversation (upgrade prompt + approve/skip buttons — the same §8.1
  interaction machinery as secret grants); writes approvals back to the CR;
  posts "upgraded ✅" after it boots into the new version.

### 24.2 The `ControlPlane` CRD

A second CRD kind, singleton named `claw` in the release namespace, created by
the chart. `Agent` (§6) remains the only **user-facing** resource; ControlPlane
is operator infrastructure — humans normally touch it only through Helm values.

```yaml
apiVersion: claw.run/v1alpha1
kind: ControlPlane
metadata:
  name: claw
  annotations:
    # Written by the CONTROLLER on Slack approval. Never templated by Helm,
    # so `helm upgrade`'s three-way merge preserves them.
    claw.run/approved-version: v0.4.0
    claw.run/approved-controller-digest: sha256:…
    claw.run/approved-runner-digest: sha256:…
spec:                        # Helm-owned POLICY (rendered from values.yaml)
  updates:
    mode: prompt             # prompt | auto | manual
    channel: stable
    manifestURL: ""          # optional override; default derived from channel
    checkInterval: 6h
  version: v0.4.0            # helm-pinned floor (what install/upgrade deploys)
  image:
    controllerRepository: docker.io/bitwavecode/kube-claw-controller
    runnerRepository: docker.io/bitwavecode/kube-claw-runner
  controller: {…}             # passthrough of today's controller.* values (args/env/resources)
status:                      # STATE (supervisor + controller, via /status)
  runningVersion: v0.3.1     # written by the CONTROLLER after boot-complete → the
  runningControllerDigest: … #   startup-confirmed signal the watchdog waits on
  availableVersion: v0.4.0
  lastCheckTime: "…"
  previousControllerDigest: …  # rollback target, recorded before each apply
  previousRunnerDigest: …
  phase: Idle | Updating | RollingBack | Degraded
  lastRollback: {from: …, to: …, reason: …, at: …}
  conditions: […]
```

**Policy/state split is the load-bearing decision:** `spec` is Helm's (a
`helm upgrade` may rewrite it wholesale); approvals live in annotations and
runtime truth in `status`, which Helm never touches — so upgrades and the
updater never fight.

**Desired-version resolution (supervisor):**

```text
mode == manual:        desired = spec.version
mode == prompt|auto:   desired = semver-max(spec.version, approved-version annotation)
```

Helm can always move forward past an approval; a stale spec can't revert one
(max). An explicit **downgrade** is: manual mode + helm (or clear the
annotations) — never automatic.

**Tag vs digest (deliberate asymmetry):** helm-pinned versions deploy by tag
(`repository:vX.Y.Z`, exactly like today); self-update deploys by the
**digest** from the manifest, pinned into the approval annotation — what the
admin approved is byte-for-byte what runs, no tag-swap TOCTOU.

### 24.3 The release manifest

Published by the release pipeline as a GitHub Release asset plus a stable
per-channel URL. One HTTPS GET — the supervisor never needs a registry API.

```json
{
  "schemaVersion": 1,
  "channel": "stable",
  "version": "v0.4.0",
  "releasedAt": "2026-07-16T00:00:00Z",
  "images": {
    "controller": "docker.io/bitwavecode/kube-claw-controller@sha256:…",
    "runner":     "docker.io/bitwavecode/kube-claw-runner@sha256:…"
  },
  "minSupervisorVersion": "v0.1.0",
  "requiresHelmUpgrade": false,
  "containsMigration": true,
  "notes": "one-paragraph human summary — used verbatim in the Slack prompt"
}
```

Rules:

- `requiresHelmUpgrade: true` **or** `minSupervisorVersion` > running
  supervisor ⇒ the release is **notify-only in every mode**: the Slack message
  says "run `./scripts/install.sh`", and there is no Upgrade button. The mode
  setting cannot override this. (Chart/RBAC/CRD changes, or a new StatefulSet
  shape needing a newer embedded template, are by definition helm-level.)
- `containsMigration: true` ⇒ the watchdog will not auto-roll-back (§24.5) and
  the prompt says so.
- **Custom registries** (`IMAGE_REPO=…` installs): Docker Hub digests are
  meaningless there — the supervisor forces manual behavior and sets a status
  condition, unless the operator points `manifestURL` at their own manifest.
- Manifest **signing** (T-9, implemented): a detached **ed25519** signature
  over the exact manifest bytes at `<manifestURL>.sig`. CI signs with the
  `MANIFEST_SIGNING_KEY` repo secret; installs pin the PEM public key via the
  `updates.manifestPublicKey` value (→ `CLAW_MANIFEST_PUBKEY`). With a key
  configured the supervisor **fails closed** — a missing or invalid signature
  rejects the manifest. The trust anchor deliberately lives in Helm values,
  never on the manifest host. Without a key: unsigned mode (HTTPS + the
  chart-pinned URL, the pre-T-9 posture).

### 24.4 Update modes

`values.yaml → spec.updates.mode`; `install.sh` gains one question
("Self-update mode? [prompt/auto/manual]") next to the existing token prompts.

- **prompt** (default): on a newer manifest version the controller DMs the
  upgrade admin — version, release notes, migration warning if any, and
  **[Upgrade] [Skip this version] [Remind me later]**. Approve → controller
  writes the approval annotations → supervisor applies. Skip is recorded in the
  store; that version never re-prompts.
- **auto**: the supervisor applies new releases unprompted (still
  digest-pinned, still health-watched); the controller posts
  "upgraded ✅ v0.3.1 → v0.4.0" after boot.
- **manual**: only `spec.version` moves the install — helm is the sole
  actuator. New releases are still *announced* ("v0.4.0 available; this install
  is helm-managed") — detection is cheap and silence isn't a feature.

The mode decides **who may move the desired version — nothing else**. The
reconcile / watchdog / rollback path is identical in all three, which is also
why manual mode still runs through the supervisor: a `helm upgrade` that ships
a bad image gets the same rollback watchdog as a bad self-update, and the chart
keeps a single architecture instead of two divergent render paths.

### 24.5 Health watchdog & rollback

Apply procedure (supervisor):

1. Record the currently-running digests into `status.previous*Digest`.
2. Patch the StatefulSet pod template — controller image **and**
   `--runner-image` in the same patch, so the pair moves in lockstep.
3. Wait for **startup-confirmed**, not merely pod-Ready: the new controller
   writes `status.runningVersion = vNEW` only after migrations ran, the store
   opened, and (when enabled) Slack connected. Probe-ready alone is not success.
4. Deadline (default 10m, spec-tunable): no confirmation ⇒ **rollback** — patch
   back to `previous*Digest` (recorded, not re-resolved), set
   `status.lastRollback`, hold `phase: Degraded` until the old version
   confirms, and notify via the supervisor's bare `chat.postMessage`. A
   crash-looping new image is caught by the same deadline.

**Migration rule:** if the applied release had `containsMigration: true`,
auto-rollback is disabled — old code on a new schema is worse than staying
down. The supervisor holds `Degraded` and pages the admin instead. Mitigations
on the controller side: on boot it snapshots the SQLite file to
`claw.db.pre-<newVersion>` **before** migrating (PVC-local; doubles as the
manual restore point), and the standing policy is **additive-only migrations
within a channel**. Snapshot retention folds into T-2 (GC).

### 24.6 The upgrade admin

Prompt mode needs a durable "who do we ask". New `settings` KV on the `Store`
(§7), key `upgrade_admin_slack_user`.

- **Capture at onboarding:** the existing channel-onboarding DM (§12) gains one
  extra question **only while no admin is set**: "Should you be the upgrade
  admin for this install? [Yes/No]". First-claim-wins, only while unset.
- **Override:** `claw settings set upgrade-admin U0123` (admin API, basic-auth)
  and an admin-UI field.
- **Never claimable via a bare DM command** — any workspace member can DM the
  bot.
- **Unset admin + prompt mode:** detection still runs; surfaced as a status
  condition + admin-UI banner, and the CLI break-glass `claw upgrade approve
  <version>` works regardless (mirrors `claw secret approve`, §8.1).

### 24.7 Chart, values, RBAC

```text
charts/
  crds/claw.run_agents.yaml
  crds/claw.run_controlplanes.yaml          # NEW (kubectl-applied, like agents)
  claw/templates/
    supervisor-deployment.yaml              # NEW — replaces controller-statefulset.yaml
    controlplane.yaml                       # NEW — policy CR rendered from values
    rbac.yaml                               # both SAs, still fully static
    service.yaml / networkpolicy.yaml / ingress.yaml   # unchanged (labels preserved)
```

- The controller StatefulSet template moves into the supervisor binary
  (embedded, versioned with it) and is parameterized by `spec.controller`
  passthrough. Selectors/labels stay identical so Service/NetworkPolicy/Ingress
  keep working untouched.
- Values: `updates.{mode,channel,manifestURL}` (new), `controller.version`
  (replaces `image.tag`); `image.repository`/`RUNNER_IMAGE` overrides kept for
  custom registries. **The default becomes a pinned release version — `latest`
  dies here**; self-update is meaningless against a mutable tag.
- **RBAC stays static in the chart, and the supervisor is deliberately NOT a
  superset of the controller.** Kubernetes only lets a principal grant
  permissions it already holds (else `escalate`/`bind`), so a supervisor that
  managed the controller's SA/RBAC would need all of the controller's powers —
  defeating the tiny-trusted-thing goal. A release that needs new RBAC sets
  `requiresHelmUpgrade`.
  - supervisor SA: `controlplanes`(+status) get/list/watch/update/patch;
    `apps/statefulsets` (release ns) get/list/watch/create/update/patch; named
    `secrets` get (bot token); `events` create.
  - controller SA: today's ClusterRole + `controlplanes`(+status)
    get/list/watch/update/patch (approval annotations, `runningVersion`).

**Trust note (honest version):** a Slack button — and in auto mode, the
manifest publisher — can now change the control plane's running code.
Mitigations: digest-pinned approvals; the admin is a stored setting, not
DM-claimable; helm-level releases degrade to notify-only; the manifest URL is
chart-pinned (an attacker needs values access to move it). In **auto** mode a
compromised manifest endpoint is in-cluster code execution — that residual risk
is why T-9 (manifest signing) is the designated follow-up, and why `prompt` is
the default mode. Approval annotations are writable by anyone with CR-update
rights — i.e. cluster admins, who already own the cluster; acceptable.

### 24.8 Migrating existing installs (chart ≤0.3.x → 0.4.0)

`helm upgrade` to 0.4.0 removes the StatefulSet from Helm's ownership, so Helm
deletes it. That is survivable **by design**: the data PVC
(`data-claw-controller-0`) was created by the StatefulSet controller from the
volumeClaimTemplate — not by Helm — and outlives the deletion. The supervisor
immediately recreates a StatefulSet with the same name/serviceName, the PVC
reattaches, SQLite is intact. Cost: one brief control-plane outage, already an
accepted property of the single-replica design (§5). `install.sh` gains the
mode prompt; `deploy-secrets.sh` is untouched.

### 24.9 Implementation plan

```text
Phase 8a  Version identity + settings: ldflags stamping (-X main.version=…) into
          controller/supervisor/runner, Store settings KV + migration, upgrade-admin
          capture (onboarding question while unset, `claw settings set upgrade-admin`,
          admin-UI field).
          AC: /version + startup log report the stamped version; admin setting survives
          restart; onboarding offers the claim exactly while unset, never after.

Phase 8b  ControlPlane CRD + supervisor skeleton: CRD gen, cmd/claw-supervisor
          reconciling the controller StatefulSet from the CR (embedded template),
          chart 0.4.0 rework (supervisor deployment, controlplane.yaml, static RBAC),
          ≤0.3.x adoption path.
          AC: fresh install via supervisor is functionally identical to 0.3.x; helm
          upgrade from 0.3.x preserves the PVC/data; envtest: CR change rolls the STS.

Phase 8c  Manifest + detection + notify: manifest schema + release-pipeline publishing,
          poller, status.availableVersion, announce in all modes, notify-only
          degradation (requiresHelmUpgrade / minSupervisorVersion / custom registry),
          skip-this-version persistence.
          AC: stale install → admin DM within checkInterval; requiresHelmUpgrade release
          shows no Upgrade button; skipped version never re-prompts.

Phase 8d  Approval + apply: prompt-mode buttons (reuse §8.1 interaction machinery),
          approval annotations, desired-version resolution (semver-max), auto mode,
          digest-pinned STS patch incl. --runner-image, post-boot "upgraded ✅",
          `claw upgrade approve` break-glass.
          AC: button press → manifest's exact digests running; manual mode never
          self-applies; helm bump past an approval wins (max rule).

Phase 8e  Watchdog + rollback: startup-confirmed signal (controller writes
          status.runningVersion), deadline rollback to previous*Digest, phase +
          lastRollback, supervisor bare chat.postMessage on failure, pre-migration
          SQLite snapshot, containsMigration ⇒ no-auto-rollback (hold Degraded + page).
          AC: kind e2e — bad image auto-rolls-back + Slack failure message lands;
          broken migration release holds Degraded and pages instead of rolling back.
```

Deferred: channels beyond `stable`, soak delay for auto mode, supervisor
announcing updates to *itself*. (T-9 manifest signing: implemented — ed25519
detached signatures, fail-closed when `updates.manifestPublicKey` is set.)

### 24.10 Open questions (carry into the build)

1. Slack upgrade-prompt timeout / re-prompt cadence — share the §22.1 answer
   for secret approvals?
2. Should `auto` mode apply immediately or after a soak delay (N hours after
   publish)?
3. Pre-migration snapshot retention count (ties into T-2 audit/GC).

---

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 0 | — | — |
| Codex Review | `/codex review` | Independent 2nd opinion | 0 | — | — |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 1 | clean | scope reduced (10 CRDs → 1); 7 arch decisions; 4 code-quality folds; 10 outside-voice findings absorbed; 0 critical gaps |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — | — |
| DX Review | `/plan-devex-review` | Developer experience gaps | 0 | — | — |

- **CODEX:** Codex CLI not authenticated — the outside voice ran as an independent Claude subagent (fresh context). 10 findings: #1 (attestation token→pod binding) folded into the new pluggable `/login` identity model; #2 (revocation is best-effort), #3 (separate intake listener + unauthenticated submitter), #4 (Slack transport — resolved to Socket Mode, signature check corrected), #5 (Slack trust), #6 (secret-version invalidation), #9 (no-timers wording, approval-pending UX, digest CEL rule), #10 (hash-chained audit) all folded. #7 (first use case) rebutted by the user (kube-claw targets any Kubernetes; Workload Identity is not universal). #8 (single binary / oneShotJob simplifications) stand as deliberate user decisions.
- **CROSS-MODEL:** one-model section review + independent Claude subagent agreed on hardening the secret loop; diverged on the first use case (#7 — user holds context the model lacked) and on two simplifications (#8 — user's explicit calls). No auto-applied recommendations; every change above was user-approved.
- **VERDICT:** ENG CLEARED — ready to implement. CEO review optional (revisit only if you want to reopen the #7 first-use-case question).

NO UNRESOLVED DECISIONS
