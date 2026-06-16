GO ?= go
CONNECTION ?= demo

build:
	$(GO) build ./...
	$(GO) build -o bin/anond ./cmd/anond

test:
	$(GO) test ./... 2>&1 | tee $(TMPDIR)/anon-test.log
	@! grep -E "^(FAIL|--- FAIL|panic)" $(TMPDIR)/anon-test.log

# Integration tests need a reachable clickhouse-client connection and admin
# rights to create the test meta DB + sandbox DBs (cleaned up afterwards).
itest:
	ANON_TEST_CONNECTION=$(CONNECTION) $(GO) test -count=1 -timeout 20m ./internal/integration/... 2>&1 | tee $(TMPDIR)/anon-itest.log
	@! grep -E "^(FAIL|--- FAIL|panic)" $(TMPDIR)/anon-itest.log

# Dry-run smoke against the demo server: discovery + map build, no writes.
smoke: build
	ANON_HMAC_KEY=smoke-test-key-0123456789abcdef ./bin/anond run --connection $(CONNECTION) --databases git --dry-run

vet:
	$(GO) vet ./...

.PHONY: build test itest smoke vet
