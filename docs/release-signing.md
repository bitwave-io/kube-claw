# Release manifest signing

Every kube-claw release publishes `manifest-stable.json` (the digest-pinned
image refs the self-update supervisor consumes) with a detached ed25519
signature (`manifest-stable.json.sig`) over the exact manifest bytes.

Installs following the official channel verify this automatically: the chart's
default `updates.manifestPublicKey` is the official release key below, and with
a key configured the supervisor is **fail-closed** — an unsigned or tampered
manifest is rejected. The trust anchor deliberately lives in Helm values (your
cluster), never on the manifest host, so the release channel can't grant
itself trust.

## Official release public key

Canonical copies of this key: this file, the chart's default
`updates.manifestPublicKey` in `charts/claw/values.yaml`, and
<https://kube-claw.com/release-signing-key.pem>. They must always agree —
cross-check at least two sources before trusting a new key.

```
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAUryD7JLZcaR1yNfyJBF+6I4GstM56T9JB57/nXl6ZvU=
-----END PUBLIC KEY-----
```

Key established 2026-07-18; releases before v0.4.4 were published unsigned.

## Verifying a manifest by hand

```sh
curl -LO https://github.com/bitwave-io/kube-claw/releases/latest/download/manifest-stable.json
curl -LO https://github.com/bitwave-io/kube-claw/releases/latest/download/manifest-stable.json.sig
openssl pkeyutl -verify -rawin -pubin -inkey release-signing-key.pem \
  -in manifest-stable.json -sigfile manifest-stable.json.sig
```

## Key rotation

`updates.manifestPublicKey` accepts multiple concatenated PEM blocks — a
rotation ring; verification passes on any member. A rotation ships a chart
release whose default carries `[retired key, new key]`, later releases drop the
retired key. Installs pick the ring up on `helm upgrade`; self-updates never
change the trust anchor.

## Running your own release channel

If you point `updates.manifestURL` at your own manifest (custom registry,
air-gapped), the official key can't speak for it: generate your own keypair
(`openssl genpkey -algorithm ed25519`), sign your manifests with
`hack/manifest-sign`, and replace `updates.manifestPublicKey` with your public
key — or set it `""` to run unsigned.
