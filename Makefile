GO ?= go

.PHONY: help
help:
	@echo "run         end-to-end demo: fixture daemon + MCP server + raw JSON-RPC transcript"
	@echo "test        go test ./..."
	@echo "build       build both binaries into bin/"
	@echo "fixture     run the fixture kmesh daemon on :15200"
	@echo "serve       run the MCP server against a fixture daemon on :15200"
	@echo "transcript  regenerate docs/demo-output.txt"
	@echo "check       vet + test"

# The single command in the README. Runs the whole stack in one process and
# prints the wire traffic. No cluster, no eBPF, no root, no ports to free.
.PHONY: run
run:
	$(GO) run ./cmd/demo

.PHONY: test
test:
	$(GO) test ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: check
check: vet test

.PHONY: build
build:
	$(GO) build -o bin/ ./cmd/...

# Two-terminal mode, for poking at the server with a real MCP client.
.PHONY: fixture
fixture:
	$(GO) run ./cmd/fixture -mode dual-engine

.PHONY: serve
serve:
	$(GO) run ./cmd/kmesh-mcp -daemon localhost:15200 -listen :8080

.PHONY: transcript
transcript:
	@mkdir -p docs
	$(GO) run ./cmd/demo > docs/demo-output.txt
	@echo "wrote docs/demo-output.txt"

.PHONY: clean
clean:
	rm -rf bin
