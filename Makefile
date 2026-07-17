.PHONY: build run migrate test vet fmt clean

build:
	go build -o bin/lynai-backend ./cmd/server

run:
	go run ./cmd/server

migrate:
	go run ./cmd/server migrate

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

clean:
	rm -rf bin/
