VERSION ?= $(shell date +%Y.%m.%d)-$(shell git rev-parse --short HEAD 2>/dev/null || echo local)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: dev build pi pi32 amd64 all release test race fuzz load check vet staticcheck vuln gosec shellcheck clean

build:
	$(BUILD) -o dist/attendance ./cmd/attendance

dev: build  ## run everything locally (see scripts/dev.sh)
	$(BUILD) -o dist/devseed ./cmd/devseed
	./scripts/dev.sh

pi:      ## Raspberry Pi / ARM mini PC, 64-bit OS
	GOOS=linux GOARCH=arm64 $(BUILD) -o dist/attendance-linux-arm64 ./cmd/attendance
pi32:    ## Raspberry Pi, 32-bit OS
	GOOS=linux GOARCH=arm GOARM=7 $(BUILD) -o dist/attendance-linux-arm ./cmd/attendance
amd64:   ## x86 mini PC or the admin PC
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/attendance-linux-amd64 ./cmd/attendance
all: pi pi32 amd64

# Files for a GitHub release, as deploy/install.sh downloads them. The bundle
# is the committed deploy/, agent/ and docs/ (git archive), so commit first.
release: amd64 pi
	git diff --quiet HEAD -- deploy agent docs 'README*.md' || { echo "commit deploy/ agent/ docs/ and the READMEs first" >&2; exit 1; }
	git archive --format=tar.gz -o dist/attendance-deploy.tar.gz HEAD deploy agent docs ':(glob)README*.md'
	cp deploy/install.sh dist/install.sh
	cd dist && sha256sum attendance-linux-amd64 attendance-linux-arm64 attendance-deploy.tar.gz install.sh > SHA256SUMS

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
	shellcheck agent/idle_logout deploy/alert/attendance-alert deploy/install.sh scripts/dev.sh

clean:
	rm -rf dist
