BINARY := tangent-plugin-github
ID     := tangent.plugin.github
DIST   := ../dist/$(ID)

.PHONY: test lint build dist clean

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	golangci-lint run --config ../.golangci.yml ./...

build:
	go build ./...

# A manifest-v2 plugin bundle: the native payload under bin/ and its
# canonical JSON declaration. Build fresh so legacy/stale payloads cannot
# survive in the exact inventory; retain the previous bundle on build failure.
dist: test
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

clean:
	rm -rf $(DIST)
