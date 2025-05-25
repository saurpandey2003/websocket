package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"github.com/Cryptovate-India/websocket-service/internal/config"
)

// MessageHandler is a function that handles messages from Delta Exchange
type MessageHandler func(message []byte, productId string)

// DeltaWebsocketClient is a client for the Delta Exchange websocket API
type DeltaWebsocketClient struct {
	conn            *websocket.Conn
	url             string
	channels        []string
	productIDs      []string
	handlers        map[string]MessageHandler
	handlersMu      sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	reconnectMax    int
	reconnectCount  int
	reconnectDelay  time.Duration
	connected       bool
	connectedAt     time.Time
	lastError       string
	lastErrorAt     time.Time
	mu              sync.RWMutex
	subscriptions   map[string]bool
	subscriptionsMu sync.RWMutex
	totalMessages   int
	logger          *zap.Logger
}

// NewDeltaWebsocketClient creates a new Delta Exchange websocket client
func NewDeltaWebsocketClient(ctx context.Context, cfg *config.Delta) *DeltaWebsocketClient {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	clientCtx, cancel := context.WithCancel(ctx)

	client := &DeltaWebsocketClient{
		url:            cfg.URL,
		channels:       cfg.Channels,
		productIDs:     cfg.ProductIDs,
		handlers:       make(map[string]MessageHandler),
		ctx:            clientCtx,
		cancel:         cancel,
		reconnectMax:   cfg.ReconnectMax,
		reconnectDelay: 5 * time.Second,
		subscriptions:  make(map[string]bool),
		logger:         logger,
	}

	return client
}

// Connect connects to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) Connect() error {
	c.logger.Info("Attempting to connect to Delta Exchange", zap.String("url", c.url))

	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		c.mu.Lock()
		c.lastError = fmt.Sprintf("Failed to connect to Delta Exchange: %v", err)
		c.lastErrorAt = time.Now()
		c.mu.Unlock()
		c.logger.Error("Failed to connect to Delta Exchange", zap.Error(err))
		return fmt.Errorf("failed to connect to Delta Exchange: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.connectedAt = time.Now()
	c.reconnectCount = 0
	c.mu.Unlock()

	c.logger.Info("Connected to Delta Exchange",
		zap.String("url", c.url),
		zap.Time("connected_at", c.connectedAt))

	// Subscribe to configured channels
	for _, channel := range c.channels {
		c.logger.Info("Subscribing to channel", zap.String("channel", channel))
		if err := c.Subscribe(channel, c.productIDs); err != nil {
			c.logger.Error("Failed to subscribe to channel",
				zap.String("channel", channel),
				zap.Error(err))
		}
	}

	go c.readPump()
	return nil
}

// RegisterHandler registers a handler for a channel
func (c *DeltaWebsocketClient) RegisterHandler(channel string, handler MessageHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[channel] = handler
	c.logger.Info("Registered handler for channel", zap.String("channel", channel))
}

// readPump reads messages from the websocket
func (c *DeltaWebsocketClient) readPump() {
	var conn *websocket.Conn
	c.mu.RLock()
	conn = c.conn
	c.mu.RUnlock()

	if conn == nil {
		c.logger.Error("Connection is nil, cannot start read pump")
		return
	}

	defer func() {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()

		if conn != nil {
			conn.Close()
		}

		select {
		case <-c.ctx.Done():
			c.logger.Info("Context canceled, stopping read pump")
			return
		default:
			c.reconnect()
		}
	}()

	c.logger.Info("Starting read pump", zap.String("url", c.url))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.lastError = fmt.Sprintf("Error reading from Delta Exchange: %v", err)
			c.lastErrorAt = time.Now()
			c.mu.Unlock()
			c.logger.Error("Error reading message",
				zap.Error(err),
				zap.Time("timestamp", time.Now()))
			return
		}

		// Log raw message for debugging
		c.logger.Info("Raw message received from Delta",
			zap.ByteString("message", message))

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			c.logger.Error("Error parsing message",
				zap.Error(err),
				zap.ByteString("message", message))
			continue
		}

		var channel string
		var msgProductID string
		// Try to get channel from payload.channels (for subscription confirmations)
		if payload, ok := msg["payload"].(map[string]interface{}); ok {
			if channels, ok := payload["channels"].([]interface{}); ok && len(channels) > 0 {
				if channelMap, ok := channels[0].(map[string]interface{}); ok {
					if name, ok := channelMap["name"].(string); ok {
					channel = name
					}
				}
			}
		}
		// Fallback to type field for data messages
		if channel == "" {
			if msgType, ok := msg["type"].(string); ok {
				channel = msgType
			} else {
				c.logger.Warn("Message does not contain a valid 'type' or 'payload.channels' field",
					zap.Any("message", msg))
				continue
			}
		}
		// Get product ID (handle both 'symbol' and 's')
		if productId, ok := msg["symbol"].(string); ok {
			msgProductID = productId
		} else if s, ok := msg["s"].(string); ok {
			msgProductID = s
		} else {
			c.logger.Warn("Message does not contain a valid 'symbol' or 's' field",
				zap.Any("message", msg))
		}

		c.handlersMu.RLock()
		c.totalMessages++
		c.logger.Info("Received message",
			zap.String("channel", channel),
			zap.String("product_id", msgProductID),
			zap.Int("total_messages", c.totalMessages))
		handler, ok := c.handlers[channel]
		c.handlersMu.RUnlock()
		if ok {
			handler(message, msgProductID)
		}
	}
}

