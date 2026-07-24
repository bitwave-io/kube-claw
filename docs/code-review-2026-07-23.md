# Codebase correctness and security review

Date: 2026-07-23

Scope: the working tree of `kube-claw` as reviewed on this date. The tree contained
uncommitted work, including active provider/model changes, so findings concerning
those features apply to the reviewed filesystem snapshot.

Secret grants are treated as authorization to the secret resource. Secret rotation
is expected to make the latest value available to agents that already have a grant;
secret-version-specific grants are deliberately not recommended here.

## Findings

### 1. Critical: agent pods can invoke unauthenticated break-glass APIs and steal secrets

Privileged endpoints are registered in `internal/apihttp/server.go:97`, while the
authentication middleware at `internal/apihttp/server.go:243` protects only `/ui`
and explicitly leaves `/v1` unaffected.

The approval handler at `internal/apihttp/server.go:942` invokes the
granter-bypassing approval path without authenticating the caller. That path,
documented at `internal/approvals/service.go:27`, assumes the API or CLI caller has
already been authenticated.

The controller NetworkPolicy at
`charts/claw/templates/networkpolicy.yaml:16` allows every pod in the
`claw-agents` namespace to reach this listener.

An agent can:

1. Use its legitimate run token to list secret names and request a secret.
2. Call unauthenticated `GET /v1/secret-requests` to obtain the request ID.
3. Call unauthenticated `POST /v1/secret-requests/{id}/approve`.
4. Retrieve the plaintext through the legitimate requested-secret endpoint.

The same unauthenticated API surface permits secret-value replacement, agent
creation, run and conversation inspection, prompt modification, schedule creation,
grant revocation, and base-image replacement.

Recommended remediation:

- Establish an explicit route-by-route authorization matrix.
- Put runner callbacks and administrative APIs on separate listeners and
  Kubernetes Services.
- Strongly authenticate every privileged endpoint.
- Default to denial when no admin credential is configured.
- Add negative authorization tests for every route and caller type.
- Narrow the controller NetworkPolicy rather than treating every agent pod as an
  administrative caller.

### 2. High: base-image and default agents have an empty image binding

The reconciler records a digest only for inline images. Agents using
`baseImageRef` or the global runner fallback receive an empty digest at
`internal/controller/agent_controller.go:66`.

Base images are resolved dynamically at run time in
`internal/runengine/engine.go:191` and can be replaced in place through
`internal/store/sqlite/baseimages.go:11`. Grants containing an empty digest still
match in `internal/store/sqlite/grants.go:37`.

Consequently, code that receives a secret can change without invalidating the
grant. This is especially dangerous in combination with the unauthenticated
base-image registration endpoint.

Recommended remediation:

- Resolve every executable image to an immutable digest before approval or launch.
- Reject empty or unresolved image bindings.
- Include the resolved base-image digest in agent status and grant evaluation.
- Require base-image registry entries to use immutable digests.
- Verify the running pod's actual `status.containerStatuses[].imageID` during
  workload login.
- Fail closed when a named base image cannot be resolved instead of silently using
  a global fallback.

### 3. High: revocation does not stop existing declared-secret access

Revocation only updates SQLite in `internal/secrets/approval.go:94`. The
declared-secret materialization path in `internal/apihttp/workload.go:223` trusts
the access token's cached scopes without checking that the grant remains live.
Access tokens remain valid for 30 minutes according to
`internal/apihttp/workload.go:26`, and revocation does not terminate active pods.

Under the intended resource-level grant model, the administration UI should show:

- Each secret's granted agents.
- Every channel currently routed to those agents.
- The approver, approval time, reason, and last materialization.
- Whether the agent currently has a live pod.
- A direct revoke action for each agent.

Grants are agent-wide. If one agent serves several channels, approving the agent
authorizes use from all those channels, and revoking it affects all of them.

Recommended remediation:

