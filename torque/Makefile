BINARY := tangent-plugin-torque
ID     := tangent.plugin.torque
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

# An installable plugin directory: the binary beside the plugin.yaml it emits
# (tangent's host convention: `<binary> --manifest`, entrypoint beside the
# manifest), so the manifest the host installs is generated from the code that
# serves it.
dist: test
	mkdir -p $(DIST)
	go build -o $(DIST)/$(BINARY) ./cmd/$(BINARY)
	$(DIST)/$(BINARY) --manifest > $(DIST)/plugin.yaml
	@echo "installable plugin at $(DIST)"

clean:
	rm -rf $(DIST)