// Subscribe subscribes to a channel
func (c *DeltaWebsocketClient) Subscribe(channel string, productIDs []string) error {
	symbols := []string{"all"}
	if len(productIDs) > 0 {
		symbols = productIDs
	}

	msg := map[string]interface{}{
		"type": "subscribe",
		"payload": map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"name":    channel,
					"symbols": symbols,
				},
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("Failed to marshal subscription message",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("failed to marshal subscription message: %w", err)
	}

	var conn *websocket.Conn
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		c.logger.Warn("Not connected to Delta Exchange", zap.String("channel", channel))
		return fmt.Errorf("not connected to Delta Exchange")
	}
	conn = c.conn
	c.mu.RUnlock()

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.logger.Error("Failed to send subscription message",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("failed to send subscription message: %w", err)
	}

	c.subscriptionsMu.Lock()
	c.subscriptions[channel] = true
	c.subscriptionsMu.Unlock()

	c.logger.Info("Subscribed to channel",
		zap.String("channel", channel),
		zap.Strings("symbols", symbols))
	return nil
}

// Unsubscribe unsubscribes from a channel
func (c *DeltaWebsocketClient) Unsubscribe(channel string) error {
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		c.logger.Warn("Not connected to Delta Exchange", zap.String("channel", channel))
		return fmt.Errorf("not connected to Delta Exchange")
	}
	conn := c.conn
	c.mu.RUnlock()

	msg := map[string]interface{}{
		"type": "unsubscribe",
		"payload": map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"name": channel,
				},
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("Failed to marshal unsubscription message",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("failed to marshal unsubscription message: %w", err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.logger.Error("Failed to send unsubscription message",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("failed to send unsubscription message: %w", err)
	}

	c.subscriptionsMu.Lock()
	delete(c.subscriptions, channel)
	c.subscriptionsMu.Unlock()

	c.logger.Info("Unsubscribed from channel", zap.String("channel", channel))
	return nil
}

// reconnect attempts to reconnect to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) reconnect() {
	c.mu.Lock()
	c.reconnectCount++
	reconnectCount := c.reconnectCount
	reconnectMax := c.reconnectMax
	c.mu.Unlock()

	if reconnectCount > reconnectMax {
		c.logger.Error("Exceeded maximum reconnection attempts",
			zap.Int("reconnect_count", reconnectCount),
			zap.Int("reconnect_max", reconnectMax))
		return
	}

	c.logger.Info("Reconnecting to Delta Exchange",
		zap.Int("attempt", reconnectCount),
		zap.Int("max_attempts", reconnectMax))

	time.Sleep(c.reconnectDelay)

	if err := c.Connect(); err != nil {
		c.logger.Error("Failed to reconnect to Delta Exchange", zap.Error(err))
	}
}

// Close closes the connection to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) Close() error {
	c.cancel()

	var conn *websocket.Conn
	c.mu.Lock()
	conn = c.conn
	c.connected = false
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		err := conn.Close()
		if err != nil {
			c.logger.Error("Failed to close connection", zap.Error(err))
		}
		return err
	}

	c.logger.Info("Closed Delta Exchange connection")
	return nil
}

// IsConnected returns whether the client is connected to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// GetConnectionStatus returns the connection status of the Delta Exchange websocket client
func (c *DeltaWebsocketClient) GetConnectionStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := map[string]interface{}{
		"connected":           c.connected,
		"connected_at":        c.connectedAt,
		"reconnect_count":     c.reconnectCount,
		"reconnect_max":       c.reconnectMax,
		"last_error":          c.lastError,
		"last_error_at":       c.lastErrorAt,
		"subscribed_channels": c.getSubscribedChannels(),
	}

	return status
}

// getSubscribedChannels returns the channels that the client is subscribed to
func (c *DeltaWebsocketClient) getSubscribedChannels() []string {
	c.subscriptionsMu.RLock()
	defer c.subscriptionsMu.RUnlock()

	channels := make([]string, 0, len(c.subscriptions))
	for channel := range c.subscriptions {
		channels = append(channels, channel)
	}

	c.logger.Info("Retrieved subscribed channels", zap.Strings("channels", channels))
	return channels
}