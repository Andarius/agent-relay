set quiet

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`

# List recipes
default:
    @just --list

# Run the tests
test:
    go vet ./... && go test ./...

# Build the local binary
build: test
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.version={{version}}" -o agent-relay .

# Cross-compile release binaries into dist/
dist: test
    rm -rf dist && mkdir -p dist
    for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
      os=${t%/*}; arch=${t#*/}; ext=$([ "$os" = windows ] && echo .exe || true); \
      GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -ldflags="-s -w -X main.version={{version}}" -o "dist/agent-relay-$os-$arch$ext" . ; \
    done
    cd dist && sha256sum agent-relay-* > SHA256SUMS
    cp install.sh dist/
    ls -la dist

# Tag VERSION (e.g. v0.1.0) and publish the release binaries to GitHub
release VERSION:
    git diff --quiet HEAD || (echo "commit first" >&2; exit 1)
    git tag {{VERSION}} && git push origin {{VERSION}}
    just dist
    gh release create {{VERSION}} dist/* --title {{VERSION}} --generate-notes
