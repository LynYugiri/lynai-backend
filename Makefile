.PHONY: build run test vet fmt clean

build:
	go build -o bin/lynai-backend ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

clean:
	rm -rf bin/
