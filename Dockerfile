# Stage 1: Cross-compile natively on the GitHub runner's CPU
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /dreeve-social main.go

# Stage 2: Minimal Alpine runtime (~18 MB total)
FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /dreeve-social /usr/local/bin/dreeve-social

VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dreeve-social"]
