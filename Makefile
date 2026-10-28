.PHONY: build test check integration install
PREFIX ?= /usr/local
TAG ?=
VERSION ?= $(if $(TAG),$(TAG),$(shell git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo dev))
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X 'gcloud-local/internal/localbuild.Version=$(VERSION)'" -o gcloud-local ./cmd/gcloud-local
test:
	go test ./...
check:
	go vet ./...
integration:
	GCLOUD_LOCAL_INTEGRATION=1 go test -v ./internal/localbuild -run TestDockerIntegration
install: build
	install -Dm755 gcloud-local $(DESTDIR)$(PREFIX)/bin/gcloud-local