- Revalidate the live grant during each materialization.
- Add a revocation epoch or token identifier that can invalidate issued tokens.
- Terminate matching live pods when a grant is revoked.
- Wipe materialized files as the pod exits.
- Expose the channel-to-agent-to-secret access graph in the UI.
- Clearly state that credentials already used outside kube-claw cannot be recalled.

### 4. High: the bash tool can read platform and model credentials

Workloads receive `ANTHROPIC_API_KEY` in their environment at
`internal/workloads/job.go:48`. Bootstrap exports the claw access token and refresh
token at `cmd/claw-bootstrap/main.go:52`. Bash commands inherit the runner's entire
environment at `cmd/claw-runner/agent.go:1415`.

A direct or indirect prompt injection can run `env`, read the projected Kubernetes
service-account token, or use `CLAW_TOKEN` to request the decrypted model-provider
key returned by `internal/apihttp/models.go:210`. Bash output is returned to the
model and may subsequently be placed in output or logs.

This also means that keeping a model API key out of the initial pod environment
does not keep it out of reach of the tool process.

Recommended remediation:

- Keep platform and model credentials outside the tool-execution process.
- Prefer a controller or sidecar model proxy that performs provider calls without
  disclosing provider keys to the agent.
- Give tool subprocesses an explicit environment allowlist.
- Do not expose refresh credentials or a broadly useful session token to bash.
- Consider separating the trusted runner and untrusted tool execution into
  separately isolated containers or workloads.

### 5. High: the admin UI fails open and has no CSRF protection

The admin password Secret is optional at
`internal/supervisor/statefulset.go:63`. An empty password disables authentication
at `internal/apihttp/server.go:246`, while the GKE overlay exposes `/ui` publicly
at `charts/claw/values-gke.yaml:57`.

State-changing forms, including secret deletion, do not contain CSRF tokens or
perform Origin validation. One example is
`internal/apihttp/dashboard.go:133`. Cached HTTP Basic credentials are ambient
browser authority, so cross-origin form submissions are relevant.

Recommended remediation:

- Refuse to expose or start the admin UI without configured authentication.
- Prefer OIDC or a normal authenticated session over long-lived HTTP Basic
  credentials.
- Add CSRF tokens to every state-changing form.
- Validate `Origin`, `Sec-Fetch-Site`, and related Fetch Metadata headers.
- Use secure, HTTP-only, SameSite cookies if sessions are introduced.
- Add CSP, frame-ancestor restrictions, and other standard administrative UI
  response headers.

### 6. High: multi-provider model support has correctness and credential-routing defects

The current provider/model work has several independent problems.

First, the runner selects its stub whenever `ANTHROPIC_API_KEY` is absent at
`cmd/claw-runner/main.go:30`. This happens before the runner resolves an OpenAI or
Gemini model from the controller. An installation configured only with an OpenAI
or Gemini provider therefore never runs its configured model.

Second, Gemini discovery hardcodes Google's inference endpoint at
`internal/models/catalog.go:149`, even when the administrator configured a custom
gateway URL. The stored provider key is subsequently sent as a Bearer credential
by `cmd/claw-runner/openai.go:245`. This can bypass a configured gateway or send a
gateway-specific credential to Google.

Third, catalog synchronization computes a handle from the provider's model ID and
upserts it at `internal/models/service.go:323`. It does not reject an existing
manual model or a model owned by another provider with the same handle. A catalog
sync can therefore overwrite an unrelated model configuration or its endpoint.

Recommended remediation:

- Resolve the controller's model registry before deciding whether to use the stub.
- Treat the legacy Anthropic environment configuration as a fallback only after
  registry resolution fails or returns no model.
- Model catalog URLs and inference URLs as separate fields.
- Never silently replace the configured inference destination.
- Reject handle collisions across providers and manual models, or namespace
  discovered handles by provider.
- Add end-to-end tests for OpenAI-only, Gemini-only, custom-gateway, and
  handle-collision installations.

### 7. High correctness: connector session IDs generate invalid Kubernetes labels

