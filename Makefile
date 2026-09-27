BINARY := llama-slot-proxy
GO     ?= go

.PHONY: all build vet fmt test clean

all: build

build:
	$(GO) build -trimpath -ldflags "-s -w" -o $(BINARY) .

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

test:
	$(GO) test ./...

clean:
	rm -f $(BINARY)
