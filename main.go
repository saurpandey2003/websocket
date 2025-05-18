package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	websocketv1 "github.com/Cryptovate-India/websocket-service/gen/websocket/api/v1"
	"github.com/Cryptovate-India/websocket-service/internal/config"
	"github.com/Cryptovate-India/websocket-service/internal/handlers"
	"github.com/Cryptovate-India/websocket-service/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {

	// Create a context that is canceled when the program receives an interrupt signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalChan
		log.Println("Received interrupt signal, shutting down...")
		cancel()
		// Give the server some time to gracefully shut down
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()

	// Load the configuration
	cfg, err := config.LoadConfig("websocket-service")
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Create the websocket handler
	websocketHandler := handlers.NewWebsocketHandler(ctx, cfg)
	defer websocketHandler.Close()

	// Create the gRPC server
	grpcServer := grpc.NewServer()
	websocketServer := server.NewServer(ctx, cfg, websocketHandler)
	websocketv1.RegisterWebsocketServiceServer(grpcServer, websocketServer)
	reflection.Register(grpcServer)

	// Start the gRPC server
	grpcListener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port: %v", err)
	}
	go func() {
		log.Printf("Starting gRPC server on port %d", cfg.GRPCPort)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("Failed to start gRPC server: %v", err)
		}
	}()

	// Create the HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", websocketHandler.HandleWebsocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Add metrics endpoint if enabled
	if cfg.Metrics.Enabled {
		mux.HandleFunc(cfg.Metrics.Endpoint, func(w http.ResponseWriter, r *http.Request) {
			// In a real implementation, you would use a metrics library like Prometheus
			stats := websocketHandler.GetStatistics()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"active_connections":%d,"active_subscriptions":%d,"messages_sent":%d,"messages_received":%d}`,
				stats["active_connections"], stats["active_subscriptions"], stats["messages_sent"], stats["messages_received"])
		})
	}

	// Start the HTTP server
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: mux,
	}
	go func() {
		log.Printf("Starting HTTP server on port %d", cfg.HTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	// Wait for the context to be canceled
	<-ctx.Done()

	// Shut down the HTTP server
	log.Println("Shutting down HTTP server...")
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpCancel()
	if err := httpServer.Shutdown(httpCtx); err != nil {
		log.Printf("Failed to shut down HTTP server: %v", err)
	}

	// Shut down the gRPC server
	log.Println("Shutting down gRPC server...")
	grpcServer.GracefulStop()

	log.Println("Server shutdown complete")
}
