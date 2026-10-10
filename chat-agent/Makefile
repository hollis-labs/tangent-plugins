BINARY := tangent-plugin-chat-agent
ID := tangent.plugin.chat-agent
DIST := ../dist/$(ID)

.PHONY: test lint build dist
test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	golangci-lint run --config ../.golangci.yml ./...

build:
	go build ./...

# Tests run separately in CI and the author gate; packaging starts no plugin,
# endpoint or provider. The declaration hashes this exact native payload.
dist: build
	@set -eu; \
	mkdir -p "$$(dirname "$(DIST)")"; \
	stage=$$(mktemp -d "$(DIST).build.XXXXXX"); \
	trap 'rm -rf "$$stage"' EXIT; \
	trap 'exit 1' HUP INT TERM; \
	mkdir -p "$$stage/bin"; \
	go build -o "$$stage/bin/$(BINARY)" ./cmd/$(BINARY); \
	"$$stage/bin/$(BINARY)" --manifest > "$$stage/plugin.yaml"; \
	rm -rf "$(DIST)"; \
	mv "$$stage" "$(DIST)"; \
	echo "installable plugin at $(DIST)"

# Source-only lifecycle acceptance against the pinned public host.
.PHONY: smoke
smoke:
	go test -race -count=1 -timeout=240s -v ./integration

.PHONY: check
check: lint test dist
