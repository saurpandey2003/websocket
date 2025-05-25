# Use Go 1.22 image to ensure support for slices and grpc
FROM golang:1.22 AS build

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker layer caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go app
RUN go build -o websocket-service main.go

# (Optional) Multi-stage build for smaller image:
# FROM debian:bookworm-slim
# WORKDIR /app
# COPY --from=build /app/websocket-service .
# CMD ["./websocket-service"]
