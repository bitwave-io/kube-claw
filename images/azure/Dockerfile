# kube-claw "azure" base image: Azure CLI (az) + the claw bootstrap + runner.
# Register as base image "azure" for agents needing Azure tooling. Credentials
# (service principal) arrive via the secret delivery; `az account show` is a read.
# NOTE: mcr.microsoft.com/azure-cli is primarily amd64 — fine for GKE Autopilot
# (amd64), build off a networked host (not cached on an airplane).
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

FROM mcr.microsoft.com/azure-cli:latest
COPY --from=build /out/claw-runner /claw/runner
COPY --from=build /out/claw-bootstrap /claw/bootstrap
RUN useradd -u 65532 -m -d /home/nonroot nonroot
ENV HOME=/home/nonroot AZURE_CONFIG_DIR=/var/run/claw/secrets/.azure
USER 65532:65532
ENTRYPOINT ["/claw/bootstrap", "/claw/runner"]
