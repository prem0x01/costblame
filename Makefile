BINARY     := costblame
MODULE     := github.com/prem0x01/costblame
BUILD_DIR  := ./bin
CMD        := ./cmd/costblame
LDFLAGS    := -s -w

.PHONY: all build test lint tidy docker-build docker-up clean

all: tidy build

build:
	CGO_ENABLED=1 go build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD)

run: build
	$(BUILD_DIR)/$(BINARY) serve

report: build
	$(BUILD_DIR)/$(BINARY) report

tui: build
	$(BUILD_DIR)/$(BINARY) tui

test:
	go test ./... -race -timeout 60s

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

docker-build:
	docker build -t costblame:latest .

docker-up:
	docker compose -f deploy/docker-compose.yml up --build

docker-down:
	docker compose -f deploy/docker-compose.yml down

clean:
	rm -rf $(BUILD_DIR)

# Generate a local .env file for docker-compose from the checked-in template
env-template:
	@cp -n .env.example .env && echo ".env written — fill in the adapters you use" || echo ".env already exists"
