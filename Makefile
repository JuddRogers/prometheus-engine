GOCMDS := $(notdir $(patsubst %/,%,$(dir $(shell find cmd -name 'main.go'))))

CLOUDSDK_CONFIG?=${HOME}/.config/gcloud
PROJECT_ID?=$(shell gcloud config get-value core/project)
GMP_CLUSTER?=gmp-test-cluster
GMP_LOCATION?=us-central1-c
API_DIR=pkg/operator/apis

# For now assume the docker daemon is mounted through a unix socket.
# TODO(pintohutch): will this work if using a remote docker over tcp?
DOCKER_HOST?=unix:///var/run/docker.sock
DOCKER_VOLUME:=$(DOCKER_HOST:unix://%=%)

IMAGE_REGISTRY?=gcr.io/$(PROJECT_ID)/prometheus-engine
TAG_NAME?=$(shell date "+gmp-%Y%d%m_%H%M")

# Support gsed on OSX (installed via brew), falling back to sed. On Linux
# systems gsed won't be installed, so will use sed as expected.
SED ?= $(shell which gsed 2>/dev/null || which sed)

# TODO(pintohutch): this is a bit hacky, but can be useful when testing.
# Ultimately this should be replaced with go templating.
define update_manifests
	find manifests examples cmd/operator/deploy -type f -name "*.yaml" -exec sed -i "s#image: .*/$(1):.*#image: ${IMAGE_REGISTRY}/$(1):${TAG_NAME}#g" {} \;
endef

define docker_build
	DOCKER_BUILDKIT=1 docker build $(1)
endef

define docker_tag_push
	docker tag $(1) $(2)
	docker push $(2)
endef

help:        ## Show this help.
             ##
	@fgrep -h "##" $(MAKEFILE_LIST) | fgrep -v fgrep | sed -e 's/\\$$//' | sed -e 's/##//'

clean:       ## Clean build time resources, primarily docker resources.
             ##
	for i in `docker images | grep -E '^gmp/|.*/prometheus-engine/.*' | awk '{print $$3}' | uniq`; do docker image rm -f $$i; done

lint:        ## Lint code.
             ##
	@echo ">> linting code"
	DOCKER_BUILDKIT=1 docker run --rm -v $(shell pwd):/app -w /app golangci/golangci-lint:v1.43.0 golangci-lint run -v --timeout=5m

$(GOCMDS):   ## Build go binary from cmd/ (e.g. 'operator').
             ## The following env variables configure the build, and are mutually exclusive:
             ## Set NO_DOCKER=1 to build natively without Docker.
             ## Set DOCKER_PUSH=1 to tag image with TAG_NAME and push to IMAGE_REGISTRY.
             ## Set CLOUD_BUILD=1 to build the image on Cloud Build, with multi-arch support.
             ## By default, IMAGE_REGISTRY=gcr.io/PROJECT_ID/prometheus-engine.
             ##
	@echo ">> building binaries"
ifeq ($(NO_DOCKER), 1)
	if [ "$@" = "frontend" ]; then pkg/ui/build.sh; fi
	CGO_ENABLED=0 go build -tags builtinassets -mod=vendor -o ./build/bin/$@ ./cmd/$@/*.go
# If pushing, build and tag native arch image to GCR.
else ifeq ($(DOCKER_PUSH), 1)
	$(call docker_build, --tag gmp/$@ -f ./cmd/$@/Dockerfile .)
	@echo ">> tagging and pushing images"
	$(call docker_tag_push,gmp/$@,${IMAGE_REGISTRY}/$@:${TAG_NAME})
	@echo ">> updating manifests with pushed images"
	$(call update_manifests,$@)
# Run on cloudbuild and tag multi-arch image to GCR.
# TODO(pintohutch): cache source tarball between binary builds?
else ifeq ($(CLOUD_BUILD), 1)
	@echo ">> building GMP images on Cloud Build with tag: $(TAG_NAME)"
	gcloud builds submit --config build.yaml --timeout=30m --substitutions=_IMAGE_REGISTRY=$(IMAGE_REGISTRY),_IMAGE=$@,TAG_NAME=$(TAG_NAME) --async
	$(call update_manifests,$@)
# Just build it locally.
else
	$(call docker_build, --tag gmp/$@ -f ./cmd/$@/Dockerfile .)
endif

bin:         ## Build all go binaries from cmd/.
             ## All env vars from $(GOCMDS) work here as well.
             ##
bin: $(GOCMDS)

regen:       ## Refresh autogenerated files and reformat code.
             ## Use DRY_RUN=1 to only validate without generating changes.
             ##
regen:
ifeq ($(DRY_RUN), 1)
	$(call docker_build, -f ./hack/Dockerfile --target hermetic -t gmp/hermetic \
		--build-arg RUNCMD='./hack/presubmit.sh all diff' .)
else
	$(call docker_build, -f ./hack/Dockerfile --target sync -o . -t gmp/sync \
		--build-arg RUNCMD='./hack/presubmit.sh' .)
	rm -rf vendor && mv vendor.tmp vendor
endif

test:        ## Run all tests. Setting NO_DOCKER=1 writes real data to GCM API under PROJECT_ID environment variable.
             ## Use GMP_CLUSTER, GMP_LOCATION to specify timeseries labels.
             ##
	@echo ">> running tests"
ifeq ($(NO_DOCKER), 1)
	kubectl apply -f manifests/setup.yaml
	kubectl apply -f cmd/operator/deploy/operator/01-priority-class.yaml
	kubectl apply -f cmd/operator/deploy/operator/03-role.yaml
	go test `go list ./... | grep -v operator/e2e | grep -v export/bench`
	go test `go list ./... | grep operator/e2e` -args -project-id=${PROJECT_ID} -cluster=${GMP_CLUSTER} -location=${GMP_LOCATION}
else
	$(call docker_build, -f ./hack/Dockerfile --target sync -o . -t gmp/hermetic \
		--build-arg RUNCMD='GIT_TAG="$(shell git describe --tags --abbrev=0)" ./hack/presubmit.sh test' .)
	rm -rf vendor.tmp
endif

kindtest:    ## Run e2e test suite against fresh kind k8s cluster.
             ##
	@echo ">> building image"
	$(call docker_build, -f hack/Dockerfile --target kindtest -t gmp/kindtest .)
	@echo ">> running container"
# We lose some isolation by sharing the host network with the kind containers.
# However, we avoid a gcloud-shell "Dockerception" and save on build times.
	docker run --network host --rm -v $(DOCKER_VOLUME):/var/run/docker.sock gmp/kindtest ./hack/kind-test.sh

presubmit:   ## Run all checks and tests before submitting a change 
             ## Use DRY_RUN=1 to only validate without regenerating changes.
             ##
presubmit: regen bin test kindtest

CURRENT_TAG = v0.7.0-gke.0
CURRENT_PROM_TAG = v2.35.0-gmp.5-gke.0
CURRENT_AM_TAG = v0.24.0-gmp.3-gke.1
LABEL_API_VERSION = 0.7.0
FILES_TO_UPDATE = $(shell find . -type f -name "*.yaml" ! -name "kube-state-metrics.yaml" ! -name "node-exporter.yaml")
updateversions: $(SED)
	@echo ">> Updating prometheus-engine images in manifests to $(CURRENT_TAG)"
	@$(SED) -i -r 's#image: gke.gcr.io/prometheus-engine/(.*):.*#image: gke.gcr.io/prometheus-engine/\1:$(CURRENT_TAG)#g' $(FILES_TO_UPDATE) # This will match all, but Prom and AM will be fixed below.
	@echo ">> Updating prometheus images in manifests to $(CURRENT_PROM_TAG)"
	@$(SED) -i -r 's#image: gke.gcr.io/prometheus-engine/prometheus:.*#image: gke.gcr.io/prometheus-engine/prometheus:$(CURRENT_PROM_TAG)#g' $(FILES_TO_UPDATE)
	@echo ">> Updating alertmanager images in manifests to $(CURRENT_AM_TAG)"
	@$(SED) -i -r 's#image: gke.gcr.io/prometheus-engine/alertmanager:.*#image: gke.gcr.io/prometheus-engine/alertmanager:$(CURRENT_AM_TAG)#g' $(FILES_TO_UPDATE)
	@echo ">> Updating app.kubernetes.io/version to $(LABEL_API_VERSION)"
	@$(SED) -i -r 's#app.kubernetes.io/version: .*#app.kubernetes.io/version: $(LABEL_API_VERSION)#g' $(FILES_TO_UPDATE)
	@echo ">> Updating constant in export.go to $(LABEL_API_VERSION)"
	@$(SED) -i -r 's#	Version    = .*#	Version    = "$(LABEL_API_VERSION)"#g' pkg/export/export.go

