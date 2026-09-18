

.PHONY: build test glua

# Build information (git commit, tag, build date) is injected at link time via
# -ldflags; version.go is static and never regenerated.
LDFLAGS := -X 'main.GitCommit=$(shell git rev-list -1 HEAD)' \
           -X 'main.GitTag=$(shell git tag --sort=v:refname | tail -1)' \
           -X 'main.BuildDate=$(shell date)'

build:
	./_tools/go-inline *.go && go fmt . &&  go build 

glua: *.go pm/*.go cmd/glua/*.go
	./_tools/go-inline *.go && go fmt . && go build -ldflags "$(LDFLAGS)" -o cmd/glua/glua ./cmd/glua

test:
	./_tools/go-inline *.go && go fmt . &&  go test




