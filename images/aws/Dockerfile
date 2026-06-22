# kube-claw "aws" base image: AWS CLI v2 + the claw bootstrap + runner. Register
# as base image "aws" for agents needing AWS tooling. amazon/aws-cli is multi-arch
# (amd64 + arm64). Credentials arrive via the secret delivery (env AWS_* or a
# shared credentials file); sts/ce reads are read-only.
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

FROM amazon/aws-cli:latest
COPY --from=build /out/claw-runner /claw/runner
COPY --from=build /out/claw-bootstrap /claw/bootstrap
RUN useradd -u 65532 -m -d /home/nonroot nonroot
ENV HOME=/home/nonroot AWS_CONFIG_FILE=/var/run/claw/secrets/.aws/config
USER 65532:65532
# amazon/aws-cli sets ENTRYPOINT=aws; override to the claw bootstrap+runner.
ENTRYPOINT ["/claw/bootstrap", "/claw/runner"]