Connector session keys contain a colon at
`internal/connector/connector.go:42`. The key is inserted into a Kubernetes Job
label at `internal/workloads/job.go:72`. Label normalization at
`internal/workloads/job.go:161` replaces dots and truncates the string but leaves
the colon unchanged.

Kubernetes label values do not permit colons. Connector messages with a nonempty
session ID therefore fail Job creation. Truncating long raw IDs also permits
different sessions to collide and be mistaken for the same warm session.

Recommended remediation:

- Derive the label from a collision-resistant hash, such as a fixed-length
  SHA-256/base32 value.
- Keep the raw external session ID only in the database and environment.
- Use the same canonical hash helper for Job creation and Job lookup.
- Add tests using colons, spaces, Unicode, maximum-length values, and pairs with a
  common 63-character prefix.

### 8. High correctness: self-update constructs invalid cloud-image digest references

Release manifests contain only controller and runner digests in
`.github/workflows/build-push.yml:120`. The controller derives the gcloud, AWS, and
Azure image references by replacing the runner repository name while preserving
the runner's digest at `cmd/claw-controller/main.go:483`.

A digest from the `kube-claw-runner` repository does not identify the distinct
`kube-claw-gcloud`, `kube-claw-aws`, or `kube-claw-azure` image. Cloud agents can
therefore become unpullable after a self-update.

Recommended remediation:

- Include an immutable reference for every cloud image in the signed release
  manifest.
- Carry those exact references through the ControlPlane desired state.
- Never transfer a digest from one repository to another.
- Add a self-update E2E case that launches each seeded cloud agent after the
  upgrade.

### 9. Medium: the vulnerability gate is ineffective

The security workflow deliberately makes `govulncheck` non-blocking because the
scanner cannot load the Go 1.26 packages. See
`.github/workflows/security.yml:24`.

Binary-mode scanning of locally built artifacts found:

- 19 reachable advisories in the controller.
- 12 reachable advisories in the runner.
- Standard-library issues fixed through Go 1.26.5.
- Reachable `golang.org/x/net` advisory GO-2026-5026; the reviewed dependency was
  `golang.org/x/net` v0.53.0.

This does not prove every published image was built with the vulnerable Go patch
level because the Docker Go tag floats. It does demonstrate that the declared
build floor can produce affected binaries without a gating scan.

Recommended remediation:

- Require Go 1.26.5 or later.
- Update `golang.org/x/net` and the related `x/*` modules to fixed current
  versions.
- Pin builder images to reviewed digests.
- Run binary-mode vulnerability checks on every produced executable.
- Make the binary scan a release gate until source-mode scanning supports the
  module.
- Scan the completed container images for OS-package vulnerabilities as well.

### 10. Medium correctness: runs can remain permanently Running or Blocked

Jobs are deleted ten minutes after completion at
`internal/workloads/job.go:61`. The run reaper ignores missing Jobs at
`internal/runengine/engine.go:95`.

If the controller is unavailable while a failed Job reaches its TTL, the Job is
deleted and its database run remains `Running` indefinitely. Because the reaper
only examines the oldest 50 running records, a group of old stuck records can also
prevent newer failed runs from being examined.

There is no implemented approval timeout for `Blocked` runs, despite the lifecycle
design describing one.

Recommended remediation:

- Persist terminal outcomes before Kubernetes TTL cleanup can remove the evidence.
- Treat sufficiently old missing Jobs as failed after a bounded grace period.
- Avoid a fixed oldest-first scan that can be starved by permanently stuck rows.
- Add deadlines for Pending, Blocked, and Running states.
- Persist a terminal reason that distinguishes timeout, missing Job, pod failure,
  and callback failure.

### 11. Medium: production encryption uses a cleartext key beside the ciphertext

The only implemented cipher constructor writes an insecure-cleartext Tink keyset
at `internal/secrets/encryption_tink.go:30`. The controller stores that keyset
beside SQLite on the same data volume at `cmd/claw-controller/main.go:127`.

