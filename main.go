package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"

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

func (ps *PostgresServer) Start(port int) error {

	// Create a default configuration if not provided
	config := processmanager.ProcessConfig{
		MaxRetries: 3,
		RetryBackoffStrategy: func(attempt int) time.Duration {
			return time.Duration(attempt*2) * time.Second
		},
		InitialTimeout: 10 * time.Second,
		MaxTimeout:     30 * time.Second,
	}
	// Initialize the Postgres connection process explicitly
	ctx, cancel := context.WithTimeout(context.Background(), config.InitialTimeout)
	defer cancel()

	err := ps.postgresConn.Initialize(ctx)
	if err != nil {
		ps.logger.Error("Failed to initialize Postgres process",
			ps.logger.Zap("error", err))
		return fmt.Errorf("initialization failed: %v", err)
	}

	// Register the Postgres process with detailed logging and error handling
	err = ps.processManager.RegisterProcess("postgres-main", ps.postgresConn)
	if err != nil {
		ps.logger.Error("Failed to register Postgres process",
			ps.logger.Zap("error", err))
		return fmt.Errorf("process registration failed: %v", err)
	}

	// Setup graceful server with more comprehensive shutdown handling
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: ps.createRoutes(),
		// Add readiness and liveness probe handlers
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in a way that allows for graceful shutdown
	go func() {
		ps.logger.Info("Starting Postgres HTTP server", ps.logger.Zap("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			ps.logger.Error("HTTP server error", ps.logger.Zap("error", err))
		}
	}()

	return nil
}

func (ps *PostgresServer) createRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/query", ps.handleQuery)

	// Add health check endpoint
	mux.HandleFunc("/health", ps.handleHealthCheck)

	return mux
}

func (ps *PostgresServer) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	// Comprehensive health check
	if ps.postgresConn.IsHealthy(r.Context()) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "healthy",
			"details": "All systems operational",
		})
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "unhealthy",
			"details": "Database connection issues detected",
		})
	}
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
		ps.logger.Error("Failed to acquire connection", ps.logger.Zap("error", err))
		http.Error(w, fmt.Sprintf("Connection failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Release()

	// Perform the query
	ps.logger.Info("Executing query",
		ps.logger.Zap("query", queryReq.Query),
		ps.logger.Zap("args", queryReq.Args),
	)

	rows, err := conn.Query(ctx, queryReq.Query, queryReq.Args...)
	if err != nil {
		ps.logger.Error("Query execution failed", ps.logger.Zap("error", err))
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
			ps.logger.Error("Failed to read row", ps.logger.Zap("error", err))
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
	// Enhanced shutdown process with comprehensive checks and human verification
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Collect detailed process information
	var (
		runningProcesses  []string
		criticalProcesses []string
	)

	ps.processManager.GetProcesses().Range(func(key, value interface{}) bool {
		instance := value.(*processmanager.ProcessInstance)
		processInfo := fmt.Sprintf("%s (State: %s)", instance.ID, instance.State)

		runningProcesses = append(runningProcesses, processInfo)

		// Identify critical processes that might block shutdown
		if instance.State != "TERMINATED" &&
			instance.State != "ERROR" &&
			instance.State != "INITIALIZING" {
			criticalProcesses = append(criticalProcesses, processInfo)
		}
		return true
	})

	// Interactive shutdown confirmation
	if len(criticalProcesses) > 0 {
		fmt.Println("\n=== SHUTDOWN VERIFICATION ===")
		fmt.Println("Critical Processes Detected:")
		for _, process := range criticalProcesses {
			fmt.Println(" - " + process)
		}

		// Comprehensive shutdown prompt
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("\nWarning: Critical processes are still running.")
		fmt.Println("Options:")
		fmt.Println("1. Proceed with forced shutdown")
		fmt.Println("2. Cancel shutdown")
		fmt.Println("3. Wait and retry shutdown")
		fmt.Print("Enter your choice (1/2/3): ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(strings.ToLower(choice))

		switch choice {
		case "1":
			ps.logger.Warn("Forced shutdown initiated",
				ps.logger.Zap("critical_processes", criticalProcesses))
		case "2":
			fmt.Println("Shutdown cancelled.")
			return nil
		case "3":
			fmt.Println("Waiting for processes to complete...")
			// Implement a wait mechanism or return to allow retry
			return fmt.Errorf("shutdown deferred")
		}
	}

	// Shutdown HTTP server
	srv := &http.Server{Addr: ":8080"}
	if err := srv.Shutdown(ctx); err != nil {
		ps.logger.Error("HTTP server shutdown error", ps.logger.Zap("error", err))
	}

	// Stop process manager with timeout
	ps.processManager.Stop()

	ps.logger.Info("Graceful shutdown complete",
		ps.logger.Zap("running_processes", runningProcesses))

	return nil
}

// main function to run the server
func main() {
	// Configure logger
	logger := logger.NewLogger("development")

	// Postgres connection string
	connectionString := "postgresql://user:password@localhost:5432/db"

	if connectionString == "" {
		logger.Error("Database connection string not set")
		os.Exit(1)
	}
	// Create and start the server
	server := NewPostgresServer(connectionString, logger)

	// Create a channel to handle server shutdown  use signal.Notify() to capture OS signals
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan,
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	// Start server
	if err := server.Start(8080); err != nil {
		logger.Error("Failed to start server", logger.Zap("error", err))
		os.Exit(1)
	}

	// Wait for shutdown signal
	sig := <-shutdownChan
	logger.Info("Received shutdown signal", logger.Zap("signal", sig))

	// Graceful shutdown with retry mechanism
	for attempts := 0; attempts < 3; attempts++ {
		err := server.GracefulShutdown()
		if err == nil {
			logger.Info("Server shutdown successfully")
			os.Exit(0)
		}

		logger.Warn("Shutdown attempt failed",
			logger.Zap("attempt", attempts+1),
			logger.Zap("error", err))

		// Wait before retry
		time.Sleep(5 * time.Second)
	}

	// Force exit if multiple shutdown attempts fail
	logger.Error("Failed to shutdown gracefully")
	os.Exit(1)
}
