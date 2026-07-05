.PHONY: build test test-cov lint check ci clean help

BINARY := bin/agy-run
COVERAGE := coverage.out

build:
	@mkdir -p bin
	go build -o $(BINARY) .

test:
	go test ./...

test-cov:
	go test -coverprofile=$(COVERAGE) ./...
	go tool cover -func=$(COVERAGE)

lint:
	go vet ./...
	golangci-lint run

check: lint test

ci: lint test-cov build

clean:
	rm -rf bin/ $(COVERAGE)

help:
	@echo "Targets: build / test / test-cov / lint / check / ci / clean"