A PVC snapshot, node-level read, or backup compromise yields both the encrypted
database and the key needed to decrypt it. The current encryption protects against
an isolated database read but not against compromise of the storage volume.

Recommended remediation:

- Implement KMS, Vault Transit, or an equivalent envelope-encryption KEK.
- Keep the KEK outside the SQLite PVC and its backups.
- Support key rotation and rewrapping.
- Fail closed in a production mode when only the local cleartext keyset is
  configured.
- Document exactly which storage-compromise scenarios encryption does and does not
  protect against.

### 12. Medium: runtime and public-server hardening gaps

Agent egress restrictions are disabled by default at
`charts/claw/values.yaml:75`. Arbitrary shell code can therefore reach cluster
services, cloud metadata endpoints, and the public internet.

The workload pod receives an explicit audience-scoped service-account token but
does not disable Kubernetes' additional default service-account-token automount at
`internal/workloads/job.go:89`.

The public HTTP server configures only `ReadHeaderTimeout` at
`internal/apihttp/ui.go:38`. It has no full-body read timeout, write timeout, idle
timeout, request-size middleware, or application-level rate limit. Secret intake
calls `ParseForm` without `MaxBytesReader` at `internal/apihttp/ui.go:71`.

The build and release process also uses mutable inputs:

- GitHub Actions are referenced by mutable major-version tags.
- The E2E workflow executes the moving `k3d/main/install.sh` script at
  `.github/workflows/test.yml:63`.
- Several runtime images use mutable `latest` or `slim` base tags.

Recommended remediation:

- Default-deny agent egress or provide explicit, reviewed egress profiles.
- Block cloud metadata and unrelated private ranges by default.
- Set `automountServiceAccountToken: false` while retaining only the explicit
  controller-audience projection.
- Add `ReadTimeout`, `WriteTimeout`, and `IdleTimeout`.
- Apply `http.MaxBytesReader` before parsing or decoding request bodies.
- Rate-limit public token endpoints at both the application and ingress layers.
- Pin GitHub Actions by commit SHA and container bases by digest.
- Download tools from versioned releases and verify checksums rather than piping a
  moving branch into a shell.

## Trust-model observations

These are not necessarily implementation defects, but they should be prominent in
the product's security documentation and administration UI.

### Grants belong to agents, not requesters or channels

A grant authorizes an agent. It is not scoped to the Slack user who requested the
secret, the channel where approval happened, or one conversation.

This is a reasonable resource-level authorization model, but it means the
administrator must be able to see every channel routed to an authorized agent.
Changing routing can also expand where an existing agent grant is usable, even
though the grant record itself did not change.

If channel-specific authorization is ever required, it needs either:

- Separate agent identities per trust domain or channel; or
- An additional routing principal in the authorization model.

### Raw-secret delivery cannot guarantee non-exfiltration

An approved agent receives the raw credential, has a shell, and normally has
outbound network access. "Never enters model context" accurately describes the
normal materialization protocol, but it cannot guarantee that agent code or a
prompt injection will not read and expose the credential.

The product should describe grants as authorizing the agent code and model to use
the credential, not merely authorizing a benign file to exist.

### The audit chain has no external trust anchor

The SQLite audit log is hash-chained, but its current head is not periodically
signed or anchored outside the database. An attacker capable of rewriting the
entire database can rewrite the events and recompute the chain.

For stronger tamper evidence, periodically publish or sign the current chain head
using a key or service outside the data volume.

## Verification performed

The following passed:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- Controller envtest against a real Kubernetes API server
- Supervisor envtest against a real Kubernetes API server
- `helm lint ./charts/claw`
- Default Helm rendering
- Slack-enabled/manual-upgrade Helm rendering
- `git diff --check`
- Repository credential-pattern scan; no private key or live credential was found

`golangci-lint` reported 27 issues:

- 24 unchecked error returns
- 2 static-analysis warnings
- 1 unused function

The full destructive k3d self-update E2E and completed-container OS-package scans
were not run as part of this review.

