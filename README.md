# 🦞 kube-claw

**A Kubernetes operator that runs sandboxed, Slack-triggered AI agents — with a real secret-authority and human-approved, audited credential access.**

Mention the bot in a channel and it spins up a one-shot/warm pod running a Claude
tool-use loop, answers in-thread, stays warm for follow-ups, then scales to zero.
Agents only get a secret after a PAM-style approval (or self-service provisioning)
in Slack — credentials are encrypted at rest, bound to the agent + image, and the
value never enters the model's context or the logs.

> Inspired by [nano-claw](https://github.com/nanocoai/nanoclaw)'s container-per-agent
> idea, rebuilt as a Kubernetes-native control plane: one CRD, a pluggable store,
> a secret authority, and a self-hosted admin UI.

---

## Quickstart (any Kubernetes cluster)

**Prerequisites:** a Kubernetes cluster you're already `kubectl`-authenticated to
(the current context is used), a container registry the cluster can pull from,
`kubectl`, `helm`, and a Slack app + Anthropic API key. Nodes are assumed amd64.

> Standing up a fresh **GKE Autopilot** cluster end-to-end (cluster, registry,
> images, HTTPS Ingress, secrets)? See **[`docs/deploy-gke.md`](docs/deploy-gke.md)**.

```bash
# Point at your registry + pick a tag.
export REGISTRY=ghcr.io/your-org           # or REGION-docker.pkg.dev/PROJECT/REPO
export TAG=$(git rev-parse --short HEAD)

# 1. Build + push the images. (CI does this on every change —
#    .github/workflows/build-push.yml — or build them directly:)
docker buildx build --platform linux/amd64 -f Dockerfile             -t $REGISTRY/claw-controller:$TAG  --push .
docker buildx build --platform linux/amd64 -f Dockerfile.runner-bash -t $REGISTRY/claw-runner-bash:$TAG --push .
docker buildx build --platform linux/amd64 -f images/gcloud/Dockerfile -t $REGISTRY/claw-gcloud:$TAG     --push .

# 2. CRD + namespaces (Helm does NOT upgrade CRDs from crds/, so apply with kubectl).
kubectl apply -f charts/claw-crds/crds/
kubectl create namespace claw-system
kubectl create namespace claw-agents

# 3. Secrets (out-of-band — never in Helm values).
for ns in claw-agents claw-system; do
  kubectl -n $ns create secret generic claw-anthropic-key --from-literal=api-key="$ANTHROPIC_API_KEY"
done
kubectl -n claw-system create secret generic claw-slack-tokens \
  --from-literal=app-token="$SLACK_APP_TOKEN" --from-literal=bot-token="$SLACK_BOT_TOKEN"
kubectl -n claw-system create secret generic claw-admin --from-literal=password="$ADMIN_PASSWORD"  # admin UI

# 4. Install the control plane.
helm upgrade --install claw ./charts/claw -n claw-system \
  --set image.repository=$REGISTRY/claw-controller --set image.tag=$TAG \
  --set controller.runnerImage=$REGISTRY/claw-runner-bash:$TAG \
  --set slack.enabled=true
kubectl -n claw-system rollout status statefulset/claw-controller

# 5. Register a base image + an agent (via a port-forward to the controller API).
kubectl -n claw-system port-forward svc/claw-controller 8443:8443 &
export CLAW_CONTROLLER_URL=http://localhost:8443
claw baseimage create default --image $REGISTRY/claw-runner-bash:$TAG \
  --description "generic shell base (bash, curl)"
claw agent create assistant --base default \
  --system-prompt "You are a helpful assistant working in an isolated sandboxed container."
```

**Admin UI:** `kubectl -n claw-system port-forward svc/claw-controller 8443:8443`, then
http://localhost:8443/ui (basic auth: `admin` / your `claw-admin` password). For an
HTTPS Ingress so secret-intake links work without a port-forward, see
[`docs/deploy-gke.md`](docs/deploy-gke.md).

### Slack app setup

Enable **Socket Mode** and create both tokens (app-level `xapp-` + bot `xoxb-`).

| Capability | Event subscription | Bot scope |
|---|---|---|
| `@mention` triggers a run | `app_mention` | — |
| Thread replies / channel messages (multi-turn) | `message.channels` (+`message.groups`) | — |
| Bot DMs you (secret links, replies) | — | `chat:write`, `im:write` |
| 👀 / 💤 reactions | — | `reactions:write` |
| Channel onboarding | `member_joined_channel` | `channels:read` |
| DM the bot `register secret …` | `message.im` | `im:history` |

Also enable the **Messages Tab** (App Home) so the bot's DM channel is usable.

### First run

Add the bot to a channel — it DMs you to pick how it behaves. Then `@`-mention it:

```
@your-bot what can you help with?
```

→ 👀 reaction → the router picks the **best-fit agent** (each carries its own image
+ prompt) → it answers in-thread and stays warm for follow-ups. For a task that
needs a credential (e.g. a cloud agent asked about spend), the agent calls
`request_secret` → you provide/approve it via a one-time link → it installs the
credential and continues. Add specialized agents (a cloud SDK base, etc.) and the
router dispatches to them automatically.

---

## Why it's interesting

- **One CRD.** The entire model is a single `Agent` resource (`claw.run/v1alpha1`).
  No mesh of CRDs for secrets, runs, or storage.
- **Secrets as a first-class authority, not env-var sprawl.** Values are encrypted
  with Google Tink (AES-256-GCM envelope; KMS-swappable), released to a workload
  only after a **durable grant** bound to the image digest + agent spec, with a
  hash-chained audit log. The agent gets a file path, never the value in-context.
- **Credential collection on demand.** When an agent discovers it needs a key it
  doesn't have, it calls `request_secret` → the user gets a **one-time intake link**
  in Slack → pastes the value → it's materialized straight into the pod's tmpfs.
- **Warm, multi-turn sessions.** A Slack thread maps to a pod that stays warm for a
  configurable idle timeout, keeping conversation history in memory; a follow-up
  that lands on a cold pod **replays the thread from the store**.
- **LLM agent routing.** Each agent pairs a prompt with an image; the controller
  classifies each request and spawns the **best-fit agent** for it — no manual
  per-channel wiring.
- **Self-configuring channels.** Add the bot to a channel and it DMs the inviter to
  ask how it should behave (active vs @-mentions only, in-channel vs threads).
- **Locked-down pods.** Non-root, read-only root filesystem, dropped capabilities,
  tmpfs secrets, projected SA token, deny-ingress NetworkPolicy.
- **Self-hosted admin UI.** Secrets (rotate, never view), conversations for audit,
  agents (create/edit), base images, prompts, channels.

---

## Architecture

```
                          Slack (Socket Mode)
                                  │  @mention / thread reply / DM
                                  ▼
┌──────────────────────────── claw-controller (control plane) ───────────────────────────┐
│  Slack router ──▶ LLM agent router ──▶ creates a Run (SQLite, hash-chained audit)         │
│  Agent reconciler (controller-runtime)   Secret authority (Tink envelope crypto)          │
│  HTTP API + /login attestation + materialize     Admin UI (/ui)                           │
│  Run engine: gate on secret grants → launch a Job per ready run                           │
└───────────────────────────────────────────────────────────────────────────────────────┘
                                  │ launches
                                  ▼
                    ┌──────────── Run pod (per Slack thread) ───────────┐
                    │  claw-bootstrap (PID1): /login → materialize → exec │
                    │  claw-runner: Claude tool-use loop                  │
                    │    tools: bash (gcloud/aws/az/curl…) + request_secret│
                    │  non-root · ro-rootfs · tmpfs secrets · warm/idle    │
                    └─────────────────────────────────────────────────────┘
```

The store sits behind a `Store` interface (SQLite by default, swappable for
Postgres/Spanner). The cipher is KMS-swappable (local dev key by default).

---

## Core concepts

### The `Agent` CRD

```yaml
apiVersion: claw.run/v1alpha1
kind: Agent
metadata:
  name: gcp-cost
  namespace: claw-agents
spec:
  baseImageRef: gcloud          # registered base image (or `image:` for a pinned digest)
  runtime:
    mode: scaleToZeroSession
    idleTimeout: 15m            # how long the pod stays warm for follow-ups (editable in the UI)
  model:
    systemPrompt: "You are a read-only GCP cost assistant."
  secrets:                      # declared secrets are gated + materialized at launch
    - name: gcp-billing-readonly
      delivery: { type: file, path: /var/run/claw/secrets/gcp.json, env: { GOOGLE_APPLICATION_CREDENTIALS: /var/run/claw/secrets/gcp.json } }
```

Agents can be created/edited without YAML via the CLI/API (`claw agent create …`)
or the admin UI (name + image dropdown + prompt), which manages the CRD for you.
The router selects an agent per request by its prompt, so define one agent per
capability (e.g. a `gcp-cost` agent on the gcloud image, a general `helper` agent).

### The agent loop

`claw-runner` runs a Claude tool-use loop (model `claude-opus-4-8`, adaptive
thinking) with two tools:

- **`bash`** — runs shell commands in the container (whatever the base image
  provides: `gcloud`/`bq`, `aws`, `az`, `curl`, `python3`, …).
- **`request_secret`** — requests *and* retrieves a credential on demand (DMs the
  user an intake link, then installs the provided value into the pod and wires up
  the env var / `gcloud auth`).

Without an Anthropic key the runner falls back to a stub (still proves the
materialize → respond path).

### Secret authority

Secrets are **not** Kubernetes Secrets or env vars. They live in the controller's
store, encrypted with Tink. A workload receives a secret only when:

1. A **grant** exists, bound to `agent + secret + image digest + spec hash + delivery hash`.
2. The pod **attests** via `/login` (Kubernetes SA TokenReview → pod UID from the
   bound token's claims, closing co-resident replay) and gets a scoped session token.
3. `materialize` returns the decrypted value to `claw-bootstrap`, written to a
   tmpfs volume — never to the model context or logs.

Grants are durable (no leases); they're invalidated when the image digest or spec
changes, or on explicit revoke. Approvals are PAM-style: configurable granters
approve via Slack buttons, or the requesting user self-services via an intake link.

---

## The `claw` CLI

```bash
export CLAW_CONTROLLER_URL=http://localhost:8443   # via `kubectl port-forward`

claw agent create assistant --base gcloud --system-prompt "…"   # create an agent (no YAML)
claw agent list

claw baseimage create gcloud --image my/gcloud-runner:tag --description "GCP cost/cloud queries"
claw baseimage list

claw secret create gcp-billing-readonly --type gcp.serviceAccountKey \
  --granter U0123 --description "read-only GCP billing key"        # → prints a one-time intake link
claw secret requests                                                # pending approvals
claw secret approve <req-id>
claw secret grants
claw secret grant revoke <grant-id>

claw prompt set assistant "<system prompt>"     # editable prompts (apply next run)
claw prompt get claw-agents assistant

claw runs list
claw runs show <run-id>
```

---

## Admin UI

Served by the controller at **`/ui`** (port `8443`):

- **Secrets** — metadata + **Rotate** (mints a one-time link to set a new value).
  Values are write-only — never viewable.
- **Conversations** — runs grouped into continuous threads for audit (request +
  answer; no secret values).
- **Agents** — **create and edit** agents (name + image dropdown + prompt), with an
  inline **idle-timeout editor**.
- **Images / Prompts / Channels** — the base-image registry, editable prompts, and
  the dynamic channel routing.

Secret-intake links are served separately on port `8090`. The UI is unauthenticated
behind a port-forward today — add auth + TLS before exposing it.

---

## Configuration

**Controller flags** (set via Helm `controller.*` values):
`--data-dir`, `--runner-image`, `--self-url`, `--ui-base-url`, `--anthropic-secret`
(K8s secret injected into run pods + the router), `--default-agent`,
`--enable-router`.

**Slack** (Helm `slack.*`): `enabled`, `tokenSecretName`, optional static `routes`
(channels self-configure via onboarding otherwise).

Two Helm charts: `charts/claw-crds` (the CRD) and `charts/claw` (the control plane).
Note: Helm does not upgrade CRDs from `crds/` — `scripts/install.sh` applies the CRD
with `kubectl` so install **and** upgrade work.

---

## Security model

- Pods run **non-root**, **read-only root fs**, **all capabilities dropped**, with a
  seccomp runtime-default profile and a baseline **deny-ingress NetworkPolicy**.
- Secrets are **encrypted at rest** (Tink AES-256-GCM envelope; KMS-swappable) and
  delivered to a **memory-backed tmpfs**, wiped on pod exit.
- Workloads **attest** via Kubernetes SA TokenReview; the issued claw session token
  is **scoped to the run and its granted secrets**.
- The decrypted value is **never** placed in the model's context, tool output, run
  outputs, or logs — agents see only a file path and a usage description.
- Hash-chained **audit log** of every secret request, grant, approval, and
  materialization.

---

## Development

```bash
make manifests        # regenerate CRDs after editing api/v1alpha1
go build ./...
go test ./...         # unit tests
make test-envtest     # controller integration tests against a real apiserver
```

Code layout:

| Path | What |
|---|---|
| `api/v1alpha1` | the `Agent` CRD |
| `cmd/claw-controller` | control-plane entrypoint |
| `cmd/claw-runner` | the agent loop (Claude tool-use) |
| `cmd/claw-bootstrap` | PID1: `/login` → materialize → exec runner |
| `cmd/claw` | the `claw` CLI |
| `internal/controller` | the Agent reconciler |
| `internal/runengine` | gate-on-grants + launch Jobs |
| `internal/secrets` | the secret authority (Tink) |
| `internal/identity` | `/login` attestation + session tokens |
| `internal/router/slack` | Slack connector, routing, onboarding, reactions |
| `internal/apihttp` | HTTP API + admin UI |
| `internal/store` | the `Store` interface (`sqlite` impl) |
| `internal/workloads` | the run Job builder |
| `charts/` | Helm charts (`claw-crds`, `claw`) |
| `images/` | base-image Dockerfiles (`gcloud`, `aws`, `azure`) |

---

## Status

MVP, actively developed. Working: the agent loop, secret authority + grants,
on-demand `request_secret`, warm multi-turn sessions with history replay, the
Slack connector (routing, onboarding, reactions), the LLM agent router, the
base-image registry, and the admin UI.

**Not yet done / hardening:** API + UI auth and TLS; a non-SQLite store + KMS master
key wired in; production deploy on GKE Autopilot (the target for the real cloud-cost
use case); `aws`/`azure` base images built and published.

---

🤖 Built with [Claude Code](https://claude.com/claude-code)
