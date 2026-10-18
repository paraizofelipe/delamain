GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK   := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
IT_SESSION    ?= delamain-test

VERSION   := $(shell cat VERSION)
LDFLAGS   := -s -w -X github.com/paraizo/delamain/internal/cli.Version=v$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
BUMP      ?= patch

.PHONY: build dist bump test lint vuln it check

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/delamain ./cmd/delamain

# Release assets: dist/delamain_<os>_<arch> plus checksums.txt, as install.sh and upgrade expect.
dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/delamain_$${os}_$${arch} ./cmd/delamain || exit 1; \
	done
	cd dist && shasum -a 256 delamain_* > checksums.txt

# make bump [BUMP=patch|minor|major]: rewrites VERSION; commit it and merge to main to release.
bump:
	@echo '$(VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION '$(VERSION)' is not X.Y.Z" >&2; exit 1; }
	@case '$(BUMP)' in patch|minor|major) ;; *) echo "BUMP must be patch|minor|major, got '$(BUMP)'" >&2; exit 1;; esac
	@echo '$(VERSION)' | awk -F. -v b='$(BUMP)' '{ \
		if (b == "major") { $$1++; $$2 = 0; $$3 = 0 } else if (b == "minor") { $$2++; $$3 = 0 } else { $$3++ } \
		printf "%d.%d.%d\n", $$1, $$2, $$3 }' > VERSION
	@echo "$(VERSION) → $$(cat VERSION)"

test:
	timeout 180s go test -race ./...

lint:
	timeout 600s $(GOLANGCI_LINT) run ./...

vuln:
	timeout 300s $(GOVULNCHECK) ./...

# Requires an isolated server: herdr --session $(IT_SESSION) server
it:
	DELAMAIN_IT_HERDR_SESSION=$(IT_SESSION) timeout 120s go test ./internal/herdr/ -run Integration -v

check: lint test vuln
