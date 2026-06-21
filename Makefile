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

.PHONY: fmt
fmt:
	go fmt ./...

# Render the Helm charts (Phase 0 acceptance check).
.PHONY: helm-template
helm-template: manifests
	helm template claw-crds ./charts/claw-crds
	helm template claw ./charts/claw
