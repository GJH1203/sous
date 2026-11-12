# Run from Linux/WSL2. Python tools come from the uv-managed venv in inference/.
PY := cd inference && uv run --locked

.PHONY: check go-check py-check fmt

## check: formatting, static analysis and all tests for both languages.
check: go-check py-check

go-check:
	@# go list (not "gofmt -l .") so the walk skips inference/.venv.
	@unformatted="$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed (run: make fmt):"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	@# A deadlocked test fails after 60s instead of Go's default 10 minutes.
	go test -race -timeout 60s ./...

py-check:
	$(PY) ruff format --check .
	$(PY) ruff check .
	$(PY) pytest

## fmt: rewrite files in place to satisfy the formatters.
fmt:
	go fmt ./...
	$(PY) ruff format .
	$(PY) ruff check --fix .
