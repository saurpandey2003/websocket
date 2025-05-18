package server

import (
	"context"
	"fmt"
	"log"
	"time"

	websocketv1 "github.com/Cryptovate-India/websocket-service/gen/websocket/api/v1"
	"github.com/Cryptovate-India/websocket-service/internal/config"
	"github.com/Cryptovate-India/websocket-service/internal/handlers"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the WebsocketService gRPC service
type Server struct {
	websocketv1.UnimplementedWebsocketServiceServer
	ctx              context.Context
	config           *config.Config
	websocketHandler *handlers.WebsocketHandler
}

// NewServer creates a new WebsocketService server
func NewServer(ctx context.Context, cfg *config.Config, websocketHandler *handlers.WebsocketHandler) *Server {
	return &Server{
		ctx:              ctx,
		config:           cfg,
		websocketHandler: websocketHandler,
	}
}

// Subscribe subscribes to a channel
func (s *Server) Subscribe(ctx context.Context, req *websocketv1.SubscribeRequest) (*websocketv1.SubscribeResponse, error) {
	log.Printf("Subscribe request received for channel %s with product IDs %v", req.Channel, req.ProductIds)

	// Create a subscription ID
	subscriptionID := fmt.Sprintf("%s-%d", req.Channel, time.Now().UnixNano())

	// Return the subscription response
	return &websocketv1.SubscribeResponse{
		SubscriptionId: subscriptionID,
		Channel:        req.Channel,
		ProductIds:     req.ProductIds,
		CreatedAt:      timestamppb.Now(),
	}, nil
}

// Unsubscribe unsubscribes from a channel
func (s *Server) Unsubscribe(ctx context.Context, req *websocketv1.UnsubscribeRequest) (*emptypb.Empty, error) {
	log.Printf("Unsubscribe request received for subscription ID %s", req.SubscriptionId)

	// Return an empty response
	return &emptypb.Empty{}, nil
}

// GetSubscriptionStatus gets the status of subscriptions
func (s *Server) GetSubscriptionStatus(ctx context.Context, req *websocketv1.GetSubscriptionStatusRequest) (*websocketv1.GetSubscriptionStatusResponse, error) {
	log.Printf("GetSubscriptionStatus request received for subscription ID %s and channel %s", req.SubscriptionId, req.Channel)

	// Get the statistics from the websocket handler
	stats := s.websocketHandler.GetStatistics()

	// Create the subscriptions
	subscriptions := make([]*websocketv1.Subscription, 0)
	if subscriptionsByChannel, ok := stats["subscriptions_by_channel"].(map[string]int); ok {
		for channel, count := range subscriptionsByChannel {
			subscription := &websocketv1.Subscription{
				SubscriptionId: fmt.Sprintf("%s-%d", channel, time.Now().UnixNano()),
				Channel:        channel,
				ClientCount:    int32(count),
				CreatedAt:      timestamppb.Now(),
			}
			subscriptions = append(subscriptions, subscription)
		}
	}

	// Return the subscription status response
	return &websocketv1.GetSubscriptionStatusResponse{
		Subscriptions: subscriptions,
	}, nil
}

// GetConnectionStatus gets the status of connections to external sources
func (s *Server) GetConnectionStatus(ctx context.Context, req *emptypb.Empty) (*websocketv1.ConnectionStatusResponse, error) {
	log.Printf("GetConnectionStatus request received")

	// Get the connection status from the websocket handler
	deltaStatus := s.websocketHandler.GetDeltaConnectionStatus()

	// Create the connection status
	connections := make(map[string]*websocketv1.ConnectionStatus)
	if s.config.Delta.Enabled {
		connections["delta"] = &websocketv1.ConnectionStatus{
			Connected: deltaStatus["connected"].(bool),
		}

		// Add the connected at time if available
		if connectedAt, ok := deltaStatus["connected_at"].(time.Time); ok && !connectedAt.IsZero() {
			connections["delta"].ConnectedAt = timestamppb.New(connectedAt)
		}

		// Add the reconnect attempts if available
		if reconnectCount, ok := deltaStatus["reconnect_count"].(int); ok {
			connections["delta"].ReconnectAttempts = int32(reconnectCount)
		}

		// Add the last error if available
		if lastError, ok := deltaStatus["last_error"].(string); ok {
			connections["delta"].LastError = lastError
		}

		// Add the last error time if available
		if lastErrorAt, ok := deltaStatus["last_error_at"].(time.Time); ok && !lastErrorAt.IsZero() {
			connections["delta"].LastErrorAt = timestamppb.New(lastErrorAt)
		}
	}

	// Return the connection status response
	return &websocketv1.ConnectionStatusResponse{
		Connections: connections,
	}, nil
}

// Broadcast broadcasts a message to all clients subscribed to a channel
func (s *Server) Broadcast(ctx context.Context, req *websocketv1.BroadcastRequest) (*emptypb.Empty, error) {
	log.Printf("Broadcast request received for channel %s with %d bytes", req.Channel, len(req.Message))

	// Convert int64 product IDs to strings
	productIDs := make([]string, len(req.ProductIds))
	for i, id := range req.ProductIds {
		productIDs[i] = fmt.Sprintf("%v", id)
	}

	// Broadcast the message to all clients subscribed to the channel
	s.websocketHandler.BroadcastToChannel(req.Channel, req.Message, productIDs[0])

	// Return an empty response
	return &emptypb.Empty{}, nil
}

// GetStatistics gets statistics about the websocket service
func (s *Server) GetStatistics(ctx context.Context, req *emptypb.Empty) (*websocketv1.StatisticsResponse, error) {
	log.Printf("GetStatistics request received")

	// Get the statistics from the websocket handler
	stats := s.websocketHandler.GetStatistics()

	// Create the statistics response
	response := &websocketv1.StatisticsResponse{
		ActiveConnections:   int32(stats["active_connections"].(int)),
		ActiveSubscriptions: int32(stats["active_subscriptions"].(int)),
		MessagesSent:        stats["messages_sent"].(int64),
		MessagesReceived:    stats["messages_received"].(int64),
	}

	// Add the subscriptions by channel if available
	if subscriptionsByChannel, ok := stats["subscriptions_by_channel"].(map[string]int); ok {
		response.SubscriptionsByChannel = make(map[string]int32)
		for channel, count := range subscriptionsByChannel {
			response.SubscriptionsByChannel[channel] = int32(count)
		}
	}

	// Add the external sources if available
	if externalSources, ok := stats["external_sources"].(map[string]bool); ok {
		response.ExternalSources = externalSources
	}

	// Return the statistics response
	return response, nil
}
