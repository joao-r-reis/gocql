package gocql

import (
	"bytes"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
)

type StdLogger interface {
	Print(v ...interface{})
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

type nopLogger struct{}

func (n nopLogger) Print(_ ...interface{}) {}

func (n nopLogger) Printf(_ string, _ ...interface{}) {}

func (n nopLogger) Println(_ ...interface{}) {}

func (n nopLogger) Error(_ string, _ ...LogField) {}

func (n nopLogger) Warning(_ string, _ ...LogField) {}

func (n nopLogger) Info(_ string, _ ...LogField) {}

func (n nopLogger) Debug(_ string, _ ...LogField) {}

type testLogger struct {
	capture bytes.Buffer
	mu      sync.Mutex
}

func (l *testLogger) Print(v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(&l.capture, v...)
}

func (l *testLogger) Printf(format string, v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.capture, format, v...)
}

func (l *testLogger) Println(v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(&l.capture, v...)
}

func (l *testLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.capture.String()
}

type defaultLogger struct{}

func (l *defaultLogger) Print(v ...interface{})                 { log.Print(v...) }
func (l *defaultLogger) Printf(format string, v ...interface{}) { log.Printf(format, v...) }
func (l *defaultLogger) Println(v ...interface{})               { log.Println(v...) }

// Logger for logging messages.
// Deprecated: Use ClusterConfig.Logger instead.
var Logger StdLogger = &defaultLogger{}

var nilInternalLogger internalLogger = loggerAdapter{
	legacyLogLevel: LogLevelNone,
	advLogger:      nopLogger{},
	legacyLogger:   nil,
}

type LogLevel int

const (
	LogLevelDebug = LogLevel(5)
	LogLevelInfo  = LogLevel(4)
	LogLevelWarn  = LogLevel(3)
	LogLevelError = LogLevel(2)
	LogLevelNone  = LogLevel(0)
)

func (recv LogLevel) String() string {
	switch recv {
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	case LogLevelNone:
		return "none"
	default:
		// fmt.sprintf allocates so use strings.Join instead
		temp := [2]string{"invalid level ", strconv.Itoa(int(recv))}
		return strings.Join(temp[:], "")
	}
}

type LogField struct {
	Name  string
	Value interface{}
}

func NewLogField(name string, value interface{}) LogField {
	return LogField{
		Name:  name,
		Value: value,
	}
}

type AdvancedLogger interface {
	Error(msg string, fields ...LogField)
	Warning(msg string, fields ...LogField)
	Info(msg string, fields ...LogField)
	Debug(msg string, fields ...LogField)
}

type internalLogger interface {
	AdvancedLogger
}

type loggerAdapter struct {
	legacyLogLevel LogLevel
	advLogger      AdvancedLogger
	legacyLogger   StdLogger
}

func (recv loggerAdapter) logLegacy(msg string, fields ...LogField) {
	var values []interface{}
	var small [5]interface{}
	l := len(fields)
	if l <= 5 { // small stack array optimization
		values = small[:l]
	} else {
		values = make([]interface{}, l)
	}
	var i int
	for _, v := range fields {
		values[i] = v.Value
		i++
	}
	recv.legacyLogger.Printf(strings.Join([]string{"gocql: ", msg, "\n"}, ""), values...)
}

func (recv loggerAdapter) Error(msg string, fields ...LogField) {
	if recv.advLogger != nil {
		recv.advLogger.Error(msg, fields...)
	} else if LogLevelError <= recv.legacyLogLevel {
		recv.logLegacy(msg, fields...)
	}
}

func (recv loggerAdapter) Warning(msg string, fields ...LogField) {
	if recv.advLogger != nil {
		recv.advLogger.Warning(msg, fields...)
	} else if LogLevelWarn <= recv.legacyLogLevel {
		recv.logLegacy(msg, fields...)
	}
}

func (recv loggerAdapter) Info(msg string, fields ...LogField) {
	if recv.advLogger != nil {
		recv.advLogger.Info(msg, fields...)
	} else if LogLevelInfo <= recv.legacyLogLevel {
		recv.logLegacy(msg, fields...)
	}
}

func (recv loggerAdapter) Debug(msg string, fields ...LogField) {
	if recv.advLogger != nil {
		recv.advLogger.Debug(msg, fields...)
	} else if LogLevelDebug <= recv.legacyLogLevel {
		recv.logLegacy(msg, fields...)
	}
}

func newInternalLoggerFromAdvancedLogger(logger AdvancedLogger) loggerAdapter {
	return loggerAdapter{
		advLogger:    logger,
		legacyLogger: nil,
	}
}

func newInternalLoggerFromStdLogger(logger StdLogger, level LogLevel) loggerAdapter {
	return loggerAdapter{
		legacyLogLevel: level,
		advLogger:      nil,
		legacyLogger:   logger,
	}
}
