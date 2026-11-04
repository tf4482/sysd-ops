# Build, check, and install sysd-ops

.PHONY: build test lint install

# Static binary in dist/
build:
	env CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/sysd-ops .

# Tests with the race detector
test:
	go test -race ./...

# Formatting, vet, and staticcheck; U1000 is off because the shared frame files carry unused helpers
lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	staticcheck -checks=inherit,-U1000 ./...

# System-wide binary; run with sudo
install: build
	install -m 0755 dist/sysd-ops /usr/local/bin/sysd-ops
