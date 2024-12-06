package logger

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Logger struct {
	*zap.Logger
	sugar *zap.SugaredLogger
}

// Zap creates a zap field with type-safe conversion
// func Zap(key string, value interface{}) zap.Field {
// 	switch v := value.(type) {
// 	case string:
// 		return zap.String(key, v)
// 	case int:
// 		return zap.Int(key, v)
// 	case bool:
// 		return zap.Bool(key, v)
// 	case error:
// 		return zap.Error(v)
// 	default:
// 		return zap.Any(key, value)
// 	}
// }

func (l *Logger) Zap(key string, value interface{}) zap.Field {
	switch v := value.(type) {
	case string:
		return zap.String(key, v)
	case int:
		return zap.Int(key, v)
	case bool:
		return zap.Bool(key, v)
	case error:
		return zap.Error(v)
	default:
		return zap.Any(key, value)
	}
}

// NewLogger creates a new logger based on the environment
func NewLogger(environment string) *Logger {
	var config zap.Config
	if environment == "production" {
		config = zap.NewProductionConfig()
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		config.EncoderConfig.StacktraceKey = "" // Disable stacktrace in production
	} else {
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	logger, err := config.Build()
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}

	return &Logger{
		Logger: logger,
		sugar:  logger.Sugar(),
	}
}

// ProcessStateChange logs state transitions with flexible state representation
func (l *Logger) ProcessStateChange(processID string, fromState, toState string, additionalFields ...zap.Field) {
	fields := append([]zap.Field{
		zap.String("processID", processID),
		zap.String("fromState", fromState),
		zap.String("toState", toState),
	}, additionalFields...)
	l.Info("Process state changed", fields...)
}

// LogProcessEvent logs process-related events with flexible state and details
func (l *Logger) LogProcessEvent(processID, state, eventType string, details map[string]interface{}) {
	fields := []zap.Field{
		zap.String("processID", processID),
		zap.String("state", state),
		zap.String("eventType", eventType),
	}

	for key, value := range details {
		fields = append(fields, zap.Any(key, value))
	}

	switch state {
	case "ERROR":
		l.Error("Process event logged", fields...)
	case "TERMINATED":
		l.Warn("Process event logged", fields...)
	default:
		l.Info("Process event logged", fields...)
	}
}

// Proxy methods for different log levels
func (l *Logger) Info(msg string, fields ...zap.Field) {
	l.Logger.Info(msg, fields...)
}

func (l *Logger) Error(msg string, fields ...zap.Field) {
	l.Logger.Error(msg, fields...)
}

func (l *Logger) Warn(msg string, fields ...zap.Field) {
	l.Logger.Warn(msg, fields...)
}

func (l *Logger) Debug(msg string, fields ...zap.Field) {
	l.Logger.Debug(msg, fields...)
}

// SugaredLogger returns the underlying sugared logger
func (l *Logger) SugaredLogger() *zap.SugaredLogger {
	return l.sugar
}
