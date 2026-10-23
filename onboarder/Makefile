# The onboarder: a controller, released as a Supervisor Service. The release
# and pre-release pattern follows warroyo/ansible-supervisor-service.
#
#   make test                          unit tests (all CI runs)
#   make release VERSION=0.1.0         package an image already pushed for
#                                      VERSION into platform-observability-onboarder.yml
#
# Pre-release gate, run by hand before a tag, with .env filled in (see
# .env.example). None of it runs in CI: it pushes to the registry and needs a
# vCenter, a Supervisor and VCF Operations.
#
#   make dev-release                   build, push and package DEV_VERSION
#   make install-supervisor-service    register and install or upgrade it
#   make verify-supervisor             assert it against the live Supervisor
IMAGE := ghcr.io/warroyo/platform-observability-addon/onboarder
PKG   := platform-observability-onboarder.field.vmware.com

# Read out of .env for make; an exported value or the command line wins.
DEV_VERSION ?= $(shell test/lib/dotenv.sh DEV_VERSION 2>/dev/null)

test:
	go vet ./... && go test -race ./...

build:
	@test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 2; }
	docker build -t $(IMAGE):$(VERSION) .

push:
	@test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 2; }
	docker push $(IMAGE):$(VERSION)

# Expects the image for VERSION in the registry already (build, then push).
# The kbld config pins the image by digest, not by tag: it ships in the
# bundle, and a tag there would send kapp-controller back to the registry on
# every deploy, which unpins the image and breaks air-gapped installs.
release:
	@test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 2; }
	printf 'apiVersion: kbld.k14s.io/v1alpha1\nkind: Config\noverrides:\n- image: onboarder\n  newImage: %s@%s\n' \
		"$(IMAGE)" \
		"$$(docker buildx imagetools inspect $(IMAGE):$(VERSION) --format '{{.Manifest.Digest}}')" \
		> config/config-release.yml
	kctrl package release -y -v $(VERSION) -t $(VERSION)
	cp carvel-artifacts/packages/$(PKG)/metadata.yml platform-observability-onboarder.yml
	printf '\n---\n' >> platform-observability-onboarder.yml
	cat carvel-artifacts/packages/$(PKG)/package.yml >> platform-observability-onboarder.yml

# A dev release is an ordinary release under a pre-release version, so it
# exercises the real build, digest pinning and packaging.
dev-release:
	@test -n "$(DEV_VERSION)" || { echo "set DEV_VERSION in .env or on the command line, e.g. make dev-release DEV_VERSION=0.1.0-rc1" >&2; exit 1; }
	$(MAKE) build VERSION=$(DEV_VERSION)
	$(MAKE) push VERSION=$(DEV_VERSION)
	$(MAKE) release VERSION=$(DEV_VERSION)
	@echo
	@echo "platform-observability-onboarder.yml is built for $(DEV_VERSION)."
	@echo "Next: make install-supervisor-service, then make verify-supervisor."

# The Supervisor's RBAC refuses even a vSphere administrator ClusterRoles,
# so the service lands only through vCenter's service-install API.
install-supervisor-service:
	test/install-supervisor-service.sh

# EXPECT_IMAGE is resolved here, so the check is against the digest actually
# in the registry for this version, which catches a stale install.
verify-supervisor:
	@test -n "$(DEV_VERSION)" || { echo "set DEV_VERSION to the version you installed" >&2; exit 1; }
	EXPECT_IMAGE="$(IMAGE)@$$(docker buildx imagetools inspect $(IMAGE):$(DEV_VERSION) --format '{{.Manifest.Digest}}')" \
		test/verify-supervisor.sh

.PHONY: test build push release dev-release install-supervisor-service verify-supervisor
