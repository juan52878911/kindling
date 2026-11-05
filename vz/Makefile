# Atajos de desarrollo. build.sh es quien sabe firmar; el resto es lo que corre
# el CI del núcleo sobre este módulo.
.PHONY: build build-hv test vet fmt check smoke sanitizers fuzz-hv clean

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

# Las pruebas de C de kling-hv con ASan y UBSan (SANITIZE=undefined para solo
# UBSan) y un fuzz corto de sus colas: lo mismo que el CI.
sanitizers:
	./scripts/sanitizers-hv.sh

fuzz-hv:
	go test -run '^$$' -fuzz '^FuzzColas$$' -fuzztime $${FUZZTIME:-30s} ./internal/hvvm/

clean:
	rm -rf bin
