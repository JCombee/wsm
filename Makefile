# wsm build, test and lint targets. Binaries are static (CGO_ENABLED=0).

GO          ?= go
VERSION     ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
MARKER      := /etc/wsm-spike-disposable

# staticcheck keeps its cache next to the Go build cache, so lint works
# wherever GOCACHE is writable.
export STATICCHECK_CACHE ?= $(shell $(GO) env GOCACHE)/staticcheck

export CGO_ENABLED := 0

.PHONY: build test lint isolation-build isolation-test clean

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/wsm ./cmd/wsm
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/wsm-helper ./cmd/wsm-helper

test:
	$(GO) test -count=1 ./...

lint:
	@out=$$(gofmt -l $(wildcard cmd internal test)); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet -tags isolation ./...
	$(GO) run $(STATICCHECK) -tags isolation ./...

# isolation-build compiles the isolation tests into one static binary, to be
# copied to a disposable VM together with bin/ and deploy/.
isolation-build:
	$(GO) test -c -trimpath -tags isolation -o bin/isolation.test ./test/isolation

# isolation-test refuses to run anywhere but a disposable VM, as root. It
# checks this before it builds or changes anything.
isolation-test:
	@if [ ! -e $(MARKER) ]; then \
		echo "refusing to run: $(MARKER) is missing; run isolation tests only on a disposable VM" >&2; exit 3; fi
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "refusing to run: isolation tests must run as root" >&2; exit 4; fi
	$(MAKE) isolation-build
	./bin/isolation.test -test.v -test.count=1 -test.timeout=45m

clean:
	rm -rf bin
