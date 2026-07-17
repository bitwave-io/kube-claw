# Sharing documents out of Slack (time-bound links)

kube-claw's agents are used for "designing out loud": a design conversation
happens in a Slack thread, and the artifact of that conversation — a design
doc — needs to leave Slack so it can be handed to another tool (typically a
local coding agent: `claude "implement this: <url>"`).

The `publish_document` agent tool does exactly that: the agent writes a full
Markdown document, publishes it, and posts back a **time-bound share link**
plus the exact expiry time.

## How it works

```
Slack thread                controller (:8443, internal)        public (:8090)
────────────                ────────────────────────────        ──────────────
"write that up as a doc"
  └─ agent calls
     publish_document ────► POST /v1/runs/{id}/artifacts
                            (run session token auth)
                            stores artifact + token hash
                            returns url + expiresAt ──────────► GET /d/{token}
  agent replies with link                                       serves raw markdown
  + "expires Fri 18 Jul,                                        until expiry, then 410
     2:40 PM UTC — say
     'reshare' for a new link"
```

- **Documents are immutable.** A published doc is stored as an `artifacts` row
  (SQLite, like everything else). A revised doc is a new artifact.
- **Links are separate from documents.** `artifact_tokens` holds only the
  SHA-256 hash of each 256-bit token (the same scheme as secret-intake links),
  with an expiry. Unlike intake tokens, share tokens are **multi-read** —
  you'll open the link yourself and then hand it to an agent.
- **Expired ≠ gone.** The link dies; the document doesn't. Saying "reshare"
  in the original thread makes the agent call `publish_document` again with
  the document's `artifact_id`: a fresh token is minted and **all previous
  links for that document are revoked**. Warm-session history means this
  works even after the pod idled out.
- **The expiry is always stated.** The tool result carries the exact expiry;
  the agent is instructed to repeat the link, the expiry, and the reshare hint
  in its Slack reply.

## Serving: raw markdown only

`GET /d/{token}` returns `text/markdown` with `nosniff`, `noindex`, and
`no-store` headers. No HTML rendering in v1 — that's what makes the link
directly consumable by a URL-fetching agent, and it keeps the public listener
free of any rendered-HTML/XSS surface. An expired or revoked link returns
`410 Gone` with a plain-text pointer back to the Slack thread.

## Security posture

- The **publish** endpoint lives on the internal `:8443` API and requires the
  run session token (`authRunInSession`), like the other runner callbacks —
  it adds nothing to the public surface.
- The **share** endpoint lives on the separate `:8090` listener (with the
  secret-intake form), so an Ingress misconfiguration still can't reach
  `/v1/*`. The Ingress exposes exactly `/ui/secret-intake` and `/d`.
- Reshare is session-bound: a run can only relink artifacts published in its
  own session, and a cross-session (or unknown) `artifact_id` reads as the
  same 404.
- Content is capped at 1 MiB; publish/reshare are audited
  (`artifact.published` / `artifact.reshared`) in the hash-chained log.

## Configuration

Helm (`charts/claw/values.yaml`):

```yaml
controller:
  artifacts:
    ttl: 24h      # default link lifetime
    maxTTL: 168h  # cap on per-publish overrides
```

These map to the controller flags `--artifact-ttl` / `--artifact-max-ttl`.
Links are built on `controller.uiBaseURL`, the same public base the intake
links use.

## Future work

- Admin UI page listing published artifacts with a regenerate/revoke button
  (fallback when the original thread is gone).
- Optional rendered-HTML view for non-technical readers (needs sanitization).
- GCS/object-store backend if artifacts outgrow SQLite TEXT rows.
