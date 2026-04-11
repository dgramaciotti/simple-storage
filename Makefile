# Variables
BINARY_NAME=storage-service
DOCKER_IMAGE=simple-storage-app
STORAGE_DATA=./storage-data

.PHONY: all build test test-integration docker-up docker-down clean fmt lint

all: build test

build:
	go build -o $(BINARY_NAME) main.go

fmt:
	go fmt ./...

lint:
	golangci-lint run

test:
	go test -v -race ./...

test-integration: docker-down
	@echo "Starting integration tests..."
	@rm -rf $(STORAGE_DATA)
	@mkdir -p $(STORAGE_DATA)
	docker compose build --no-cache
	docker compose up -d
	@echo "Waiting for app to be ready..."
	@count=0; \
	until curl -s http://localhost:8080/health > /dev/null; do \
		if [ $$count -ge 30 ]; then echo "App failed to start"; exit 1; fi; \
		sleep 1; \
		count=$$((count+1)); \
	done
	@go test -v -count=1 -tags integration ./tests/integration/...; \
	RET=$$?; \
	$(MAKE) docker-down; \
	exit $$RET

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down --remove-orphans

clean:
	rm -f $(BINARY_NAME)
	rm -rf $(STORAGE_DATA)
