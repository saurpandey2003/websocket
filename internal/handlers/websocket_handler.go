package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cryptovate-India/websocket-service/internal/clients"
	"github.com/Cryptovate-India/websocket-service/internal/config"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
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
	upgrader             websocket.Upgrader
	clients              map[*Client]bool
	clientsMu            sync.RWMutex
	broadcast            chan []byte
	register             chan *Client
	unregister           chan *Client
	subscriptions        map[string]map[*Client]bool
	subscriptionsMu      sync.RWMutex
	config               *config.Config
	deltaClient          *clients.DeltaWebsocketClient
	ctx                  context.Context
	cancel               context.CancelFunc
	messagesSent         int64
	messagesReceived     int64
	messagesSentTotal    *prometheus.CounterVec
	messagesReceivedTotal *prometheus.CounterVec
	activeConnections     *prometheus.GaugeVec
	activeSubscriptions   *prometheus.GaugeVec
	errorsTotal           *prometheus.CounterVec
	logger               *zap.Logger
}

// NewWebsocketHandler creates a new websocket handler
func NewWebsocketHandler(ctx context.Context, cfg *config.Config) *WebsocketHandler {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	handlerCtx, cancel := context.WithCancel(ctx)

	handler := &WebsocketHandler{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  cfg.Websocket.ReadBufferSize,
			WriteBufferSize: cfg.Websocket.WriteBufferSize,
			CheckOrigin: func(r *http.Request) bool {
				if !cfg.Websocket.CheckOrigin {
					return true
				}
				origin := r.Header.Get("Origin")
				for _, allowedOrigin := range cfg.GetCORSAllowedOrigins() {
					if allowedOrigin == "*" || allowedOrigin == origin {
						return true
					}
				}
				return false
			},
		},
		clients:              make(map[*Client]bool),
		broadcast:            make(chan []byte),
		register:             make(chan *Client),
		unregister:           make(chan *Client),
		subscriptions:        make(map[string]map[*Client]bool),
		config:               cfg,
		ctx:                  handlerCtx,
		cancel:               cancel,
		logger:               logger,
		messagesSentTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "websocket_messages_sent_total",
				Help: "Total number of messages sent to clients",
			},
			[]string{"channel", "product_id"},
		),
		messagesReceivedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "websocket_messages_received_total",
				Help: "Total number of messages received from clients",
			},
			[]string{"channel"},
		),
		activeConnections: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "websocket_active_connections",
				Help: "Number of active client connections",
			},
			[]string{},
		),
		activeSubscriptions: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "websocket_active_subscriptions",
				Help: "Number of active subscriptions per channel",
			},
			[]string{"channel"},
		),
		errorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "websocket_errors_total",
				Help: "Total number of errors encountered",
			},
			[]string{"error_type"},
		),
	}

	// Register Prometheus metrics
	prometheus.MustRegister(
		handler.messagesSentTotal,
		handler.messagesReceivedTotal,
		handler.activeConnections,
		handler.activeSubscriptions,
		handler.errorsTotal,
	)

	// Expose metrics endpoint
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		logger.Error("Prometheus server failed", zap.Error(http.ListenAndServe(":9090", nil)))
	}()

	logger.Info("Websocket handler created", zap.Any("config", cfg))

	if cfg.Delta.Enabled {
		handler.deltaClient = clients.NewDeltaWebsocketClient(handlerCtx, &cfg.Delta)
		logger.Info("Delta Exchange client created")

		// Register handlers for Delta Exchange channels
		for _, channel := range cfg.Delta.Channels {
			handler.registerDeltaHandler(channel)
		}

		if err := handler.deltaClient.Connect(); err != nil {
			logger.Error("Failed to connect to Delta Exchange", zap.Error(err))
		}
	}

	go handler.run()
	return handler
}

// HandleWebsocket handles a websocket connection
func (h *WebsocketHandler) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("Failed to upgrade connection", zap.Error(err))
		return
	}

	client := &Client{
		conn:           conn,
		send:           make(chan []byte, 1024), // Increased buffer size
		subscriptions:  make(map[string]bool),
		productFilters: make(map[string][]string),
		id:             fmt.Sprintf("%d", time.Now().UnixNano()),
		connectedAt:    time.Now(),
		lastActivity:   time.Now(),
	}

	h.register <- client
	h.logger.Info("Client connected", zap.String("client_id", client.id))

	go h.readPump(client)
	go h.writePump(client)
}

