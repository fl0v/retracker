#!/bin/bash
# Check if development tools are installed and accessible

echo "=== Checking Development Tools ==="
echo ""

# Check Go
echo "Go:"
if command -v go >/dev/null 2>&1; then
    echo "  ✓ Found: $(which go)"
    echo "  Version: $(go version)"
    echo "  GOPATH: $(go env GOPATH)"
    echo "  GOBIN: $(go env GOBIN)"
else
    echo "  ✗ Go not found"
    exit 1
fi

echo ""

# Check golangci-lint
echo "golangci-lint:"
GOLANGCI_LINT=""
if command -v golangci-lint >/dev/null 2>&1; then
    GOLANGCI_LINT=$(which golangci-lint)
    echo "  ✓ Found in PATH: $GOLANGCI_LINT"
elif [ -f "$(go env GOPATH)/bin/golangci-lint" ]; then
    GOLANGCI_LINT="$(go env GOPATH)/bin/golangci-lint"
    echo "  ✓ Found in GOPATH/bin: $GOLANGCI_LINT"
    echo "  ⚠ Not in PATH. Add to PATH:"
    echo "    export PATH=\$PATH:$(go env GOPATH)/bin"
elif [ -f "$HOME/go/bin/golangci-lint" ]; then
    GOLANGCI_LINT="$HOME/go/bin/golangci-lint"
    echo "  ✓ Found in ~/go/bin: $GOLANGCI_LINT"
    echo "  ⚠ Not in PATH. Add to PATH:"
    echo "    export PATH=\$PATH:$HOME/go/bin"
else
    echo "  ✗ Not found"
    echo "  Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
fi

if [ -n "$GOLANGCI_LINT" ]; then
    echo "  Version: $($GOLANGCI_LINT --version 2>/dev/null || echo 'unknown')"
fi

echo ""

# Check gofumpt
echo "gofumpt:"
GOFUMPT=""
if command -v gofumpt >/dev/null 2>&1; then
    GOFUMPT=$(which gofumpt)
    echo "  ✓ Found in PATH: $GOFUMPT"
elif [ -f "$(go env GOPATH)/bin/gofumpt" ]; then
    GOFUMPT="$(go env GOPATH)/bin/gofumpt"
    echo "  ✓ Found in GOPATH/bin: $GOFUMPT"
    echo "  ⚠ Not in PATH. Add to PATH:"
    echo "    export PATH=\$PATH:$(go env GOPATH)/bin"
elif [ -f "$HOME/go/bin/gofumpt" ]; then
    GOFUMPT="$HOME/go/bin/gofumpt"
    echo "  ✓ Found in ~/go/bin: $GOFUMPT"
    echo "  ⚠ Not in PATH. Add to PATH:"
    echo "    export PATH=\$PATH:$HOME/go/bin"
else
    echo "  ✗ Not found (optional, but recommended)"
    echo "  Install with: go install mvdan.cc/gofumpt@latest"
fi

if [ -n "$GOFUMPT" ]; then
    echo "  Version: $($GOFUMPT -version 2>/dev/null || echo 'unknown')"
fi

echo ""

# Check gofmt (always available with Go)
echo "gofmt:"
if command -v gofmt >/dev/null 2>&1; then
    echo "  ✓ Found: $(which gofmt)"
else
    echo "  ✗ Not found (should be with Go installation)"
fi

echo ""
echo "=== PATH Check ==="
echo "Current PATH:"
echo "$PATH" | tr ':' '\n' | grep -E "(go|bin)" | sed 's/^/  /'

echo ""
echo "=== Recommendations ==="
if [ -z "$GOLANGCI_LINT" ]; then
    echo "1. Install golangci-lint:"
    echo "   go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
    echo ""
fi

GOPATH_BIN="$(go env GOPATH)/bin"
if [[ ":$PATH:" != *":$GOPATH_BIN:"* ]] && [ -d "$GOPATH_BIN" ]; then
    echo "2. Add Go bin directory to your PATH:"
    echo "   Add this to your ~/.bashrc or ~/.zshrc:"
    echo "   export PATH=\$PATH:$GOPATH_BIN"
    echo ""
fi

echo "=== Done ==="
