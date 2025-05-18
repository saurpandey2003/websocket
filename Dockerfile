# Build stage
FROM golang:1.21-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o websocket-service .

# Final stage
FROM alpine:latest

# Set working directory
WORKDIR /app

# Install CA certificates
RUN apk --no-cache add ca-certificates

# Copy the binary from the builder stage
COPY --from=builder /app/websocket-service .

# Copy the configuration files
COPY --from=builder /app/config ./config

# Expose the HTTP and gRPC ports
EXPOSE 8080 9090

# Set the entry point
ENTRYPOINT ["./websocket-service"]
