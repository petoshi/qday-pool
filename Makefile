QDAY_GO := $(if $(wildcard ../qday/.tools/go/bin/go),../qday/.tools/go/bin/go,go)
VERSION ?= dev

.PHONY: build test race vet dist clean

build:
	mkdir -p build
	$(QDAY_GO) build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o build/qday-pool ./cmd/qday-pool

test:
	$(QDAY_GO) test ./...

race:
	$(QDAY_GO) test -race ./...

vet:
	$(QDAY_GO) vet ./...

dist:
	python3 scripts/package.py --go $(QDAY_GO) --version $(VERSION)

clean:
	python3 -c 'from pathlib import Path; import shutil; shutil.rmtree(Path("build"), ignore_errors=True)'
