.PHONY: build test vet clean

build:
	go build -o bin/xjobs ./cmd/xjobs

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
