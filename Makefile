VERSION ?= $(shell date +%Y.%m.%d)-$(shell git rev-parse --short HEAD 2>/dev/null || echo local)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: dev build pi pi32 amd64 all test race fuzz load check vet staticcheck vuln gosec shellcheck clean

build:
	$(BUILD) -o dist/attendance ./cmd/attendance

dev: build  ## run everything locally (see scripts/dev.sh)
	./scripts/dev.sh

pi:      ## Raspberry Pi / ARM mini PC, 64-bit OS
	GOOS=linux GOARCH=arm64 $(BUILD) -o dist/attendance-linux-arm64 ./cmd/attendance
pi32:    ## Raspberry Pi, 32-bit OS
	GOOS=linux GOARCH=arm GOARM=7 $(BUILD) -o dist/attendance-linux-arm ./cmd/attendance
amd64:   ## x86 mini PC or the admin PC
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/attendance-linux-amd64 ./cmd/attendance
all: pi pi32 amd64

test:
	go test ./...
race:
	go test -race ./...
fuzz:
	go test ./internal/token -run '^$$' -fuzz FuzzParse -fuzztime 60s
load:
	go test ./internal/core -run TestLoad50PCs -v

# Static checks from the spec (7.1). The tools are run at pinned versions.
check: vet staticcheck vuln gosec shellcheck race
vet:
	go vet ./...
staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
gosec:
	go run github.com/securego/gosec/v2/cmd/gosec@v2.30.0 -severity high ./...
shellcheck:
	shellcheck agent/idle_logout deploy/alert/attendance-alert

clean:
	rm -rf dist
