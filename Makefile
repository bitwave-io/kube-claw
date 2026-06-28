# kube-claw — Phase 0 build skeleton
CONTROLLER_GEN ?= go tool controller-gen

.PHONY: all
all: generate manifests vet test build

# Generate DeepCopy methods for the API types.
.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./api/..."

# Generate the Agent CRD into the claw-crds chart.
.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=charts/claw-crds/crds

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: generate
	go test ./...

# Integration tests against a real apiserver (kube-apiserver + etcd via envtest).
# envtest tests skip themselves when KUBEBUILDER_ASSETS is unset.
.PHONY: test-envtest
test-envtest: generate manifests
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path)" \
		go test ./... -count=1

.PHONY: build
build: generate
	go build ./...

# Build the controller + runner images locally (tag :dev) for k3d/local dev.
# For pushing to a registry, use docker buildx --push (see README "Advanced:
# building a custom image") or scripts/build-push-gke.sh.
IMAGE_TAG ?= dev
.PHONY: images
images:
	docker build -f Dockerfile        -t kube-claw-controller:$(IMAGE_TAG) .
	docker build -f Dockerfile.runner -t kube-claw-runner:$(IMAGE_TAG) .

# Cloud base images (gcloud/aws/azure). Large; not needed for most local dev, so
# kept out of `images`. The controller auto-registers these by name on startup.
.PHONY: cloud-images
cloud-images:
	docker build -f images/gcloud/Dockerfile -t kube-claw-gcloud:$(IMAGE_TAG) .
	docker build -f images/aws/Dockerfile    -t kube-claw-aws:$(IMAGE_TAG) .
	docker build -f images/azure/Dockerfile  -t kube-claw-azure:$(IMAGE_TAG) .

.PHONY: fmt
fmt:
	go fmt ./...

# Render the Helm charts (Phase 0 acceptance check).
.PHONY: helm-template
helm-template: manifests
	helm template claw-crds ./charts/claw-crds
	helm template claw ./charts/claw
