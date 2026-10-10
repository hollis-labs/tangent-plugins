BINARY := tangent-plugin-messaging
ID := tangent.plugin.messaging
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

# Real compiled consumer and the pinned public Tangent host; only test-owned
# databases, sockets and synthetic channel/gateway/dispatcher traffic.
.PHONY: smoke-stage1
smoke-stage1:
	go test -race -count=1 -timeout=240s -v ./integration -run '^TestStage1'
