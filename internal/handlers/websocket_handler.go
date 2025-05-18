package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cryptovate-India/websocket-service/internal/clients"
	"github.com/Cryptovate-India/websocket-service/internal/config"
	"github.com/gorilla/websocket"
)

// Client represents a connected websocket client
type Client struct {
	conn           *websocket.Conn
	send           chan []byte
	subscriptions  map[string]bool
	productFilters map[string][]string
	mu             sync.RWMutex
	id             string
	connectedAt    time.Time
	lastActivity   time.Time
}

// WebsocketHandler handles websocket connections
type WebsocketHandler struct {
	upgrader         websocket.Upgrader
	clients          map[*Client]bool
	clientsMu        sync.RWMutex
	broadcast        chan []byte
	register         chan *Client
	unregister       chan *Client
	subscriptions    map[string]map[*Client]bool
	subscriptionsMu  sync.RWMutex
	config           *config.Config
	deltaClient      *clients.DeltaWebsocketClient
	ctx              context.Context
	cancel           context.CancelFunc
	messagesSent     int64
	messagesReceived int64
}

// NewWebsocketHandler creates a new websocket handler
func NewWebsocketHandler(ctx context.Context, cfg *config.Config) *WebsocketHandler {
	handlerCtx, cancel := context.WithCancel(ctx)

	// Create the websocket handler
	handler := &WebsocketHandler{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  cfg.Websocket.ReadBufferSize,
			WriteBufferSize: cfg.Websocket.WriteBufferSize,
			CheckOrigin: func(r *http.Request) bool {
				if !cfg.Websocket.CheckOrigin {
					return true
				}
				// Check the origin against the allowed origins
				origin := r.Header.Get("Origin")
				for _, allowedOrigin := range cfg.GetCORSAllowedOrigins() {
					if allowedOrigin == "*" || allowedOrigin == origin {
						return true
					}
				}
				return false
			},
		},
		clients:       make(map[*Client]bool),
		broadcast:     make(chan []byte),
		register:      make(chan *Client),
		unregister:    make(chan *Client),
		subscriptions: make(map[string]map[*Client]bool),
		config:        cfg,
		ctx:           handlerCtx,
		cancel:        cancel,
	}

	fmt.Println("Websocket handler created")
	fmt.Println("Websocket handler config:", cfg)
	fmt.Println("Websocket handler context:", handlerCtx)

	// Create the Delta Exchange client if enabled
	if cfg.Delta.Enabled {
		handler.deltaClient = clients.NewDeltaWebsocketClient(handlerCtx, &cfg.Delta)

		// // Register handlers for Delta Exchange channels
		// for _, channel := range cfg.Delta.Channels {
		// 	handler.registerDeltaHandler(channel)
		// }

		fmt.Println("WS_handler:ctor: Delta Exchange client created")

		// Connect to Delta Exchange
		if err := handler.deltaClient.Connect(); err != nil {
			fmt.Println("Failed to connect to Delta Exchange: ", err)
		}
	}

	// Start the handler
	go handler.run()

	return handler
}

// HandleWebsocket handles a websocket connection
func (h *WebsocketHandler) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	// Upgrade the connection to a websocket
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}

	// Create a new client
	client := &Client{
		conn:           conn,
		send:           make(chan []byte, 256),
		subscriptions:  make(map[string]bool),
		productFilters: make(map[string][]string),
		id:             fmt.Sprintf("%d", time.Now().UnixNano()),
		connectedAt:    time.Now(),
		lastActivity:   time.Now(),
	}

	// Register the client
	h.register <- client

	// Start the read and write pumps
	go h.readPump(client)
	go h.writePump(client)
}

