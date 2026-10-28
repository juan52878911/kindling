# Atajos de desarrollo. build.sh es quien sabe firmar; el resto es lo que corre
# el CI del núcleo sobre este módulo.
.PHONY: build build-hv test vet fmt check smoke clean

build:
	./build.sh

# kling-hv: el VMM sobre Hypervisor.framework (docs/backend-hv.md del núcleo).
build-hv:
	./build-hv.sh

test:
	go test -race ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt: format the files above" && exit 1)

check: fmt vet test

smoke: build
	./scripts/smoke.sh

clean:
	rm -rf bin
