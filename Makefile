.PHONY: help test go-test test-schema test-runtime coverage clean lint install-deps up down

CH_HTTP_PORT ?= 18123
CH_TCP_PORT  ?= 19000

# Go anonymization pipeline (anon/ module). The de-anon / masking / leak-guard
# code is security-critical, so its unit suite + a coverage floor gate releases
# alongside the schema/runtime suites (CLAUDE.md hard rule #1/#2).
GO_DIR       ?= anon
GO_COVER_MIN ?= 80

# Prefer the `$(DOCKER_COMPOSE)` v2 plugin if available, else fall back to v1.
DOCKER_COMPOSE := $(shell $(DOCKER_COMPOSE) version >/dev/null 2>&1 && echo "$(DOCKER_COMPOSE)" || echo "docker-compose")
PIP            := $(shell command -v pip >/dev/null 2>&1 && echo pip || echo pip3)

help:
	@echo "bentoclick — make targets"
	@echo ""
	@echo "  make test          Full suite: go-test + schema (pytest) + runtime (vitest)"
	@echo "  make go-test       go vet + build + unit tests for anon/ with a $(GO_COVER_MIN)% coverage floor"
	@echo "  make test-schema   pytest tests/schema/ against CH 26.3 in Docker"
	@echo "  make test-runtime  vitest run --coverage tests/runtime/"
	@echo "  make coverage      Same as test-runtime; HTML report at tests/runtime/coverage/"
	@echo "  make up            Start the ClickHouse test container in background"
	@echo "  make down          Stop and remove the test container"
	@echo "  make clean         make down + remove caches and coverage artifacts"
	@echo "  make install-deps  pip install -r tests/requirements.txt && npm --prefix tests install"

VENV_DIR := tests/.venv
VENV_PY  := $(VENV_DIR)/bin/python
VENV_PIP := PIP_USER=0 $(VENV_DIR)/bin/pip

$(VENV_DIR):
	python3 -m venv $(VENV_DIR)
	$(VENV_PIP) install --upgrade pip >/dev/null

install-deps: $(VENV_DIR)
	$(VENV_PIP) install -r tests/requirements.txt
	npm install

up:
	$(DOCKER_COMPOSE) -f tests/docker-compose.test.yml up -d
	@echo "Waiting for ClickHouse on :$(CH_HTTP_PORT)..."
	@for i in $$(seq 1 30); do \
	  curl -fsS "http://localhost:$(CH_HTTP_PORT)/ping" >/dev/null 2>&1 && exit 0; \
	  sleep 1; \
	done; \
	echo "ClickHouse did not become healthy in 30s" >&2; exit 1

down:
	$(DOCKER_COMPOSE) -f tests/docker-compose.test.yml down -v

test-schema: up
	$(VENV_PY) -m pytest tests/schema -v
	@$(MAKE) down

test-runtime:
	npx vitest run --coverage --config tests/vitest.config.ts

coverage: test-runtime
	@echo "Coverage report at tests/runtime/coverage/index.html"

go-test:
	cd $(GO_DIR) && go vet ./...
	cd $(GO_DIR) && go build ./...
	cd $(GO_DIR) && go test ./internal/... -coverprofile=coverage.out -covermode=atomic
	@cd $(GO_DIR) && total=$$(go tool cover -func=coverage.out | awk '/^total:/{print $$3+0}'); \
	  echo "==> Go internal/ coverage: $$total% (floor $(GO_COVER_MIN)%)"; \
	  awk "BEGIN{exit ($$total < $(GO_COVER_MIN))}" || { echo "ERROR: Go coverage $$total% is below the $(GO_COVER_MIN)% floor" >&2; exit 1; }

test: go-test
	@./tests/run.sh

clean: down
	rm -rf tests/runtime/coverage tests/.cache tests/clickhouse-data
	find . -type d -name __pycache__ -prune -exec rm -rf {} +
	find . -type d -name .pytest_cache -prune -exec rm -rf {} +
