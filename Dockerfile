# syntax=docker/dockerfile:1

# Go cross-compiles natively on the build machine, so an amd64 image builds
# fast on an arm64 laptop too: docker build --platform linux/amd64 .
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY contract ./contract
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 bot
WORKDIR /app
COPY --from=build /out/bot ./bot
COPY data ./data
# Root CA of the MAX API certificate. It is trusted only by the MAX client,
# not added to the system store.
COPY certs/russian_trusted_root_ca.pem ./certs/
ENV MAX_CA_FILE=/app/certs/russian_trusted_root_ca.pem \
    LOG_FORMAT=json \
    TZ=Europe/Moscow
USER 10001
ENTRYPOINT ["/app/bot"]
