.PHONY: build test test-race test-logs coverage coverage-ci fmt-check vet lint vuln-check version-bump version-sync version-check version-test release-check check frontend-check e2e example-check example-ollama run docker-build

APP_VERSION := $(shell tr -d '\r\n' < VERSION)
LDFLAGS := -s -w -X knowledge-ba46-flow/internal/version.Version=$(APP_VERSION)

build: version-check
	go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o KBM-flow ./cmd/KBM-flow

test:
	go test ./...

test-race:
	go test -race ./...

# test-logs runs the suite through go test -json and fails on error output
# inside otherwise-passing tests (panics, WARNINGs, relayed client errors) —
# the class `go test` hides by buffering passing packages' output. Intended
# error-path noise is allowlisted in scripts/audit-test-logs. In CI the
# coverage job audits the test-report.json stream it already produces.
test-logs:
	go test -json ./... | go run ./scripts/audit-test-logs

coverage:
	# -coverpkg instruments every package in each test binary: without it a
	# line exercised only from another package's tests reports as uncovered
	# and both Codecov and SonarQube under-attribute coverage.
	go test -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# coverage-ci produces the two artifacts the coverage/scan services ingest:
# coverage.out (Go coverprofile, consumed by Codecov and SonarQube) and
# test-report.json (go test -json stream, consumed by SonarQube's Go analyzer
# as test execution data).
coverage-ci:
	go test -json -coverpkg=./... -coverprofile=coverage.out ./... > test-report.json

fmt-check:
	@files="$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))"; \
	if [ -n "$$files" ]; then echo "$$files"; exit 1; fi

vet:
	go vet ./...

# golangci-lint runs the curated analyzer set from .golangci.yml. It is a
# separate binary, not a Go tool dependency — install with
# `brew install golangci-lint` (or the CI action). Without the binary the
# target self-skips; LINT_REQUIRED=1 fails closed.
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed — skipping (brew install golangci-lint)"; \
		test -z "$$LINT_REQUIRED" || exit 1; \
	else \
		golangci-lint run; \
	fi

# govulncheck reports reachable Go vulnerability advisories; needs network
# access to the Go vuln database, so it is separate from `check`.
vuln-check:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Semgrep SAST — same registry rulesets as the CI `semgrep` job. Needs the
# semgrep CLI (brew/pipx) and network access to semgrep.dev for rules.
# The JSF autoescape rule is a Java-only check that misfires on Alpine.js
# @keydown.escape attributes in the embedded HTML — excluded here and in CI.
semgrep:
	semgrep scan --config p/golang --config p/security-audit --metrics=off --error --exclude-rule java.lang.security.audit.xss.jsf.autoescape-disabled.autoescape-disabled

version-sync:
	npm run version:sync

version-bump:
	@test -n "$(TYPE)" || (echo "usage: make version-bump TYPE=major|minor|patch" >&2; exit 1)
	npm run version:bump -- "$(TYPE)"

version-check:
	sh scripts/check-version.sh

# Exercises the bump/sync/check scripts against a throwaway metadata copy.
version-test:
	npm run version:test

release-check:
	@test -n "$(RELEASE_TAG)" || (echo "usage: make release-check RELEASE_TAG=v$(APP_VERSION)" >&2; exit 1)
	sh scripts/check-version.sh "$(RELEASE_TAG)"

install-hooks:
	git config core.hooksPath hooks

# Full local quality gate — same checks CI runs plus an optional local
# SonarCloud upload. Needs network (semgrep rules, govulncheck, sonarcloud).
# Export SONAR_TOKEN (sonarcloud.io account token) to include sonar.
quality: check frontend-check semgrep vuln-check coverage-ci sonar

# SonarCloud upload via the local `sonar` CLI — reads
# sonar-project.properties (key, org, host URL, coverage paths).
# Skipped quietly without SONAR_TOKEN; CI stays the authoritative scan.
sonar:
	@if [ -z "$$SONAR_TOKEN" ]; then \
		echo "SONAR_TOKEN not set — skipping SonarCloud upload"; \
	else \
		command -v sonar >/dev/null || { echo "sonar CLI not found — brew install sonar-scanner or sonarqube-cli"; exit 1; }; \
		sonar -Dsonar.token="$$SONAR_TOKEN"; \
	fi

check: version-check fmt-check vet lint test-race build

frontend-check:
	npm ci --ignore-scripts
	npm run check

# Browser e2e: drives the real web UI in system Chrome via the DevTools
# Protocol against a seeded fixture — no Ollama, no network. Skips cleanly
# when no Chrome/Chromium is installed (set KBM_E2E_CHROME to point at one).
e2e:
	npm run e2e

# Fast, deterministic scanner/analyzer checks over the curated example shelves.
example-check:
	go test ./internal/examplecorpus

# Explicit opt-in to live local Ollama generation; never accepts or applies.
example-ollama:
	@test -n "$(TIER)" || (echo "usage: make example-ollama TIER=basic|intermediate|e2e|llm-eval" >&2; exit 1)
	scripts/run-example-ollama.sh "$(TIER)"

run: version-check
	go run -ldflags "$(LDFLAGS)" ./cmd/KBM-flow run

docker-build:
	docker compose build
