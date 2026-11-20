VERSION ?= 0.2.0
IMAGE   ?= ghcr.io/scheff-of-staff/scheff-writer

.PHONY: build test vet check run image ship clean

build:
	go build -trimpath -o bin/scheff-writer ./cmd/scheff-writer

test:
	go test ./...

vet:
	go vet ./...

check: vet test

# Local run against a folder of Markdown projects, no sign-in, plain
# http on localhost only. Example: make run ROOT=~/writing
ROOT ?= ./example
run: build
	mkdir -p $(ROOT)/04_Projects
	WRITER_ROOT=$(ROOT) WRITER_ALLOW_NO_AUTH=1 WRITER_LISTEN=127.0.0.1:8445 ./bin/scheff-writer

# Local single-arch image.
image:
	docker build -t $(IMAGE):$(VERSION) .

# Registry-free deploy to a Scheff of Staff host: build amd64 and
# stream the image over SSH into the host's Docker. Override HOST.
HOST ?= scheff-agent
ship:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(VERSION) --load .
	docker save $(IMAGE):$(VERSION) | gzip | ssh $(HOST) 'gunzip | docker load'

clean:
	rm -rf bin
