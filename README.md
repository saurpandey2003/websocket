# Challenge

Following is the information on the working websocket service. This service is reading data from a stream and publishing it to its clients by managing channels and massaging the data. Also you can subscribe different channels on this websocket and get the access of ticker data on that. You can find more information on this by visiting [here](https://docs.delta.exchange/#public-channels-2). Following are the challanges that needs to be solved.

1. **Bug Identification and Resolution**: Identify and fix multiple bugs present in the repository.

2. **Telemetry Implementation**: Build appropriate telemetry around the messages to facilitate debugging of message flow.

3. **Deployment**: Deploy the service to a publicly accessible location.

Please create a remote branch and submit it back to us and we will get in touch with you as soon as possible.

# Websocket Service

The Websocket Service is a microservice that provides real-time data streaming capabilities for the Cryptovate platform. It connects to external data sources like Delta Exchange via websockets and provides a public-facing websocket API for clients to consume real-time data.

## Architecture

The Websocket Service follows a layered architecture:

1. **External Websocket Clients**: Connect to external data sources like Delta Exchange to receive real-time data.
2. **Internal Handlers**: Process and manage websocket connections, subscriptions, and message broadcasting.
3. **gRPC Server**: Provides a gRPC API for internal services to interact with the websocket service.
4. **HTTP Server**: Provides a public-facing websocket endpoint for clients to connect to.

## Features

- Real-time data streaming from external sources
- Client subscription management
- Channel-based message broadcasting
- Product ID filtering for subscriptions
- Connection status monitoring
- Statistics and metrics collection
- Automatic reconnection to external sources

## API

### Websocket API

Clients can connect to the websocket endpoint at `/ws` and interact with the service using JSON messages:

#### Subscribe to a channel

```json
{
  "type": "subscribe",
  "channel": "v2/ticker",
  "product_ids": [27, 28, 29]
}
```

#### Unsubscribe from a channel

```json
{
  "type": "unsubscribe",
  "channel": "v2/ticker"
}
```

#### Ping

```json
{
  "type": "ping"
}
```

### gRPC API

The service also provides a gRPC API for internal services to interact with the websocket service. See the [proto definition](./protos/websocket/v1/api.proto) for details.

## Configuration

The service can be configured using YAML configuration files in the `config` directory:

- `local.yaml`: Local development configuration
- `development.yaml`: Development environment configuration
- `production.yaml`: Production environment configuration

## Development

### Prerequisites

- Go 1.21 or later
- Protocol Buffers compiler (protoc)
- Go plugins for Protocol Buffers
- Access to the `github.com/Cryptovate-India/server-utils` package

### Setup

1. Clone the repository
2. Ensure the `server-utils` package is available at `../../pkg/server-utils` (the go.mod file includes a replace directive for this dependency)
3. Install dependencies: `make deps`
4. Generate Protocol Buffers code: `make proto`
5. Build the service: `make build`
6. Run the service: `make run`

### Testing

Run the tests with:

```bash
make test
```

## Deployment

The service can be deployed using Docker:

```bash
make docker-build
make docker-run
```

Or using Kubernetes with the provided Helm charts.

## Integration with API Gateway

The Websocket Service is designed to be integrated with the API Gateway, which provides a public-facing endpoint for clients to connect to. The API Gateway routes websocket connections to the Websocket Service based on the request path.

## Best Practices

1. **Security**: Always use TLS for production deployments and implement proper authentication and authorization.
2. **Scalability**: Deploy multiple instances of the service behind a load balancer for high availability and scalability.
3. **Monitoring**: Use the provided metrics endpoint to monitor the service's health and performance.
4. **Rate Limiting**: Configure rate limiting to prevent abuse and ensure fair usage of resources.
5. **Error Handling**: Implement proper error handling and logging to diagnose issues in production.
