# Stage 1: Build the single static Go binary
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /dreeve-social main.go

# Stage 2: Minimal Alpine runtime (~18 MB total)
FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /dreeve-social /usr/local/bin/dreeve-social

VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dreeve-social"]
