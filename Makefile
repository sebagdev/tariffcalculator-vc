APP := tariff-comparator
IMAGE ?= tariffcalculator-vc:local
PORT ?= 8080

.PHONY: test run build docker-build docker-run compose-up compose-down

test:
	go test ./...

run:
	go run . --addr 127.0.0.1:$(PORT)

build:
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/$(APP) .

docker-build:
	docker build -t $(IMAGE) .

docker-run:
	docker run --rm -p $(PORT):8080 -v "$$(pwd)/../docs:/app/docs:ro" $(IMAGE)

compose-up:
	docker compose up --build

compose-down:
	docker compose down
