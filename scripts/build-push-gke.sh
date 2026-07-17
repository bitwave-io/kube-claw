#!/usr/bin/env bash
# Build the kube-claw images for linux/amd64 (GKE Autopilot nodes are amd64) and
# push them to Google Artifact Registry.
#
# Usage:
#   PROJECT=my-gcp-project REGION=us-central1 REPO=claw TAG=v1 ./scripts/build-push-gke.sh
#
# Optional:
#   IMAGES="controller runner gcloud aws azure"   # which to build (default: all)
#
# Prereqs: docker buildx, gcloud, and an Artifact Registry Docker repo:
#   gcloud artifacts repositories create "$REPO" --repository-format=docker --location="$REGION"
#   gcloud auth configure-docker "${REGION}-docker.pkg.dev"
set -euo pipefail

: "${PROJECT:?set PROJECT to your GCP project id}"
: "${REGION:?set REGION, e.g. us-central1}"
REPO="${REPO:-claw}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo latest)}"
IMAGES="${IMAGES:-controller runner gcloud aws azure}"

REGISTRY="${REGION}-docker.pkg.dev/${PROJECT}/${REPO}"
PLATFORM="linux/amd64"

echo "Pushing to ${REGISTRY} (tag ${TAG}, platform ${PLATFORM})"

build() { # name dockerfile
  local name="$1" dockerfile="$2" ref="${REGISTRY}/kube-claw-${1}:${TAG}"
  echo "==> kube-claw-${name}  (${dockerfile})"
  docker buildx build --platform "${PLATFORM}" -f "${dockerfile}" -t "${ref}" --push .
  echo "    pushed ${ref}"
}

for img in $IMAGES; do
  case "$img" in
    controller) build controller Dockerfile ;;
    runner)     build runner     Dockerfile.runner ;;
    gcloud)     build gcloud     images/gcloud/Dockerfile ;;
    aws)        build aws        images/aws/Dockerfile ;;
    azure)      build azure      images/azure/Dockerfile ;;
    *) echo "unknown image '$img'" >&2; exit 2 ;;
  esac
done

cat <<EOF

Done. Reference these in Helm values-gke.yaml (or --set):
  image.repository       = ${REGISTRY}/kube-claw-controller
  image.runnerRepository = ${REGISTRY}/kube-claw-runner
  supervisor.repository  = ${REGISTRY}/kube-claw-supervisor
  version                = ${TAG}

The controller auto-registers the gcloud/aws/azure base images on startup,
deriving their refs from the runner repository (same registry + tag). Point an agent at one
with:  spec.baseImageRef: gcloud   (or aws / azure)

If your images don't follow the kube-claw-<name> naming, override the refs with
the controller flag --cloud-base-images="gcloud=REF,aws=REF,azure=REF".
EOF
