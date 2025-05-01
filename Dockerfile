FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
RUN go mod verify

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /radicale-controller .

FROM alpine:latest

RUN apk --no-cache add ca-certificates

COPY --from=builder /radicale-controller /usr/local/bin/radicale-controller

ENV POLL_INTERVAL="5m"
ENV RIGHTS_FILE_PATH="/data/rights"
ENV RADICALE_STORAGE_PATH="/data/collections/collection-root"
ENV HTTP_METHOD="GET"
ENV AUTH_TYPE="none"
ENV CONFIG_PATH="radicale"

RUN addgroup -S radicalecontroller && adduser -S radicalecontroller -G radicalecontroller
RUN mkdir -p /data

USER radicalecontroller

WORKDIR /data

ENTRYPOINT ["/usr/local/bin/radicale-controller"]