// BroadcastToChannel broadcasts a message to all clients subscribed to a channel
// BroadcastToChannel broadcasts a message to all clients subscribed to a channel
func (h *WebsocketHandler) BroadcastToChannel(channel string, message []byte, productID string) {
    start := time.Now()
    defer func() {
        h.logger.Info("Broadcast completed",
            zap.String("channel", channel),
            zap.String("product_id", productID),
            zap.Duration("latency", time.Since(start)))
    }()

    // Log raw message for debugging
    h.logger.Info("Processing raw message for broadcast",
        zap.String("channel", channel),
        zap.ByteString("message", message))

    // Transform v2/spot_price message to expected format
    if channel == "v2/spot_price" {
        var msg map[string]interface{}
        if err := json.Unmarshal(message, &msg); err != nil {
            h.logger.Error("Failed to parse v2/spot_price message", zap.Error(err))
            return
        }

        // Log parsed message to inspect field names
        h.logger.Info("Parsed v2/spot_price message", zap.Any("message", msg))

        // Try to get symbol and price, supporting both 'symbol'/'price' and 's'/'p'
        symbol, symbolOk := msg["symbol"]
        if !symbolOk {
            symbol, symbolOk = msg["s"]
        }
        price, priceOk := msg["price"]
        if !priceOk {
            price, priceOk = msg["p"]
        }

        if !symbolOk || !priceOk {
            h.logger.Warn("Message missing required fields",
                zap.Bool("symbol_ok", symbolOk),
                zap.Bool("price_ok", priceOk),
                zap.Any("message", msg))
            return
        }

        // Transform to expected format
        transformedMsg := map[string]interface{}{
            "s":    symbol,
            "p":    price,
            "type": msg["type"],
        }

        // Add timestamp if missing
        if _, hasTimestamp := msg["timestamp"]; !hasTimestamp {
            transformedMsg["timestamp"] = time.Now().UnixNano() / int64(time.Millisecond)
        }

        // Re-marshal the transformed message
        updatedMessage, err := json.Marshal(transformedMsg)
        if err != nil {
            h.logger.Error("Failed to marshal transformed v2/spot_price message", zap.Error(err))
            return
        }
        message = updatedMessage
    }

    h.subscriptionsMu.RLock()
    clients, ok := h.subscriptions[channel]
    h.subscriptionsMu.RUnlock()
    if !ok {
        h.logger.Warn("No clients subscribed to channel", zap.String("channel", channel))
        return
    }

    // Parse the message to check for timestamp and transform fields
    var msg map[string]interface{}
    if err := json.Unmarshal(message, &msg); err != nil {
        h.logger.Error("Failed to parse message for processing", zap.Error(err))
        return
    }

    // Add timestamp if missing
    if _, hasTimestamp := msg["timestamp"]; !hasTimestamp {
        msg["timestamp"] = time.Now().UnixNano() / int64(time.Millisecond) // Microsecond precision
    }

    // Transform spot_30mtwap_price message to match expected format
    if channel == "spot_30mtwap_price" {
        // Log warning if price deviates significantly
        if priceStr, ok := msg["price"].(string); ok {
            if price, err := strconv.ParseFloat(priceStr, 64); err == nil {
                expectedPrice := 0.0014579
                if math.Abs(price-expectedPrice) > expectedPrice*100 { // Arbitrary threshold
                    h.logger.Warn("Unexpected price for .DEXBTUSD",
                        zap.Float64("received_price", price),
                        zap.Float64("expected_price", expectedPrice))
                }
            }
        }

        // Create new message with expected fields and order
        transformedMsg := map[string]interface{}{
            "symbol":    msg["symbol"],
            "price":     msg["price"],
            "type":      msg["type"],
            "timestamp": msg["timestamp"],
        }

        // Re-marshal the transformed message
        updatedMessage, err := json.Marshal(transformedMsg)
        if err != nil {
            h.logger.Error("Failed to marshal transformed message", zap.Error(err))
            return
        }
        message = updatedMessage
    }

    // Transform funding_rate message to match expected format
    if channel == "funding_rate" {
        // Log warning if product_id doesn't match expected
        if pid, ok := msg["product_id"].(float64); ok && pid != 139 {
            h.logger.Warn("Unexpected product_id for BTCUSD",
                zap.Float64("received_product_id", pid),
                zap.Float64("expected_product_id", 139))
        }

        // Create new message with expected fields and order
        transformedMsg := map[string]interface{}{
            "symbol":                   msg["symbol"],
            "product_id":               msg["product_id"],
            "type":                     msg["type"],
            "funding_rate":             msg["funding_rate"],
            "funding_rate_8h":          msg["funding_rate_8h"],
            "next_funding_realization": msg["next_funding_realization"],
            "predicted_funding_rate":   msg["predicted_funding_rate"],
            "timestamp":                msg["timestamp"],
        }

        // Re-marshal the transformed message
        updatedMessage, err := json.Marshal(transformedMsg)
        if err != nil {
            h.logger.Error("Failed to marshal transformed message", zap.Error(err))
            return
        }
        message = updatedMessage
    }

    for client := range clients {
        client.mu.RLock()
        clientProductIDs, hasFilter := client.productFilters[channel]
        client.mu.RUnlock()

        if hasFilter && len(clientProductIDs) > 0 {
            matched := false
            for _, clientProductID := range clientProductIDs {
                if clientProductID == "all" || productID == clientProductID {
                    matched = true
                    break
                }
            }
            if !matched {
                continue
            }
        }

        h.logger.Info("Broadcasting message",
            zap.String("client_id", client.id),
            zap.String("channel", channel),
            zap.String("product_id", productID))

        select {
        case client.send <- message:
            atomic.AddInt64(&h.messagesSent, 1)
            h.messagesSentTotal.WithLabelValues(channel, productID).Inc()
        default:
            h.logger.Warn("Client channel full, unregistering",
                zap.String("client_id", client.id))
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
	h.clientsMu.RLock()
	activeConnections := len(h.clients)
	h.clientsMu.RUnlock()

	h.subscriptionsMu.RLock()
	activeSubscriptions := 0
	subscriptionsByChannel := make(map[string]int)
	for channel, clients := range h.subscriptions {
		subscriptionsByChannel[channel] = len(clients)
		activeSubscriptions += len(clients)
	}
	h.subscriptionsMu.RUnlock()

	externalSources := make(map[string]bool)
	if h.deltaClient != nil {
		externalSources["delta"] = h.deltaClient.IsConnected()
	}

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
	h.logger.Info("Websocket handler closed")
}

// registerDeltaHandler registers a handler for a Delta Exchange channel
func (h *WebsocketHandler) registerDeltaHandler(channel string) {
	h.deltaClient.RegisterHandler(channel, func(message []byte, msgProductID string) {
		h.BroadcastToChannel(channel, message, msgProductID)
	})
	h.logger.Info("Registered Delta handler", zap.String("channel", channel))
}

// run runs the websocket handler
func (h *WebsocketHandler) run() {
	defer func() {
		h.clientsMu.Lock()
		for client := range h.clients {
			client.conn.Close()
		}
		h.clientsMu.Unlock()

		if h.deltaClient != nil {
			h.deltaClient.Close()
		}
		h.logger.Info("Websocket handler stopped")
	}()

	for {
		select {
		case <-h.ctx.Done():
			h.logger.Info("Context canceled, stopping websocket handler")
			return
		case client := <-h.register:
			h.clientsMu.Lock()
			h.clients[client] = true
			h.activeConnections.WithLabelValues().Set(float64(len(h.clients)))
			h.clientsMu.Unlock()
			h.logger.Info("Client registered", zap.String("client_id", client.id))
		case client := <-h.unregister:
			h.clientsMu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				h.activeConnections.WithLabelValues().Set(float64(len(h.clients)))
			}
			h.clientsMu.Unlock()

			h.subscriptionsMu.Lock()
			for channel, clients := range h.subscriptions {
				if _, ok := clients[client]; ok {
					delete(clients, client)
					h.activeSubscriptions.WithLabelValues(channel).Set(float64(len(clients)))
					if len(clients) == 0 {
						delete(h.subscriptions, channel)
					}
				}
			}
			h.subscriptionsMu.Unlock()
			h.logger.Info("Client unregistered", zap.String("client_id", client.id))
		case message := <-h.broadcast:
			h.clientsMu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
					atomic.AddInt64(&h.messagesSent, 1)
					h.messagesSentTotal.WithLabelValues("broadcast", "").Inc()
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
		h.logger.Info("Client read pump stopped", zap.String("client_id", client.id))
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
				h.logger.Error("Error reading message",
					zap.Error(err),
					zap.String("client_id", client.id))
				h.errorsTotal.WithLabelValues("read_message").Inc()
			}
			break
		}

		client.lastActivity = time.Now()

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			h.logger.Error("Error parsing message",
				zap.Error(err),
				zap.ByteString("message", message),
				zap.String("client_id", client.id))
			h.errorsTotal.WithLabelValues("message_parse").Inc()
			continue
		}

		var channel string
		if msgType, ok := msg["type"].(string); ok {
			channel = msgType
		}

		atomic.AddInt64(&h.messagesReceived, 1)
		h.messagesReceivedTotal.WithLabelValues(channel).Inc()
		h.logger.Info("Received client message",
			zap.String("client_id", client.id),
			zap.String("type", channel))

		if msgType, ok := msg["type"].(string); ok {
			switch msgType {
			case "subscribe":
				h.handleSubscribe(client, msg)
			case "unsubscribe":
				h.handleUnsubscribe(client, msg)
			case "ping":
				h.handlePing(client)
			default:
				h.logger.Warn("Unknown message type",
					zap.String("type", msgType),
					zap.String("client_id", client.id))
				h.errorsTotal.WithLabelValues("unknown_message_type").Inc()
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
		h.logger.Info("Client write pump stopped", zap.String("client_id", client.id))
	}()

	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				h.logger.Error("Failed to get writer",
					zap.Error(err),
					zap.String("client_id", client.id))
				h.errorsTotal.WithLabelValues("write_message").Inc()
				return
			}
			w.Write(message)

			n := len(client.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				msg := <-client.send
				w.Write(msg)
				atomic.AddInt64(&h.messagesSent, 1)
				h.messagesSentTotal.WithLabelValues("queued", "").Inc()
			}

			if err := w.Close(); err != nil {
				h.logger.Error("Failed to close writer",
					zap.Error(err),
					zap.String("client_id", client.id))
				h.errorsTotal.WithLabelValues("write_close").Inc()
				return
			}
		case <-ticker.C:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				h.logger.Error("Failed to send ping",
					zap.Error(err),
					zap.String("client_id", client.id))
				h.errorsTotal.WithLabelValues("ping").Inc()
				return
			}
		}
	}
}

