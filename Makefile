# Shared entry points for every plugin in this repo. Each plugin is its own Go
# module under its own directory, so these targets iterate over the list rather
# than recursing into a single build.
#
# Adding a plugin means adding its directory name here and nothing else.

PLUGINS := github runner tesseract torque messaging

.PHONY: all test lint dist clean

all: test dist

test:
	@for p in $(PLUGINS); do \
		echo "==> test $$p"; \
		(cd $$p && $(MAKE) test) || exit 1; \
	done

lint:
	@for p in $(PLUGINS); do \
		echo "==> lint $$p"; \
		(cd $$p && $(MAKE) lint) || exit 1; \
	done

# dist/tangent.plugin.<plugin>/ is an installable plugin directory: the binary
# beside the plugin.yaml it emits. Install one with:
#   tangent plugin install "$PWD/dist/tangent.plugin.<plugin>"
dist:
	@for p in $(PLUGINS); do \
		echo "==> dist $$p"; \
		(cd $$p && $(MAKE) dist) || exit 1; \
	done

clean:
	rm -rf dist

# Source verification only. This never uses the operator's installed plugins,
# channel routes, provider credentials, or running Tangent database.
.PHONY: smoke
smoke:
	$(MAKE) -C messaging smoke-stage1
