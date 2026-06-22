# kube-claw "gcloud" base image: Google Cloud SDK (gcloud, bq) + the claw
# bootstrap + runner. Generic — any agent needing GCP tooling references this base
# (register it as base image "gcloud"). Non-root (65532) for the pod's
# runAsNonRoot; gcloud config lives on the tmpfs secrets dir (read-only root fs).
FROM golang:1.26 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/claw-runner ./cmd/claw-runner && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/claw-bootstrap ./cmd/claw-bootstrap

FROM google/cloud-sdk:slim
COPY --from=build /out/claw-runner /claw/runner
COPY --from=build /out/claw-bootstrap /claw/bootstrap
RUN useradd -u 65532 -m -d /home/nonroot nonroot
ENV CLOUDSDK_CONFIG=/var/run/claw/secrets/.gcloud HOME=/home/nonroot
USER 65532:65532
ENTRYPOINT ["/claw/bootstrap", "/claw/runner"]
