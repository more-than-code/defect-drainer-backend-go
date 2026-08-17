BIN := bin/defect-drainer
.PHONY: build test vet cross-linux

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --always --dirty)" -o $(BIN) ./cmd/defect-drainer

test:
	go test ./...

vet:
	go vet ./...

cross-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/defect-drainer-linux-amd64 ./cmd/defect-drainer