// BroadcastToChannel broadcasts a message to all clients subscribed to a channel
func (h *WebsocketHandler) BroadcastToChannel(channel string, message []byte, productID string) {
	// Get the clients subscribed to the channel
	h.subscriptionsMu.RLock()
	// fmt.Println("WS_Handler: Broadcase: Broadcasting to channel:", channel)
	// fmt.Println("WS_Handler: Broadcast: total subscribers:", len(h.subscriptions))
	// Get the list of clients subscribed to the channel
	clients, ok := h.subscriptions[channel]
	h.subscriptionsMu.RUnlock()
	if !ok {
		return
	}

	// Broadcast the message to all clients subscribed to the channel
	for client := range clients {
		// Check if the client has a product filter for the channel
		client.mu.RLock()
		clientProductIDs, hasFilter := client.productFilters[channel]
		client.mu.RUnlock()

		// fmt.Println("WS_Handler: Broadcast: reading product ids:", clientProductIDs)

		// If the client has a product filter, check if the message matches the filter
		if hasFilter && len(clientProductIDs) > 0 {
			// Check if any of the product IDs match
			fmt.Println("WS_Handler: Broadcast: checking product ids:", productID, "in clientProductIDs:", clientProductIDs)
			match := false
			for _, clientProductID := range clientProductIDs {
				if productID == clientProductID {
					fmt.Println("WS_Handler: Broadcast: found a match for product id:", productID, "in clientProductIDs:", clientProductIDs)
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}

		fmt.Println("WS_Handler: Broadcast: sending message to client:", client.id, "on channel:", channel, "for product:", productID)

		// Send the message to the client
		select {
		case client.send <- message:
		default:
			h.unregister <- client
		}
	}
}

// GetDeltaConnectionStatus gets the connection status of the Delta Exchange client
func (h *WebsocketHandler) GetDeltaConnectionStatus() map[string]interface{} {
	if h.deltaClient != nil {
		return h.deltaClient.GetConnectionStatus()
	}
	return map[string]interface{}{
		"connected": false,
	}
}

// GetStatistics gets statistics about the websocket handler
func (h *WebsocketHandler) GetStatistics() map[string]interface{} {
	// Get the number of active connections
	h.clientsMu.RLock()
	activeConnections := len(h.clients)
	h.clientsMu.RUnlock()

	// Get the number of active subscriptions
	h.subscriptionsMu.RLock()
	activeSubscriptions := 0
	subscriptionsByChannel := make(map[string]int)
	for channel, clients := range h.subscriptions {
		subscriptionsByChannel[channel] = len(clients)
		activeSubscriptions += len(clients)
	}
	h.subscriptionsMu.RUnlock()

	// Get the external sources
	externalSources := make(map[string]bool)
	if h.deltaClient != nil {
		externalSources["delta"] = h.deltaClient.IsConnected()
	}

	// Create the statistics
	stats := map[string]interface{}{
		"active_connections":       activeConnections,
		"active_subscriptions":     activeSubscriptions,
		"messages_sent":            atomic.LoadInt64(&h.messagesSent),
		"messages_received":        atomic.LoadInt64(&h.messagesReceived),
		"subscriptions_by_channel": subscriptionsByChannel,
		"external_sources":         externalSources,
	}

	return stats
}

// Close closes the websocket handler
func (h *WebsocketHandler) Close() {
	h.cancel()
}

// registerDeltaHandler registers a handler for a Delta Exchange channel
func (h *WebsocketHandler) registerDeltaHandler(channel string) {
	h.deltaClient.RegisterHandler(channel, func(message []byte, msgProductID string) {
		// Broadcast the message to all clients subscribed to the channel
		h.BroadcastToChannel(channel, message, msgProductID)
	})
}

// run runs the websocket handler
func (h *WebsocketHandler) run() {
	defer func() {
		// Close all clients
		h.clientsMu.Lock()
		for client := range h.clients {
			client.conn.Close()
		}
		h.clientsMu.Unlock()

		// Close the Delta Exchange client
		if h.deltaClient != nil {
			h.deltaClient.Close()
		}
	}()

	for {
		select {
		case <-h.ctx.Done():
			return
		case client := <-h.register:
			h.clientsMu.Lock()
			h.clients[client] = true
			h.clientsMu.Unlock()
		case client := <-h.unregister:
			h.clientsMu.Lock()
			fmt.Println("WS_Handler: unregistering client")

			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.clientsMu.Unlock()

			// Remove the client from all subscriptions
			h.subscriptionsMu.Lock()
			for channel, clients := range h.subscriptions {
				if _, ok := clients[client]; ok {
					delete(clients, client)
					if len(clients) == 0 {
						delete(h.subscriptions, channel)
					}
				}
			}
			h.subscriptionsMu.Unlock()
		case message := <-h.broadcast:
			// Broadcast the message to all clients
			h.clientsMu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.clientsMu.RUnlock()
		}
	}
}

// readPump reads messages from the client
func (h *WebsocketHandler) readPump(client *Client) {
	defer func() {
		h.unregister <- client
		client.conn.Close()
	}()

	client.conn.SetReadLimit(h.config.Websocket.MaxMessageSize)
	client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.conn.SetPongHandler(func(string) error {
		client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading message: %v", err)
			}
			break
		}

		// Update the last activity time
		client.lastActivity = time.Now()

		// Increment the messages received counter
		atomic.AddInt64(&h.messagesReceived, 1)

		// Parse the message
		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		// Handle the message based on the type
		if msgType, ok := msg["type"].(string); ok {
			switch msgType {
			case "subscribe":
				h.handleSubscribe(client, msg)
			case "unsubscribe":
				h.handleUnsubscribe(client, msg)
			case "ping":
				h.handlePing(client)
			default:
				log.Printf("Unknown message type: %s", msgType)
			}
		}
	}
}

// writePump writes messages to the client
func (h *WebsocketHandler) writePump(client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		client.conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// The hub closed the channel
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Increment the messages sent counter
			atomic.AddInt64(&h.messagesSent, 1)

			// Add queued messages to the current websocket message
			n := len(client.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				msg := <-client.send
				w.Write(msg)
				// Increment the messages sent counter
				atomic.AddInt64(&h.messagesSent, 1)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleSubscribe handles a subscribe message
func (h *WebsocketHandler) handleSubscribe(client *Client, msg map[string]interface{}) {
	fmt.Println("subscribing a message: ", msg)

	var chName string = ""
	// Check if the message has the new format with payload.channels
	if payload, ok := msg["payload"].(map[string]interface{}); ok {
		if channels, ok := payload["channels"].([]interface{}); ok {
			// Process each channel in the payload
			for _, channelObj := range channels {
				if channelMap, ok := channelObj.(map[string]interface{}); ok {
					// Get the channel name
					channelName, ok := channelMap["name"].(string)
					if !ok {
						log.Printf("Channel object does not contain a name")
						continue
					}

					// Get the symbols directly as product IDs
					var productIDs []string
					if symbols, ok := channelMap["symbols"].([]interface{}); ok {
						for _, symbol := range symbols {
							if symbolStr, ok := symbol.(string); ok {
								if symbolStr == "all" {
									// Use all product IDs from config
									productIDs = []string{"all"}
									break
								} else {
									// Add the symbol directly as a product ID
									productIDs = append(productIDs, symbolStr)
								}
							} else if symbolFloat, ok := symbol.(float64); ok {
								// Convert float to string
								productIDs = append(productIDs, fmt.Sprintf("%v", symbolFloat))
							}
						}
					}

					fmt.Println("subscribing to channel: ", channelName)
					fmt.Println("subscribing to product IDs: ", productIDs)
					chName = channelName

					// Check if deltaClient has not registered for this channel then register first.
					if h.deltaClient != nil {
						if channel, ok := msg["type"].(string); ok {
							if channel == "subscribe" {
								h.registerDeltaHandler(channelName)
								fmt.Println("WS_handler: Delta: subscribing to channel: ", channelName)
								h.deltaClient.Subscribe(channelName, productIDs)
							}
						}
					}

					// Subscribe the client to the channel
					h.subscribeClient(client, channelName, productIDs)
				}
			}
		}
	} else {
		log.Printf("Subscribe message does not contain a payload")
		return
	}

	// Send a subscription confirmation
	response := map[string]interface{}{
		"type": "subscribed",
		"payload": map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"name":    chName,
					"symbols": []string{"all"},
				},
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		log.Printf("Error marshaling subscription confirmation: %v", err)
		return
	}

	fmt.Println("sending subscription confirmation: ", string(data))

	client.send <- data
}

// handleUnsubscribe handles an unsubscribe message
func (h *WebsocketHandler) handleUnsubscribe(client *Client, msg map[string]interface{}) {
	// Get the channel from the message
	var chName string = ""
	// Check if the message has the new format with payload.channels
	if payload, ok := msg["payload"].(map[string]interface{}); ok {
		if channels, ok := payload["channels"].([]interface{}); ok {
			// Process each channel in the payload
			for _, channelObj := range channels {
				if channelMap, ok := channelObj.(map[string]interface{}); ok {
					// Get the channel name
					channelName, ok := channelMap["name"].(string)
					if !ok {
						log.Printf("Channel object does not contain a name")
						continue
					}

					fmt.Println("subscribing to channel: ", channelName)
					chName = channelName

					if h.deltaClient != nil {
						if channel, ok := msg["type"].(string); ok {
							if channel == "unsubscribe" {
								fmt.Println("WS_handler: Delta: unsubscribing from channel: ", channelName)
								//check if no other subscriptions exist for this channel
								if clients, ok := h.subscriptions[channelName]; ok {
									if len(clients) == 0 {
										// Unsubscribe the client from the channel
										h.deltaClient.Unsubscribe(channelName)
									} else {
										fmt.Println("WS_handler: Delta: still ", len(clients), " clients subscribed to channel: ", channelName)
									}
								} else {
									// Unsubscribe the client from the channel
									h.deltaClient.Unsubscribe(channelName)
									fmt.Println("WS_handler: Delta: unsubscribed from channel: ", channelName)
								}
							}
						}
					}
					// Subscribe the client to the channel
					h.unsubscribeClient(client, channelName)
				}
			}
		}
	} else {
		log.Printf("Unsubscribe message does not contain a payload")
		return
	}

	// Unsubscribe the client from the channel
	// h.unsubscribeClient(client, channel)

	// Send an unsubscription confirmation
	response := map[string]interface{}{
		"type":    "unsubscribed",
		"channel": chName,
	}
	data, err := json.Marshal(response)
	if err != nil {
		log.Printf("Error marshaling unsubscription confirmation: %v", err)
		return
	}
	client.send <- data
}

// handlePing handles a ping message
func (h *WebsocketHandler) handlePing(client *Client) {
	// Send a pong response
	response := map[string]interface{}{
		"type": "pong",
		"time": time.Now().UnixNano() / int64(time.Millisecond),
	}
	data, err := json.Marshal(response)
	if err != nil {
		log.Printf("Error marshaling pong response: %v", err)
		return
	}
	client.send <- data
}

// subscribeClient subscribes a client to a channel
func (h *WebsocketHandler) subscribeClient(client *Client, channel string, productIDs []string) {
	// Add the subscription to the client
	client.mu.Lock()
	client.subscriptions[channel] = true
	client.productFilters[channel] = productIDs
	client.mu.Unlock()

	// Add the client to the subscription
	h.subscriptionsMu.Lock()
	if _, ok := h.subscriptions[channel]; !ok {
		h.subscriptions[channel] = make(map[*Client]bool)
	}
	h.subscriptions[channel][client] = true
	h.subscriptionsMu.Unlock()
}

// unsubscribeClient unsubscribes a client from a channel
func (h *WebsocketHandler) unsubscribeClient(client *Client, channel string) {
	// Remove the subscription from the client
	client.mu.Lock()
	delete(client.subscriptions, channel)
	delete(client.productFilters, channel)
	client.mu.Unlock()

	// Remove the client from the subscription
	h.subscriptionsMu.Lock()
	if clients, ok := h.subscriptions[channel]; ok {
		delete(clients, client)
		if len(clients) == 0 {
			delete(h.subscriptions, channel)
		}
	}
	h.subscriptionsMu.Unlock()
}
