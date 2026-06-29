# Example: Slack-triggered GCP cost agent

End-to-end walkthrough of the kube-claw secret loop with the `gcp-cost` agent.
Run it on a local cluster with `scripts/smoke-k3d.sh`, or step through manually below.

## Install

```bash
kubectl apply -f ./charts/crds/
kubectl create namespace claw-system
kubectl create namespace claw-agents
helm upgrade --install claw ./charts/claw -n claw-system \
  --set image.repository=<your-registry>/claw-controller --set image.tag=<tag>
kubectl apply -f examples/gcp-cost-slack/agent.yaml
```

Port-forward the API + intake UI for the CLI / browser:

```bash
kubectl -n claw-system port-forward svc/claw-controller 8443:8443 &   # API
kubectl -n claw-system port-forward svc/claw-controller 8090:8090 &   # intake UI
export CLAW_CONTROLLER_URL=http://localhost:8443
```

## 1. Create the secret + submit its value (one-time link)

```bash
claw secret create gcp-billing-readonly \
  --namespace claw-agents --type gcp.serviceAccountKey --granter U_ALEX
# prints a one-time intake URL — open it and paste the GCP key JSON.
# (CI / break-glass alternative: claw secret put gcp-billing-readonly --from-file key.json)
```

## 2. Trigger a run (no Slack needed)

```bash
claw run create --agent gcp-cost --input "why did GCP cost spike yesterday?"
claw runs show <run-id>     # phase: Blocked (awaiting approval)
```

## 3. Approve (break-glass; Slack PAM approval is the product UX)

```bash
claw secret requests           # list pending
claw secret approve <req-id> --reason "cost bot needs read-only billing"
claw runs show <run-id>        # phase: Succeeded, with the agent's response
```

On approval the controller mints a durable grant (bound to the agent, image
digest, spec hash, and delivery), the run engine launches the agent Job, the
bootstrap attests via `/login` and materializes the decrypted key to tmpfs, and
the runner uses it.

## Slack (optional)

Set `CLAW_SLACK_APP_TOKEN` / `CLAW_SLACK_BOT_TOKEN` on the controller to enable
the Socket Mode connector; approvals then arrive as interactive buttons in Slack
(only configured granters may approve). See `values-slack.yaml`.
