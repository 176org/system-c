package processmanager

import (
	"context"
	"fmt"

	"sync"
	"time"

	"github.com/176org/system-c/pkg/logger"
)

type RecoveryStrategy interface {
	Recover(ctx context.Context, processID string) error
}

type ProcessConfig struct {
	MaxRetries           int
	RetryBackoffStrategy func(attempt int) time.Duration
	InitialTimeout       time.Duration
	MaxTimeout           time.Duration
	RecoveryStrategy     RecoveryStrategy
}

type DatabaseProcess interface {
	Initialize(ctx context.Context) error
	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error
	IsHealthy(ctx context.Context) bool
}

type ProcessManager struct {
	processes      sync.Map
	logger         *logger.Logger
	config         ProcessConfig
	stopMonitoring chan struct{}
	mu             sync.RWMutex
}

type ProcessInstance struct {
	ID             string
	Process        DatabaseProcess
	State          string
	LastError      error
	Attempts       int
	CancelFunc     context.CancelFunc
	LastConnection time.Time
}

func NewProcessManager(logger *logger.Logger, config ProcessConfig) *ProcessManager {
	if config.RetryBackoffStrategy == nil {
		config.RetryBackoffStrategy = defaultExponentialBackoff
	}

	pm := &ProcessManager{
		logger:         logger,
		config:         config,
		stopMonitoring: make(chan struct{}),
	}

	go pm.startHealthMonitoring()
	return pm
}

func defaultExponentialBackoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt)) * time.Second
}

func (pm *ProcessManager) RegisterProcess(id string, process DatabaseProcess) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	instance := &ProcessInstance{
		ID:         id,
		Process:    process,
		State:      "INITIALIZING",
		CancelFunc: cancel,
	}

	// Log initial state
	pm.logger.ProcessStateChange(id, "", "INITIALIZING")

	err := pm.connectProcess(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to register process %s: %v", id, err)
	}

	pm.processes.Store(id, instance)
	pm.logger.Info("Process registered", logger.Zap("processID", id))
	return nil
}

func (pm *ProcessManager) connectProcess(ctx context.Context, instance *ProcessInstance) error {
	oldState := instance.State
	instance.State = "CONNECTING"
	pm.logger.ProcessStateChange(instance.ID, oldState, "CONNECTING")

	startTime := time.Now()
	for instance.Attempts < pm.config.MaxRetries {
		err := instance.Process.Connect(ctx)
		if err == nil {
			oldState = instance.State
			instance.State = "READY"
			instance.LastConnection = time.Now()
			instance.Attempts = 0

			pm.logger.ProcessStateChange(instance.ID, oldState, "READY")
			return nil
		}

		instance.LastError = err
		instance.Attempts++

		backoff := pm.config.RetryBackoffStrategy(instance.Attempts)
		if time.Since(startTime)+backoff > pm.config.MaxTimeout {
			instance.State = "ERROR"
			pm.logger.ProcessStateChange(instance.ID, oldState, "ERROR")
			return fmt.Errorf("max connection timeout exceeded: %v", err)
		}

		pm.logger.LogProcessEvent(instance.ID, "CONNECTING", "CONNECTION_RETRY", map[string]interface{}{
			"attempt": instance.Attempts,
			"error":   err,
		})

		time.Sleep(backoff)
	}

	instance.State = "ERROR"
	pm.logger.ProcessStateChange(instance.ID, oldState, "ERROR")
	return fmt.Errorf("max retries reached for process %s", instance.ID)
}

func (pm *ProcessManager) startHealthMonitoring() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pm.stopMonitoring:
			return
		case <-ticker.C:
			pm.checkProcessHealth()
		}
	}
}

func (pm *ProcessManager) checkProcessHealth() {
	pm.processes.Range(func(key, value interface{}) bool {
		instance := value.(*ProcessInstance)
		pm.mu.Lock()
		defer pm.mu.Unlock()

		if !instance.Process.IsHealthy(context.Background()) {
			pm.logger.LogProcessEvent(instance.ID, instance.State, "HEALTH_CHECK_FAILED", nil)
			go pm.recoverProcess(instance)
		}
		return true
	})
}

func (pm *ProcessManager) recoverProcess(instance *ProcessInstance) {
	oldState := instance.State
	instance.State = "RECONNECTING"
	pm.logger.ProcessStateChange(instance.ID, oldState, "RECONNECTING")

	if pm.config.RecoveryStrategy != nil {
		err := pm.config.RecoveryStrategy.Recover(context.Background(), instance.ID)
		if err != nil {
			pm.logger.LogProcessEvent(
				instance.ID,
				"RECONNECTING",
				"RECOVERY_FAILED",
				map[string]interface{}{"error": err},
			)

			instance.State = "ERROR"
			pm.logger.ProcessStateChange(instance.ID, "RECONNECTING", "ERROR")
		} else {
			instance.State = "READY"
			pm.logger.ProcessStateChange(instance.ID, "RECONNECTING", "READY")
		}
	}
}

func (pm *ProcessManager) Stop() {
	close(pm.stopMonitoring)

	pm.processes.Range(func(key, value interface{}) bool {
		instance := value.(*ProcessInstance)

		oldState := instance.State
		instance.State = "TERMINATING"
		pm.logger.ProcessStateChange(instance.ID, oldState, "TERMINATING")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		instance.Process.Disconnect(ctx)
		instance.CancelFunc()

		instance.State = "TERMINATED"
		pm.logger.ProcessStateChange(instance.ID, "TERMINATING", "TERMINATED")

		return true
	})
}

// GetProcesses exposes the processes field for external access.
func (pm *ProcessManager) GetProcesses() *sync.Map {
	return &pm.processes
}