// handleSubscribe handles a subscribe message
func (h *WebsocketHandler) handleSubscribe(client *Client, msg map[string]interface{}) {
	h.logger.Info("Processing subscribe message",
		zap.String("client_id", client.id),
		zap.Any("message", msg))

	subscribedChannels := []map[string]interface{}{}
	if payload, ok := msg["payload"].(map[string]interface{}); ok {
		if channels, ok := payload["channels"].([]interface{}); ok {
			for _, channelObj := range channels {
				if channelMap, ok := channelObj.(map[string]interface{}); ok {
					channelName, ok := channelMap["name"].(string)
					if !ok {
						h.logger.Warn("Channel object does not contain a name",
							zap.String("client_id", client.id))
						h.errorsTotal.WithLabelValues("invalid_channel_name").Inc()
						continue
					}

					var productIDs []string
					if symbols, ok := channelMap["symbols"].([]interface{}); ok {
						for _, symbol := range symbols {
							if symbolStr, ok := symbol.(string); ok {
								productIDs = append(productIDs, symbolStr)
							} else if symbolFloat, ok := symbol.(float64); ok {
								productIDs = append(productIDs, fmt.Sprintf("%v", symbolFloat))
							}
						}
					}

					h.logger.Info("Subscribing to channel",
						zap.String("client_id", client.id),
						zap.String("channel", channelName),
						zap.Strings("product_ids", productIDs))

					if h.deltaClient != nil {
						if channel, ok := msg["type"].(string); ok {
							if channel == "subscribe" {
								h.registerDeltaHandler(channelName)
								h.deltaClient.Subscribe(channelName, productIDs)
							}
						}
					}

					h.subscribeClient(client, channelName, productIDs)
					subscribedChannels = append(subscribedChannels, map[string]interface{}{
						"name":    channelName,
						"symbols": productIDs,
					})
				}
			}
		} else {
			h.logger.Warn("Subscribe message does not contain a payload",
				zap.String("client_id", client.id))
			h.errorsTotal.WithLabelValues("invalid_payload").Inc()
			return
		}
	} else {
		h.logger.Warn("Subscribe message does not contain a payload",
			zap.String("client_id", client.id))
		h.errorsTotal.WithLabelValues("invalid_payload").Inc()
		return
	}

	response := map[string]interface{}{
		"type": "subscribed",
		"payload": map[string]interface{}{
			"channels": subscribedChannels,
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		h.logger.Error("Error marshaling subscription confirmation",
			zap.Error(err),
			zap.String("client_id", client.id))
		h.errorsTotal.WithLabelValues("marshal_error").Inc()
		return
	}

	h.logger.Info("Sending subscription confirmation",
		zap.String("client_id", client.id),
		zap.String("response", string(data)))
	client.send <- data
}

// handleUnsubscribe handles an unsubscribe message
func (h *WebsocketHandler) handleUnsubscribe(client *Client, msg map[string]interface{}) {
	h.logger.Info("Processing unsubscribe message",
		zap.String("client_id", client.id),
		zap.Any("message", msg))

	var unsubscribedChannels []string
	if payload, ok := msg["payload"].(map[string]interface{}); ok {
		if channels, ok := payload["channels"].([]interface{}); ok {
			for _, channelObj := range channels {
				if channelMap, ok := channelObj.(map[string]interface{}); ok {
					channelName, ok := channelMap["name"].(string)
					if !ok {
						h.logger.Warn("Channel object does not contain a name",
							zap.String("client_id", client.id))
						h.errorsTotal.WithLabelValues("invalid_channel_name").Inc()
						continue
					}

					h.logger.Info("Unsubscribing from channel",
						zap.String("client_id", client.id),
						zap.String("channel", channelName))

					h.unsubscribeClient(client, channelName)
					unsubscribedChannels = append(unsubscribedChannels, channelName)

					if h.deltaClient != nil {
						h.subscriptionsMu.RLock()
						clients, ok := h.subscriptions[channelName]
						h.subscriptionsMu.RUnlock()
						if ok && len(clients) == 0 {
							h.logger.Info("No clients left, unsubscribing from Delta",
								zap.String("channel", channelName))
							if err := h.deltaClient.Unsubscribe(channelName); err != nil {
								h.logger.Error("Failed to unsubscribe from Delta channel",
									zap.String("channel", channelName),
									zap.Error(err))
								h.errorsTotal.WithLabelValues("delta_unsubscribe").Inc()
							}
						} else if ok {
							h.logger.Info("Clients still subscribed",
								zap.String("channel", channelName),
								zap.Int("client_count", len(clients)))
						}
					}
				}
			}
		} else {
			h.logger.Warn("Unsubscribe message does not contain a payload",
				zap.String("client_id", client.id))
			h.errorsTotal.WithLabelValues("invalid_payload").Inc()
			return
		}
	} else {
		h.logger.Warn("Unsubscribe message does not contain a payload",
			zap.String("client_id", client.id))
		h.errorsTotal.WithLabelValues("invalid_payload").Inc()
		return
	}

	for _, channelName := range unsubscribedChannels {
		response := map[string]interface{}{
			"type":   "unsubscribed",
			"channel": channelName,
		}
		data, err := json.Marshal(response)
		if err != nil {
			h.logger.Error("Error marshaling unsubscription confirmation",
				zap.Error(err),
				zap.String("client_id", client.id),
				zap.String("channel", channelName))
			h.errorsTotal.WithLabelValues("marshal_error").Inc()
			continue
		}
		client.send <- data
	}
}

// handlePing handles a ping message
func (h *WebsocketHandler) handlePing(client *Client) {
	response := map[string]interface{}{
		"type": "pong",
		"time": time.Now().UnixNano() / int64(time.Millisecond),
	}
	data, err := json.Marshal(response)
	if err != nil {
		h.logger.Error("Error marshaling pong response",
			zap.Error(err),
			zap.String("client_id", client.id))
		h.errorsTotal.WithLabelValues("marshal_error").Inc()
		return
	}
	client.send <- data
	h.logger.Info("Sent pong response", zap.String("client_id", client.id))
}

// subscribeClient subscribes a client to a channel
func (h *WebsocketHandler) subscribeClient(client *Client, channel string, productIDs []string) {
	client.mu.Lock()
	client.subscriptions[channel] = true
	client.productFilters[channel] = productIDs
	client.mu.Unlock()

	h.subscriptionsMu.Lock()
	if _, ok := h.subscriptions[channel]; !ok {
		h.subscriptions[channel] = make(map[*Client]bool)
	}
	h.subscriptions[channel][client] = true
	h.activeSubscriptions.WithLabelValues(channel).Inc()
	h.subscriptionsMu.Unlock()

	h.logger.Info("Client subscribed",
		zap.String("client_id", client.id),
		zap.String("channel", channel),
		zap.Strings("product_ids", productIDs))
}

// unsubscribeClient unsubscribes a client from a channel
func (h *WebsocketHandler) unsubscribeClient(client *Client, channel string) {
	client.mu.Lock()
	delete(client.subscriptions, channel)
	delete(client.productFilters, channel)
	client.mu.Unlock()

	h.subscriptionsMu.Lock()
	if clients, ok := h.subscriptions[channel]; ok {
		delete(clients, client)
		h.activeSubscriptions.WithLabelValues(channel).Set(float64(len(clients)))
		if len(clients) == 0 {
			delete(h.subscriptions, channel)
		}
	}
	h.subscriptionsMu.Unlock()

	h.logger.Info("Client unsubscribed",
		zap.String("client_id", client.id),
		zap.String("channel", channel))
}