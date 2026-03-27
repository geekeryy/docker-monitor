.PHONY: build-image

IMAGE ?= docker.io/geekeryy/docker-monitor:latest
PLATFORMS ?= linux/amd64,linux/arm64
GO_IMAGE ?= docker.io/library/golang:1.25.5-alpine
ALPINE_IMAGE ?= docker.io/library/alpine:3.22

# docker buildx create --name container-builder --driver docker-container --bootstrap --use
build-image:
	docker buildx build --push --platform $(PLATFORMS) --build-arg GO_IMAGE=$(GO_IMAGE) --build-arg ALPINE_IMAGE=$(ALPINE_IMAGE) -t $(IMAGE) -f Dockerfile .
	docker pull $(IMAGE)