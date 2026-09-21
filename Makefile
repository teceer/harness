BIN ?= $(HOME)/bin/harness

.PHONY: build test install hooks
build:
	go build -trimpath -ldflags "-s -w" -o bin/harness .
	cc -O2 -Wall -o bin/harness-keywait keywait/keywait.c -framework ApplicationServices

test:
	go test ./...

install: build
	install -m 0755 bin/harness $(BIN)
	install -m 0755 bin/harness-keywait $(dir $(BIN))harness-keywait

# register hooks in every CLAUDE_CONFIG_DIR from ~/.harness/config.toml
hooks: install
	$(BIN) install-hooks
