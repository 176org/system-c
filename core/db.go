package database

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/176org/system-c/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresConnectionProcess struct {
	config    string
	pool      *pgxpool.Pool
	logger    *logger.Logger
	mu        sync.Mutex
	state     string
	lastError error
}

func (p *PostgresConnectionProcess) GetPool() *pgxpool.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pool
}

func (p *PostgresConnectionProcess) ExecuteQuery(ctx context.Context, query string, args ...interface{}) (*pgxpool.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pool == nil {
		return nil, fmt.Errorf("database connection pool is not initialized")
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire connection: %v", err)
	}

	return conn, nil
}

func NewPostgresConnectionProcess(
	connectionString string,
	logger *logger.Logger,
) *PostgresConnectionProcess {
	return &PostgresConnectionProcess{
		config: connectionString,
		logger: logger,
		state:  "INITIALIZING",
	}
}

func (p *PostgresConnectionProcess) Initialize(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.ProcessStateChange("postgres-connection", p.state, "INITIALIZING",
		p.logger.Zap("connectionString", p.config))

	p.state = "INITIALIZING"
	p.logger.Info("Initializing Postgres connection")

	return nil
}

func (p *PostgresConnectionProcess) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	oldState := p.state
	p.state = "CONNECTING"
	p.logger.ProcessStateChange("postgres-connection", oldState, "CONNECTING")

	p.logger.LogProcessEvent("postgres-connection", "CONNECTING", "connection_attempt", map[string]interface{}{
		"connection_string": p.config,
	})

	config, err := pgxpool.ParseConfig(p.config)
	if err != nil {
		p.state = "ERROR"
		p.lastError = err
		p.logger.ProcessStateChange("postgres-connection", "CONNECTING", "ERROR")
		p.logger.Error("Failed to parse Postgres config",
			p.logger.Zap("error", err))
		return fmt.Errorf("failed to parse postgres config: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		p.state = "ERROR"
		p.lastError = err
		p.logger.ProcessStateChange("postgres-connection", "CONNECTING", "ERROR")
		p.logger.Error("Unable to connect to database",
			p.logger.Zap("error", err))
		return fmt.Errorf("unable to connect to database: %v", err)
	}

	// Verify connection
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		p.state = "ERROR"
		p.lastError = err
		p.logger.ProcessStateChange("postgres-connection", "CONNECTING", "ERROR")
		p.logger.Error("Failed to acquire database connection",
			p.logger.Zap("error", err))
		return fmt.Errorf("failed to acquire connection: %v", err)
	}
	defer conn.Release()

	err = conn.Conn().Ping(ctx)
	if err != nil {
		pool.Close()
		p.state = "ERROR"
		p.lastError = err
		p.logger.ProcessStateChange("postgres-connection", "CONNECTING", "ERROR")
		p.logger.Error("Database ping failed",
			p.logger.Zap("error", err))
		return fmt.Errorf("database ping failed: %v", err)
	}

	p.pool = pool
	p.state = "READY"
	p.logger.ProcessStateChange("postgres-connection", "CONNECTING", "READY")

	return nil
}

func (p *PostgresConnectionProcess) Disconnect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.LogProcessEvent("postgres-connection", p.state, "disconnection_attempt", nil)

	if p.pool != nil {
		oldState := p.state
		p.state = "TERMINATING"
		p.logger.ProcessStateChange("postgres-connection", oldState, "TERMINATING")

		p.pool.Close()
		p.pool = nil

		p.state = "TERMINATED"
		p.logger.ProcessStateChange("postgres-connection", "TERMINATING", "TERMINATED")
	}
	return nil
}

func (p *PostgresConnectionProcess) IsHealthy(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pool == nil {
		p.logger.Warn("Database connection pool is nil")
		return false
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		p.logger.Error("Failed to acquire connection",
			p.logger.Zap("error", err))
		return false
	}
	defer conn.Release()

	// Perform a lightweight health check
	var result int
	err = conn.Conn().QueryRow(ctx, "SELECT 1").Scan(&result)

	if err != nil {
		p.logger.Error("Health check query failed",
			p.logger.Zap("error", err))
		return false
	}

	return result == 1
}

// PostgresRecoveryStrategy implements the RecoveryStrategy interface
type PostgresRecoveryStrategy struct {
	Logger *logger.Logger
}

func (s *PostgresRecoveryStrategy) Recover(ctx context.Context, processID string) error {
	// Log recovery attempt
	s.Logger.Warn("Attempting Postgres connection recovery",
		s.Logger.Zap("processID", processID))

	// Implement specific recovery logic
	details := map[string]interface{}{
		"recovery_type": "soft_reset",
		"timestamp":     time.Now(),
	}

	s.Logger.LogProcessEvent(processID, "ERROR", "recovery_initiated", details)

	// Return an error if recovery fails, otherwise return nil
	return nil
}
