# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CMDS=iscsiplugin
all: build

include release-tools/build.make

GOPATH ?= $(shell go env GOPATH)
GOBIN ?= $(GOPATH)/bin
export GOPATH GOBIN

REGISTRY ?= test
IMAGENAME ?= iscsi-csi
# Output type of docker buildx build
OUTPUT_TYPE ?= docker
ARCH ?= amd64
IMAGE_TAG = $(REGISTRY)/$(IMAGENAME):$(IMAGE_VERSION)
EXT_LDFLAGS = -s -w -extldflags "-static"

.EXPORT_ALL_VARIABLES:

all: iscsi

.PHONY: iscsi
iscsi:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -a -ldflags "${EXT_LDFLAGS}" -mod vendor -o bin/${ARCH}/iscsiplugin ./cmd/iscsiplugin

.PHONY: test-container
test-container:
	make
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="linux/$(ARCH)" \
		-t $(IMAGE_TAG) --build-arg ARCH=$(ARCH) .

.PHONY: sanity-test
sanity-test:
	make
	./test/sanity/run-test.sh
.PHONY: mod-check
mod-check:
	go mod verify && [ "$(shell sha512sum go.mod)" = "`sha512sum go.mod`" ] || ( echo "ERROR: go.mod was modified by 'go mod verify'" && false )

.PHONY: clean
clean:
	go clean -mod=vendor -r -x
	rm -f bin/iscsiplugin

# Standalone example fixture commands require an explicitly selected namespace.
ISCSI_SERVER_NAMESPACE ?=
.PHONY: install-iscsi-server
install-iscsi-server:
	@test -n "$(ISCSI_SERVER_NAMESPACE)" || (echo 'Set ISCSI_SERVER_NAMESPACE to a dedicated existing namespace' >&2; exit 2)
	kubectl create -f deploy/example/iscsi-server.yaml -n "$(ISCSI_SERVER_NAMESPACE)"
	kubectl rollout status deployment/iscsi-server -n "$(ISCSI_SERVER_NAMESPACE)" --timeout=120s

.PHONY: uninstall-iscsi-server
uninstall-iscsi-server:
	@test -n "$(ISCSI_SERVER_NAMESPACE)" || (echo 'Set ISCSI_SERVER_NAMESPACE to a dedicated existing namespace' >&2; exit 2)
	kubectl delete deployment,service -n "$(ISCSI_SERVER_NAMESPACE)" -l iscsi.csi.k8s.io/example=true --ignore-not-found

.PHONY: e2e-build e2e-test
GINKGO_FLAGS ?= -ginkgo.v
e2e-build:
	go test -mod=vendor -tags=e2e -c -o bin/iscsi-e2e.test ./test/e2e

e2e-test: e2e-build
	./bin/iscsi-e2e.test -test.v $(GINKGO_FLAGS)

ACTIONLINT ?= actionlint
E2E_RECORD_TIMEOUT ?= 1800
.PHONY: e2e-unit e2e-lint e2e-fixture-test e2e-record
e2e-unit:
	go test -mod=vendor -count=1 -v ./test/e2e/...

e2e-lint:
	@test -z "$$(gofmt -l test/e2e)" || (echo 'Run gofmt on test/e2e Go files' >&2; exit 1)
	go vet -mod=vendor -tags=e2e ./test/e2e/...
	shellcheck -x hack/verify-boilerplate.sh
	bash hack/verify-boilerplate.sh
	yamllint --strict -d '{extends: default, rules: {line-length: disable, truthy: disable}}' deploy/example/iscsi-server.yaml .github/workflows/iscsi-e2e.yaml
	$(ACTIONLINT) .github/workflows/iscsi-e2e.yaml

e2e-fixture-test:
	go build -mod=vendor -o bin/e2e-fixture-test ./test/e2e/cmd/fixture-integration
	./bin/e2e-fixture-test

# E2E_ARTIFACT_DIR must be a new directory; never overwrite another run's results.
e2e-record: e2e-build
	@test -n "$(E2E_ARTIFACT_DIR)" || (echo 'Set E2E_ARTIFACT_DIR to a new results directory' >&2; exit 2)
	go build -mod=vendor -o bin/e2e-record ./test/e2e/cmd/record-run
	./bin/e2e-record --artifacts "$(E2E_ARTIFACT_DIR)" --timeout "$(E2E_RECORD_TIMEOUT)"
