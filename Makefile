.PHONY: help build test lint security fmt vet clean docker-build docker-load

help:
	@echo "Gitea Actions Runner Controller Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build            - Build the manager binary"
	@echo "  test             - Run tests"
	@echo "  lint             - Run all linters"
	@echo "  security         - Run security checks"
	@echo "  fmt              - Format code"
	@echo "  vet              - Run go vet"
	@echo "  clean            - Clean build artifacts"
	@echo "  docker-build     - Build Docker image"
	@echo "  docker-load      - Load Docker image into kind cluster"

# Build the manager binary
build: fmt vet
	@echo "Building manager binary..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go build -o bin/manager ./cmd/manager'

# Run tests
test:
	@echo "Running tests..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go test ./...'

# Format code with gofmt
fmt:
	@echo "Checking gofmt..."
	@zsh -lc 'cd $$(pwd) && mise exec -- gofmt -l . 2>/dev/null | grep -v "^vendor/" | grep -v "zz_generated" || echo "gofmt: OK"'

# Install goimports if missing and format with it
goimports:
	@echo "Installing goimports..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go install golang.org/x/tools/cmd/goimports@latest'
	@echo "Running goimports..."
	@zsh -lc 'cd $$(pwd) && mise exec -- goimports -l . 2>/dev/null | grep -v "^vendor/" | grep -v "zz_generated" || echo "goimports: OK"'

# Run go vet
vet:
	@echo "Running go vet..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go vet ./...'

# Install golangci-lint and run it
golangci-lint:
	@echo "Installing golangci-lint..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.57.0'
	@echo "Running golangci-lint..."
	@zsh -lc 'cd $$(pwd) && mise exec -- golangci-lint run --timeout=5m --enable=govet,staticcheck,errcheck,ineffassign,unused,gocritic,revive'

# Install gosec and run it
gosec:
	@echo "Installing gosec..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go install github.com/securego/gosec/v2/cmd/gosec@latest'
	@echo "Running gosec..."
	@zsh -lc 'cd $$(pwd) && mise exec -- gosec ./...'

# Install semgrep and run it (requires pipx or brew)
semgrep:
	@command -v semgrep >/dev/null 2>&1 || { echo "Installing semgrep via pipx..."; pipx install semgrep; }
	@echo "Running semgrep..."
	@semgrep --config auto

# Run lint target (combines gofmt, goimports, go vet)
lint: fmt goimports vet
	@echo "Lint checks completed."

# Run security checks (gosec)
security: gosec
	@echo "Security checks completed."

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf bin/
	@zsh -lc 'cd $$(pwd) && mise exec -- go clean ./...'

# Build Docker image
docker-build: build
	@echo "Building Docker image..."
	@docker build -t gitea-runner-controller:latest .

# Load Docker image into kind cluster
docker-load: docker-build
	@echo "Loading Docker image into garc-dev kind cluster..."
	@kind load docker-image --name garc-dev gitea-runner-controller:latest

# Install tools needed for build
install-tools:
	@echo "Installing required build tools..."
	@zsh -lc 'cd $$(pwd) && mise exec -- go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0'
	@zsh -lc 'cd $$(pwd) && mise exec -- go install golang.org/x/tools/cmd/goimports@latest'
	@zsh -lc 'cd $$(pwd) && mise exec -- go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.57.0'
	@zsh -lc 'cd $$(pwd) && mise exec -- go install github.com/securego/gosec/v2/cmd/gosec@latest'
	@echo "Use: pipx install semgrep  (or brew install semgrep)"
