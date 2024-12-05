package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	database "github.com/176org/system-c/core"
	processmanager "github.com/176org/system-c/pkg/doctor"
	"github.com/176org/system-c/pkg/logger"
)

// QueryRequest represents the structure of incoming query requests
type QueryRequest struct {
	Query   string        `json:"query"`
	Args    []interface{} `json:"args"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// QueryResponse represents the structure of query responses
type QueryResponse struct {
	Rows     []map[string]interface{} `json:"rows"`
	RowCount int                      `json:"row_count"`
}

// PostgresServer manages the HTTP server and database connection
type PostgresServer struct {
	processManager *processmanager.ProcessManager
	postgresConn   *database.PostgresConnectionProcess
	logger         *logger.Logger
}

// NewPostgresServer initializes a new Postgres HTTP server
func NewPostgresServer(connectionString string, logger *logger.Logger) *PostgresServer {
	// Create process manager configuration
	config := processmanager.ProcessConfig{
		MaxRetries: 3,
		RetryBackoffStrategy: func(attempt int) time.Duration {
			return time.Duration(attempt*2) * time.Second
		},
		InitialTimeout: 10 * time.Second,
		MaxTimeout:     30 * time.Second,
		RecoveryStrategy: &database.PostgresRecoveryStrategy{
			Logger: logger,
		},
	}

	// Create process manager
	processManager := processmanager.NewProcessManager(logger, config)

	// Create Postgres connection process
	postgresConn := database.NewPostgresConnectionProcess(connectionString, logger)

	return &PostgresServer{
		processManager: processManager,
		postgresConn:   postgresConn,
		logger:         logger,
	}
}

// Start initializes the Postgres connection and starts the HTTP server
func (ps *PostgresServer) Start(port int) error {
	// Register the Postgres process
	err := ps.processManager.RegisterProcess("postgres-main", ps.postgresConn)
	if err != nil {
		return fmt.Errorf("failed to register Postgres process: %v", err)
	}

	// Setup HTTP server routes
	http.HandleFunc("/query", ps.handleQuery)

	// Start HTTP server
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
	}
	ps.logger.Info("Starting Postgres HTTP server", logger.Zap("port", port))

	return srv.ListenAndServe()
}

// handleQuery processes incoming database query requests
func (ps *PostgresServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	// Validate request method
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse incoming request
	var queryReq QueryRequest
	err := json.NewDecoder(r.Body).Decode(&queryReq)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Set default timeout if not provided
	if queryReq.Timeout == 0 {
		queryReq.Timeout = 10 * time.Second
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(r.Context(), queryReq.Timeout)
	defer cancel()

	// Get the connection pool
	pool := ps.postgresConn.GetPool()
	if pool == nil {
		http.Error(w, "Database connection not available", http.StatusInternalServerError)
		return
	}

	// Acquire a connection from the pool
	conn, err := pool.Acquire(ctx)
	if err != nil {
		ps.logger.Error("Failed to acquire connection", logger.Zap("error", err))
		http.Error(w, fmt.Sprintf("Connection failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Release()

	// Perform the query
	ps.logger.Info("Executing query",
		logger.Zap("query", queryReq.Query),
		logger.Zap("args", queryReq.Args),
	)

	rows, err := conn.Query(ctx, queryReq.Query, queryReq.Args...)
	if err != nil {
		ps.logger.Error("Query execution failed", logger.Zap("error", err))
		http.Error(w, fmt.Sprintf("Query failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// Process query results
	results := []map[string]interface{}{}
	fieldDescriptions := rows.FieldDescriptions()

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			ps.logger.Error("Failed to read row", logger.Zap("error", err))
			http.Error(w, "Failed to read query results", http.StatusInternalServerError)
			return
		}

		rowMap := make(map[string]interface{})
		for i, val := range values {
			rowMap[string(fieldDescriptions[i].Name)] = val
		}
		results = append(results, rowMap)
	}

	// Send response
	response := QueryResponse{
		Rows:     results,
		RowCount: len(results),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ps *PostgresServer) GracefulShutdown() error {
	// List running processes
	var runningProcesses []string
	ps.processManager.GetProcesses().Range(func(key, value interface{}) bool {
		instance := value.(*processmanager.ProcessInstance)
		if instance.State != "TERMINATED" && instance.State != "ERROR" {
			runningProcesses = append(runningProcesses, fmt.Sprintf("%s (State: %s)", instance.ID, instance.State))
		}
		return true
	})

	// Show active processes
	if len(runningProcesses) > 0 {
		fmt.Println("Active Processes:")
		for _, process := range runningProcesses {
			fmt.Println(" - " + process)
		}

		// Confirm shutdown
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Warning: Active processes detected. Proceed with shutdown? (Y/N): ")
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" {
			fmt.Println("Shutdown cancelled.")
			return nil
		}
	}

	// Perform graceful shutdown
	ps.logger.Info("Initiating shutdown...")
	// Shut down the HTTP server
	srv := &http.Server{Addr: fmt.Sprintf(":%d", 8080)} // Ensure this matches your running server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		ps.logger.Error("HTTP server shutdown error", logger.Zap("error", err))
		return fmt.Errorf("HTTP server shutdown failed: %v", err)
	}

	ps.logger.Info("HTTP server shutdown successfully.")

	// Stop the process manager
	ps.processManager.Stop()
	ps.logger.Info("THE MANAGER IS DOWN BYE.")
	return nil
}

// main function to run the server
func main() {
	// Configure logger
	logger := logger.NewLogger("development")

	// Postgres connection string
	connectionString := "postgresql://user:password@localhost:5432/db"

	// Create and start the server
	server := NewPostgresServer(connectionString, logger)

	// Create a channel to handle server shutdown  use signal.Notify() to capture OS signals
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	// Start server on port 8080
	go func() {
		logger.Info("Starting server on port 8080")
		if err := server.Start(8080); err != nil {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	go func() {
		// Simulating a shutdown signal after some time or via user input
		time.Sleep(1 * time.Hour) // use signal.Notify() instead
		shutdownChan <- os.Interrupt
	}()

	// Wait for shutdown signal
	<-shutdownChan

	// Initiate graceful shutdown
	if err := server.GracefulShutdown(); err != nil {
		logger.Sugar().Error("Shutdown error", err)

		os.Exit(1)
	}

	logger.Info("Server shutdown completed successfully")
	os.Exit(0)
}
