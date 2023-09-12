all: mediacache

mediacache: cmd/mediacache/*.go
	go build -o bin/mediacache cmd/mediacache/*.go

.PHONY: clean
clean:
	rm -rf bin

.PHONY: install
install:
	go install ./cmd/mediacache

.PHONY: precommit
precommit:
	go fmt ./...
	go vet ./...
