# Build settings for the CyberKiller agent.
# Override on the command line:
#   make CK_API=https://your-range.example/api build

CK_API      ?= https://cyberkiller.net/api
CK_LAN      ?=
OUT         ?= cyberkiller-agent
LDFLAGS     := -s -w \
               -X main.defaultAPI=$(CK_API) \
               -X main.lanFallback=$(CK_LAN)
BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build verify clean

build:
	go build $(BUILD_FLAGS) -o $(OUT) .
	@echo
	@echo "  ✓ Built $(OUT) (API: $(CK_API))"
	@echo "  → sha256: $$(sha256sum $(OUT) | cut -d' ' -f1)"

# Print SHA256 of an existing binary so you can compare to a release.
verify:
	@sha256sum $(OUT)

clean:
	rm -f $(OUT) $(OUT).update
