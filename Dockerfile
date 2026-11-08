## Stage 1: build the Go binary
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY . .
RUN go mod download
RUN CGO_ENABLED=0 go build -o /check_lookup check_lookup.go

## Stage 2: minimal runtime image
FROM alpine:latest

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /check_lookup /app/check_lookup

ENTRYPOINT ["/app/check_lookup"]
