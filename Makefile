BINARY := tofu-workflows
CMD := ./cmd/tofu-workflows
GO_MK_MODULES := go-build.mk
GO_MK_API_REF := bc41cecbc2b2d643d24e426a1ca104a4a4e3350a
GO_MK_BASE_URL := https://raw.githubusercontent.com/agoodkind/go-makefile/$(GO_MK_API_REF)

# Dedicated qualification workflows run the proof packages with their required services.
GO_TEST_TARGETS := ./cmd/... ./internal/...

include bootstrap.mk

test: build

.DEFAULT_GOAL := help
