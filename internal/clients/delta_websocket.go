package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Cryptovate-India/websocket-service/internal/config"
	"github.com/gorilla/websocket"
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
}

// NewDeltaWebsocketClient creates a new Delta Exchange websocket client
func NewDeltaWebsocketClient(ctx context.Context, cfg *config.Delta) *DeltaWebsocketClient {
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
	}

	return client
}

// Connect connects to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) Connect() error {
	fmt.Println("Delta_WS: Connect: Attempting to connect to Delta Exchange...")

	// First check if already connected (with lock)
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	fmt.Println("Delta_WS: Connect: connecting url:", c.url)
	// Connect to the websocket (without holding the lock)
	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		c.mu.Lock()
		c.lastError = fmt.Sprintf("Failed to connect to Delta Exchange: %v", err)
		c.lastErrorAt = time.Now()
		c.mu.Unlock()
		return fmt.Errorf("failed to connect to Delta Exchange: %w", err)
	}

	// Update connection state (with lock)
	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.connectedAt = time.Now()
	c.reconnectCount = 0
	c.mu.Unlock()

	// Start the read pump
	go c.readPump()

	// // Subscribe to channels (without holding the lock)
	// for _, channel := range c.channels {
	// 	fmt.Println("Delta_WS: Connect: Subscribing to channel:", channel)
	// 	if err := c.Subscribe(channel, c.productIDs); err != nil {
	// 		log.Printf("Failed to subscribe to channel %s: %v", channel, err)
	// 	}
	// }

	return nil
}

// RegisterHandler registers a handler for a channel
func (c *DeltaWebsocketClient) RegisterHandler(channel string, handler MessageHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[channel] = handler
}

// readPump reads messages from the websocket
func (c *DeltaWebsocketClient) readPump() {
	// Store a local reference to the connection to avoid race conditions
	var conn *websocket.Conn
	c.mu.RLock()
	conn = c.conn
	c.mu.RUnlock()

	if conn == nil {
		log.Println("Delta_WS: readPump: Connection is nil, cannot start read pump")
		return
	}

	defer func() {
		// Mark as disconnected
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()

		// Close the connection without holding the lock
		if conn != nil {
			conn.Close()
		}

		// Attempt to reconnect if the context is not canceled
		select {
		case <-c.ctx.Done():
			return
		default:
			c.reconnect()
		}
	}()

	for {
		_, message, err := conn.ReadMessage()
		// fmt.Println("Delta_WS: readPump: Read message:", string(message))
		if err != nil {
			// Update error state with lock
			c.mu.Lock()
			c.lastError = fmt.Sprintf("Error reading from Delta Exchange: %v", err)
			c.lastErrorAt = time.Now()
			c.mu.Unlock()
			return
		}

		// Parse the message to get the channel
		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Error parsing message from Delta Exchange: %v", err)
			continue
		}

		// Check for the new message format with payload.channels
		var channel string
		var msgProductID string
		// if payload, ok := msg["payload"].(map[string]interface{}); ok {
		// 	if channels, ok := payload["channels"].([]interface{}); ok && len(channels) > 0 {
		// 		if channelMap, ok := channels[0].(map[string]interface{}); ok {
		// 			if name, ok := channelMap["name"].(string); ok {
		// 				channel = name
		// 			}
		// 		}
		// 	}
		// }
		if msgType, ok := msg["type"].(string); ok {
			channel = msgType
		} else {
			log.Printf("Delta_WS: readPump: msg does not contain a valid 'type' field: %v", msg)
		}
		if productId, ok := msg["symbol"].(string); ok {
			msgProductID = productId
		} else {
			log.Printf("Delta_WS: readPump: msg does not contain a valid 'type' field: %v", msg)
		}

		// Call the handler for the channel
		c.handlersMu.RLock()
		c.totalMessages++
		fmt.Println("Delta_WS: readPump: Channel:", channel, "product:", msgProductID, "Message count:", c.totalMessages)
		// for ch := range c.handlers {
		// 	fmt.Printf("Delta_WS: readPump: Registered handler for channel: %s\n", ch)
		// }

		// Every message has type, and based on that type (Channel name) we can call the handler.
		handler, ok := c.handlers[channel]
		c.handlersMu.RUnlock()
		if ok {
			handler(message, msgProductID)
		}
	}
}

// subscribe subscribes to a channel
func (c *DeltaWebsocketClient) Subscribe(channel string, productIDs []string) error {
	// Use the product IDs as symbols
	symbols := []string{"all"}
	if len(productIDs) > 0 {
		symbols = productIDs
	}

	// Create the subscription message using the new format
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

	// Marshal the message
	data, err := json.Marshal(msg)
	if err != nil {
		fmt.Println("Delta_WS: Subscribe: Error marshalling subscription message:", err)
		return fmt.Errorf("failed to marshal subscription message: %w", err)
	}

	// Check connection status and get conn (with lock)
	var conn *websocket.Conn
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return fmt.Errorf("not connected to Delta Exchange")
	}
	conn = c.conn
	c.mu.RUnlock()

	// Send the message (without holding the lock)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to send subscription message: %w", err)
	}

	// Add the subscription
	c.subscriptionsMu.Lock()
	c.subscriptions[channel] = true
	c.subscriptionsMu.Unlock()

	return nil
}

// unsubscribe unsubscribes from a channel
func (c *DeltaWebsocketClient) Unsubscribe(channel string) error {
	// Check connection status and get conn (with lock)
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return fmt.Errorf("not connected to Delta Exchange")
	}
	conn := c.conn
	c.mu.RUnlock()
	// Create the unsubscription message
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
	// Marshal the message
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal unsubscription message: %w", err)
	}
	// Send the message (without holding the lock)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to send unsubscription message: %w", err)
	}
	// Remove the subscription
	c.subscriptionsMu.Lock()
	delete(c.subscriptions, channel)
	c.subscriptionsMu.Unlock()
	return nil
}

// reconnect attempts to reconnect to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) reconnect() {
	// Get the reconnect count with lock
	c.mu.Lock()
	c.reconnectCount++
	reconnectCount := c.reconnectCount
	reconnectMax := c.reconnectMax
	c.mu.Unlock()

	if reconnectCount > reconnectMax {
		log.Printf("Exceeded maximum reconnection attempts (%d)", reconnectMax)
		return
	}

	log.Printf("Reconnecting to Delta Exchange (attempt %d/%d)...", reconnectCount, reconnectMax)

	// Sleep without holding any locks
	time.Sleep(c.reconnectDelay)

	// Connect without holding any locks
	if err := c.Connect(); err != nil {
		log.Printf("Failed to reconnect to Delta Exchange: %v", err)
	}
}

// Close closes the connection to the Delta Exchange websocket API
func (c *DeltaWebsocketClient) Close() error {
	// Cancel the context first to signal all goroutines to stop
	c.cancel()

	// Get the connection with lock
	var conn *websocket.Conn
	c.mu.Lock()
	conn = c.conn
	c.connected = false
	c.conn = nil // Clear the connection reference
	c.mu.Unlock()

	// Close the connection without holding the lock
	if conn != nil {
		return conn.Close()
	}

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

	fmt.Println("Delta_WS: getSubscribedChannels: Subscribed channels:", channels)
	return channels
}
