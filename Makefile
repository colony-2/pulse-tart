.PHONY: test vet build bundle integration

test:
	go test -race ./...

vet:
	go vet ./...
	for script in scripts/*.sh; do bash -n "$$script"; done

build:
	go build ./...

bundle:
	./scripts/build-bundle.sh

integration:
	./scripts/test-macos.sh
