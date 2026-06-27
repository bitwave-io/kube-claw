# Deploying kube-claw to GKE Autopilot

The production target for the cloud-cost use case: amd64 Autopilot nodes, images
in Artifact Registry, the intake UI behind an HTTPS Ingress. This is the clean
home for the GCP-cost agent (no local-k3d image-import quirks).

Placeholders used throughout: `PROJECT` (GCP project id), `REGION` (e.g.
`us-central1`), `REPO` (Artifact Registry repo name, e.g. `claw`), `TAG` (image
tag, e.g. a git short SHA), `HOST` (the intake UI hostname, e.g. `claw.example.com`).

## 1. Cluster + registry

```bash
gcloud config set project PROJECT

# Autopilot cluster (regional)
gcloud container clusters create-auto claw --region REGION
gcloud container clusters get-credentials claw --region REGION

# Artifact Registry Docker repo + docker auth
gcloud artifacts repositories create REPO --repository-format=docker --location=REGION
gcloud auth configure-docker REGION-docker.pkg.dev
```

## 2. Build + push images (amd64)

```bash
PROJECT=PROJECT REGION=REGION REPO=REPO TAG=TAG ./scripts/build-push-gke.sh
# builds + pushes kube-claw-controller, kube-claw-runner, kube-claw-gcloud (set IMAGES= to add aws/azure)
```

## 3. Namespaces, CRD, RBAC

```bash
kubectl create namespace claw-system
kubectl create namespace claw-agents

# Helm does NOT upgrade CRDs from crds/ — apply with kubectl so upgrades work too.
kubectl apply -f charts/claw-crds/crds/
```

## 4. Secrets (out-of-band — never in Helm values)

```bash
# Anthropic key in BOTH namespaces (run pods + the controller's router):
for ns in claw-agents claw-system; do
  kubectl -n "$ns" create secret generic claw-anthropic-key --from-literal=api-key="$ANTHROPIC_API_KEY"
done

# Slack tokens (Socket Mode app-level + bot), in claw-system:
kubectl -n claw-system create secret generic claw-slack-tokens \
  --from-literal=app-token="$SLACK_APP_TOKEN" --from-literal=bot-token="$SLACK_BOT_TOKEN"
```

(`scripts/deploy-secrets.sh` does the same from a gitignored `.secrets.env`.)

## 5. Install the control plane

Reserve a static IP + managed cert for the Ingress host first (so `HOST` resolves
and TLS provisions), then:

```bash
helm upgrade --install claw ./charts/claw -n claw-system \
  -f charts/claw/values-gke.yaml \
  --set image.repository=REGION-docker.pkg.dev/PROJECT/REPO/kube-claw-controller \
  --set image.tag=TAG \
  --set controller.runnerImage=REGION-docker.pkg.dev/PROJECT/REPO/kube-claw-runner:TAG \
  --set controller.uiBaseURL=https://HOST \
  --set ui.ingress.host=HOST

kubectl -n claw-system rollout status statefulset/claw-controller
```

Point `HOST`'s DNS at the Ingress address (`kubectl -n claw-system get ingress`).
The managed cert can take ~15 min to go Active.

## 6. Register the base image + an agent

```bash
kubectl -n claw-system port-forward svc/claw-controller 8443:8443 &
export CLAW_CONTROLLER_URL=http://localhost:8443

claw baseimage create gcloud \
  --image REGION-docker.pkg.dev/PROJECT/REPO/kube-claw-gcloud:TAG \
  --description "Google Cloud SDK (gcloud, bq) — GCP cost/billing queries"

claw agent create gcp-cost --base gcloud \
  --system-prompt "You are a read-only GCP cost assistant. Use gcloud/bq to answer billing/spend questions; request a read-only billing key if you don't have one."
```

(Or create agents in the admin UI at `/ui/agents`.)

## 7. Slack app + first run

Configure the Slack app exactly as in the README's "Slack app setup" table
(Socket Mode, `app_mention` + `message.channels`, `chat:write`/`im:write`,
`reactions:write`, `member_joined_channel`, App Home Messages Tab on). Add the
bot to a channel → it DMs you to pick its behavior → `@mention` it.

## Production hardening checklist

These ship after this MVP — do them before exposing beyond a trusted team:

- [ ] **API + UI auth.** `/v1/*` and `/ui/*` (the admin dashboard) are currently
      unauthenticated behind the in-cluster service. Put the dashboard behind IAP
      or an auth proxy; restrict the API service to in-cluster callers.
- [ ] **TLS to the controller** (not just the Ingress) and an internal-only API.
- [ ] **KMS master key.** Swap the local dev Tink key for a Cloud KMS KEK
      (the cipher is KMS-swappable) so secret encryption keys aren't on the PVC.
- [ ] **Store.** SQLite-on-PVC is single-writer; move to Postgres/Spanner (the
      `Store` interface is pluggable) for HA.
- [ ] **Workload Identity** for the agent pods if they should use GCP IAM
      directly instead of a service-account key via `request_secret`.
- [ ] **Egress policy.** The deny-ingress NetworkPolicy is in place; add an egress
      policy allowing only `api.anthropic.com` + the cloud APIs the agents need.
- [ ] **Backups** of the controller PVC (or the external store).
