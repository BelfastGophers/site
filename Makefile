# Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

.PHONY: run test lint check install-hooks

# run starts the service with logs a terminal can read, and no shutdown
# delay, since no load balancer is waiting to notice. Flags win over the
# environment, so those two are fixed here; other settings come from the
# environment, as in production: PORT=9000 make run.
run:
	go run ./cmd/... -log-format text -shutdown-delay 0

# test runs every test with the race detector, as CI does.
test:
	go test -race ./...

# lint runs the linters CI runs.
lint:
	@command -v golangci-lint >/dev/null || { \
		echo "golangci-lint not found: see https://golangci-lint.run/welcome/install/"; exit 1; \
	}
	golangci-lint run

# check runs what CI checks on every change, so a push does not fail on
# something you could have caught: formatting, vet, the tests and the
# linters. CI also checks complexity, and that the managed files match httpgen.
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	$(MAKE) --no-print-directory test lint

# install-hooks points git at .githooks. Run it after cloning: core.hooksPath
# is local to your copy of the repository, so it is never cloned with it.
install-hooks:
	go tool httpgen hooks
