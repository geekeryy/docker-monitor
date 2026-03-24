# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.25.5-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG TARGETOS
ARG TARGETARCH

ENV CGO_ENABLED=0

RUN GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor

FROM alpine:3.22

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata openssh-client

COPY --from=builder /out/monitor /usr/local/bin/monitor
COPY configs/config.yaml /app/configs/config.yaml

RUN mkdir -p /app/data

ENTRYPOINT ["monitor"]
CMD ["-f", "/app/configs/config.yaml"]