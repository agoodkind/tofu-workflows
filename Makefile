BINARY := tofu-workflows
CMD := ./cmd/tofu-workflows
GO_MK_MODULES := go-build.mk

# Dedicated qualification workflows run the proof packages with their required services.
GO_TEST_TARGETS := ./cmd/... ./internal/...

include bootstrap.mk

test: build

.DEFAULT_GOAL := help
