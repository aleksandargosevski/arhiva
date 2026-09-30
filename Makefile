BINARY    := arhiva
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOPATH    := $(shell go env GOPATH)

.PHONY: build install clean test lint dev

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

install: build
	@mkdir -p $(GOPATH)/bin
	rm -f $(GOPATH)/bin/$(BINARY)
	cp bin/$(BINARY) $(GOPATH)/bin/$(BINARY)
	@echo "Installed $(BINARY) to $(GOPATH)/bin/$(BINARY)"
	@echo "For cd on quit, see the Shell integration section in README.md"

clean:
	rm -rf bin/ dist/

test:
	go test ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

dev: build
	./bin/$(BINARY)
