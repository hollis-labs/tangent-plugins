BINARY := tangent-plugin-runner
ID     := tangent.plugin.runner
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
# canonical JSON declaration.
dist: test
	mkdir -p $(DIST)/bin
	go build -o $(DIST)/bin/$(BINARY) ./cmd/$(BINARY)
	$(DIST)/bin/$(BINARY) --manifest > $(DIST)/plugin.yaml
	@echo "installable plugin at $(DIST)"

clean:
	rm -rf $(DIST)
