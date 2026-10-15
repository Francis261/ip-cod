# ipcod — build & packaging
#
# Primary targets:
#   make build            build all binaries for the host platform
#   make test             run unit tests + vet
#   make release          cross-compile static binaries for common VPS targets
#   make install-agent    install the agent + systemd unit on this host (needs root)
#
# Binaries produced in dist/:
#   ipcod-coordinator, ipcod-agent, ipcod-beacon   (standalone, one per role)
#   ipcod                                          (single-file bundle: `ipcod agent|beacon|coordinator`)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Stamp the version into internal/cli.Version, shared by every binary.
LDFLAGS  = -s -w -X github.com/ipcod/ipcod/internal/cli.Version=$(VERSION)

# name:pkg pairs. The three role binaries plus the unified `ipcod` bundle.
TARGETS  = ipcod-coordinator:coordinator ipcod-agent:agent ipcod-beacon:beacon ipcod:ipcod
OUT      = dist

# Targets we ship as static Linux binaries. The agent is what runs on VPSes, so
# we cover both amd64 and arm64 (common VPS arches, incl. Graviton / Oracle ARM).
PLATFORMS = linux/amd64 linux/arm64

.PHONY: all build test vet release clean install-agent

all: build

build:
	@mkdir -p $(OUT)
	@for t in $(TARGETS); do \
		name=$${t%%:*}; pkg=$${t##*:}; \
		echo "  build $$name"; \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/$$name ./cmd/$$pkg; \
	done

test: vet
	go test ./...

vet:
	go vet ./...

# Cross-compile fully static binaries (CGO is off; the sqlite driver is pure
# Go, so no cgo/toolchain is needed on target VPSes).
release: clean
	@mkdir -p $(OUT)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		for t in $(TARGETS); do \
			name=$${t%%:*}; pkg=$${t##*:}; \
			out=$(OUT)/$$name-$$os-$$arch; \
			echo "  release $$out"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/$$pkg; \
		done; \
	done
	@cd $(OUT) && sha256sum *-linux-* > SHA256SUMS && echo "wrote $(OUT)/SHA256SUMS"

clean:
	rm -rf $(OUT)

# Install the agent on the current host. Run from a checked-out repo on the VPS.
# Usage: sudo make install-agent COORD=wss://coord.example.com:8443/ws TOKEN=ipca_xxx
install-agent:
	@test -n "$(COORD)" || (echo "set COORD=wss://host/ws" && exit 1)
	@test -n "$(TOKEN)" || (echo "set TOKEN=ipca_xxx" && exit 1)
	install -m 0755 dist/ipcod-agent /usr/local/bin/ipcod-agent
	install -d -m 0755 /etc/ipcod
	install -m 0644 deploy/agent.env.example /etc/ipcod/agent.env
	sed -i 's|^COORDINATOR=.*|COORDINATOR=$(COORD)|' /etc/ipcod/agent.env
	sed -i 's|^TOKEN=.*|TOKEN=$(TOKEN)|' /etc/ipcod/agent.env
	chmod 0600 /etc/ipcod/agent.env
	install -m 0644 deploy/ipcod-agent.service /etc/systemd/system/ipcod-agent.service
	systemctl daemon-reload
	systemctl enable --now ipcod-agent
	@echo "agent installed; check: journalctl -u ipcod-agent -f"
