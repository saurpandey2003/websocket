# Build stage
FROM golang:1.20-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o websocket-service main.go

# Final image
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/websocket-service .
CMD ["./websocket-service"]
