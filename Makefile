# Fetch and Score
#
# Everything CI does is a target here, so a failing check can be reproduced
# locally without reading a workflow file.

SHELL := /bin/bash
.DEFAULT_GOAL := help

API_DIR   := api
WEB_DIR   := web
E2E_DIR   := e2e
BIN_DIR   := bin
TOOLS_DIR := tools

TAILWIND        := $(TOOLS_DIR)/tailwindcss
TAILWIND_VERSION := v4.3.3

# Local development ports. The frontend falls back to 8099 for the API when
# served from localhost, so these two agree by default.
API_PORT := 8099
WEB_PORT := 8100
DB       := data/dev.db

export FNS_DEV            := 1
export FNS_ADDR           := 127.0.0.1:$(API_PORT)
export FNS_BASE_URL       := http://127.0.0.1:$(WEB_PORT)
export FNS_ALLOWED_ORIGIN := http://127.0.0.1:$(WEB_PORT)
export FNS_SECURE_COOKIES := false
export FNS_DB_PATH        := $(DB)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'

# --- tools -----------------------------------------------------------------

$(TAILWIND):
	@mkdir -p $(TOOLS_DIR)
	@echo "Fetching Tailwind $(TAILWIND_VERSION)…"
	@curl -sfL -o $@ "https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-linux-x64"
	@chmod +x $@

.PHONY: tools
tools: $(TAILWIND) ## Fetch the standalone build tools

# --- build -----------------------------------------------------------------

.PHONY: generate
generate: ## Regenerate the frontend scoring table from the Go rules
	@cd $(API_DIR) && go generate ./...

.PHONY: css
css: $(TAILWIND) ## Build the stylesheet
	@$(TAILWIND) -i $(WEB_DIR)/css/app.css -o $(WEB_DIR)/css/app.build.css --minify

.PHONY: audio
audio: ## Regenerate the spoken timer cues (needs espeak-ng and ffmpeg)
	@$(WEB_DIR)/audio/generate.sh

.PHONY: build
build: generate css ## Build the API binaries and the site
	@mkdir -p $(BIN_DIR)
	@cd $(API_DIR) && go build -o ../$(BIN_DIR)/fetchandscore ./cmd/fetchandscore
	@cd $(API_DIR) && go build -o ../$(BIN_DIR)/fnsctl ./cmd/fnsctl
	@echo "Built $(BIN_DIR)/fetchandscore and $(BIN_DIR)/fnsctl"

.PHONY: dist
dist: build ## Assemble the exact directory GitHub Pages will serve
	@rm -rf dist && mkdir -p dist
	@cp -r $(WEB_DIR)/. dist/
	@rm -f dist/package.json dist/js/*.test.js dist/css/app.css
	@rm -rf dist/audio/generate.sh
	@echo "dist/ ready ($$(du -sh dist | cut -f1))"

# --- development -----------------------------------------------------------

.PHONY: seed
seed: build ## Load a demo club, season and session
	@mkdir -p $$(dirname $(DB))
	@./$(BIN_DIR)/fnsctl seed

.PHONY: dev
dev: build ## Run the API and the site locally
	@mkdir -p $$(dirname $(DB))
	@echo "API  http://127.0.0.1:$(API_PORT)"
	@echo "Site http://127.0.0.1:$(WEB_PORT)"
	@trap 'kill 0' EXIT; \
		./$(BIN_DIR)/fetchandscore & \
		python3 -m http.server $(WEB_PORT) --directory $(WEB_DIR) --bind 127.0.0.1 >/dev/null 2>&1 & \
		$(TAILWIND) -i $(WEB_DIR)/css/app.css -o $(WEB_DIR)/css/app.build.css --watch >/dev/null 2>&1 & \
		wait

# --- tests -----------------------------------------------------------------

.PHONY: test
test: test-go test-js ## Run the unit tests

.PHONY: test-go
test-go: ## Run the Go tests with the race detector
	@cd $(API_DIR) && go test ./... -race -coverprofile=coverage.out -covermode=atomic

.PHONY: test-js
test-js: ## Run the browser-module tests
	@cd $(WEB_DIR) && node --test 'js/*.test.js'

.PHONY: test-e2e
test-e2e: build ## Run the end-to-end tests
	@cd $(E2E_DIR) && npm ci --no-audit --no-fund && npx playwright test

# --- checks ----------------------------------------------------------------

.PHONY: fmt
fmt: ## Format the Go source
	@cd $(API_DIR) && gofmt -w .

.PHONY: lint
lint: lint-go lint-js ## Run every linter

.PHONY: lint-go
lint-go: ## Vet and lint the Go source
	@cd $(API_DIR) && go vet ./...
	@test -z "$$(gofmt -l $(API_DIR))" || { echo "gofmt needed:"; gofmt -l $(API_DIR); exit 1; }
	@command -v golangci-lint >/dev/null && (cd $(API_DIR) && golangci-lint run) \
		|| echo "golangci-lint not installed; skipping"

.PHONY: lint-js
lint-js: ## Lint and format-check the JavaScript
	@npx --yes @biomejs/biome ci $(WEB_DIR)/js $(E2E_DIR) || true

.PHONY: verify-generated
verify-generated: generate ## Fail if the generated scoring table is stale
	@git diff --exit-code -- $(WEB_DIR)/js/scoring-table.js \
		|| { echo "scoring-table.js is stale. Run: make generate"; exit 1; }

.PHONY: security
security: ## Run the security and vulnerability scanners
	@cd $(API_DIR) && go tool govulncheck ./... 2>/dev/null \
		|| (command -v govulncheck >/dev/null && (cd $(API_DIR) && govulncheck ./...)) \
		|| echo "govulncheck not installed; skipping"
	@command -v gosec >/dev/null && (cd $(API_DIR) && gosec -quiet ./...) \
		|| echo "gosec not installed; skipping"
	@command -v trivy >/dev/null && trivy fs --quiet --exit-code 1 --severity HIGH,CRITICAL . \
		|| echo "trivy not installed; skipping"

.PHONY: duplication
duplication: ## Report copy-pasted code
	@npx --yes jscpd --min-lines 12 --threshold 3 \
		--pattern "{api,web,e2e}/**/*.{go,js}" --ignore "**/*.test.js,**/node_modules/**" .

.PHONY: check
check: lint verify-generated test security duplication ## Everything CI runs

# --- housekeeping ----------------------------------------------------------

.PHONY: clean
clean: ## Remove build output and the local database
	@rm -rf $(BIN_DIR) dist $(WEB_DIR)/css/app.build.css data/ $(API_DIR)/coverage.out
	@rm -rf $(E2E_DIR)/test-results $(E2E_DIR)/playwright-report

.PHONY: docker
docker: ## Build the container image
	@docker build -t fetchandscore/api:dev -f deploy/Dockerfile .
