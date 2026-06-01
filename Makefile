.PHONY: build run test clean

build:
	go build -o bin/octo-fleet .

run: build
	./bin/octo-fleet -config configs/fleet.yaml

test:
	go test ./...

clean:
	rm -rf bin/
