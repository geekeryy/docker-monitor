.PHONY: build-image
# docker buildx create --name container-builder --driver docker-container --bootstrap --use
build-image:
	docker buildx build --push --platform linux/amd64,linux/arm64 -t docker.io/geekeryy/docker-monitor:latest -f Dockerfile .
	docker pull docker.io/geekeryy/docker-monitor:latest