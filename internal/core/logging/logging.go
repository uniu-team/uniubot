// Package logging is the process wide logger.
// Level comes from LOG_LEVEL, then config, then INFO; the last 2000 entries are
// retained for the dashboard API and the SSE stream.
package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Level string

// Supported levels. SUCCESS weighs the same as INFO.
const (
	LevelTrace   Level = "TRACE"
	LevelDebug   Level = "DEBUG"
	LevelInfo    Level = "INFO"
	LevelSuccess Level = "SUCCESS"
	LevelWarn    Level = "WARN"
	LevelError   Level = "ERROR"
)

var levelWeight = map[Level]int{
	LevelTrace:   0,
	LevelDebug:   10,
	LevelInfo:    20,
	LevelSuccess: 20,
	LevelWarn:    30,
	LevelError:   40,
}

var levelColor = map[Level]string{
	LevelTrace:   "\x1b[90m",
	LevelDebug:   "\x1b[36m",
	LevelInfo:    "\x1b[37m",
	LevelSuccess: "\x1b[92m",
	LevelWarn:    "\x1b[33m",
	LevelError:   "\x1b[31m",
}

const (
	BufferSize = 2000
	// ConfigurableLevels are the levels the dashboard accepts.
	colorReset = "\x1b[0m"
	green      = "\x1b[32m"
	yellow     = "\x1b[33m"
)

var ConfigurableLevels = []Level{LevelTrace, LevelDebug, LevelInfo, LevelWarn, LevelError}

type Entry struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Filename  string `json:"filename"`
	Func      string `json:"func"`
	Line      int    `json:"line"`
}

// Subscriber must not block: slow consumers must buffer internally.
type Subscriber func(Entry)

// Logger is safe for concurrent use.
type Logger struct {
	mu        sync.Mutex
	level     Level
	buffer    []Entry
	nextID    int64
	subs      map[int]Subscriber
	nextSubID int
	console   bool
	// parent is set for prefix views; all state lives on the root.
	parent *Logger
	prefix string
}

var defaultLogger = New(LevelInfo)

func New(level Level) *Logger {
	if _, ok := levelWeight[level]; !ok {
		level = LevelInfo
	}
	return &Logger{
		level:   level,
		buffer:  make([]Entry, 0, 256),
		subs:    make(map[int]Subscriber),
		console: true,
	}
}

func Default() *Logger { return defaultLogger }

func (l *Logger) SetConsole(enabled bool) {
	l = l.root()
	l.mu.Lock()
	l.console = enabled
	l.mu.Unlock()
}

func (l *Logger) SetPrefix(prefix string) {
	if l.parent != nil {
		l.parent.SetPrefix(prefix)
		return
	}
	l.mu.Lock()
	l.prefix = prefix
	l.mu.Unlock()
}

// WithPrefix prefixes console output only; buffer and subscribers are shared.
func (l *Logger) WithPrefix(prefix string) *Logger {
	r := l.root()
	return &Logger{parent: r, prefix: prefix}
}

func (l *Logger) root() *Logger {
	for l.parent != nil {
		l = l.parent
	}
	return l
}

func (l *Logger) Level() Level {
	l = l.root()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

func (l *Logger) SetLevel(level Level) error {
	normalized := Level(strings.ToUpper(string(level)))
	if _, ok := levelWeight[normalized]; !ok || normalized == LevelSuccess {
		return fmt.Errorf("invalid log level %q. Expected one of: TRACE, DEBUG, INFO, WARN, ERROR", level)
	}
	l = l.root()
	l.mu.Lock()
	l.level = normalized
	l.mu.Unlock()
	return nil
}

func ParseLevel(raw string) Level {
	level := Level(strings.ToUpper(strings.TrimSpace(raw)))
	if _, ok := levelWeight[level]; !ok || level == LevelSuccess {
		return LevelInfo
	}
	return level
}

func (l *Logger) IsLevelEnabled(level Level) bool {
	w, ok := levelWeight[level]
	if !ok {
		return false
	}
	l = l.root()
	l.mu.Lock()
	current := l.level
	l.mu.Unlock()
	return w >= levelWeight[current]
}

func (l *Logger) log(level Level, skip int, format string, args ...any) {
	if !l.IsLevelEnabled(level) {
		return
	}
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	filename, funcName, line := caller(skip + 2)
	entry := Entry{
		Timestamp: time.Now().UTC().Format("2006-01-02 15:04:05"),
		Level:     string(level),
		Message:   message,
		Filename:  filename,
		Func:      funcName,
		Line:      line,
	}

	prefix := l.prefix
	l = l.root()
	l.mu.Lock()
	l.nextID++
	entry.ID = l.nextID
	l.buffer = append(l.buffer, entry)
	if len(l.buffer) > BufferSize {
		l.buffer = l.buffer[len(l.buffer)-BufferSize:]
	}
	subs := make([]Subscriber, 0, len(l.subs))
	for _, sub := range l.subs {
		subs = append(subs, sub)
	}
	console := l.console
	l.mu.Unlock()

	for _, sub := range subs {
		sub(entry)
	}

	if console {
		colour := levelColor[level]
		if colour == "" {
			colour = colorReset
		}
		line := fmt.Sprintf("%s[%s]%s %s[%s]%s %s[%s:%s:%d]%s: %s%s%s",
			green, entry.Timestamp, colorReset,
			colour, entry.Level, colorReset,
			yellow, entry.Filename, entry.Func, entry.Line, colorReset,
			colour, message, colorReset)
		if prefix != "" {
			line = prefix + " " + line
		}
		fmt.Println(line)
	}
}

func caller(skip int) (string, string, int) {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown", "unknown", 0
	}
	fn := runtime.FuncForPC(pc)
	name := "anonymous"
	if fn != nil {
		name = fn.Name()
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
	}
	return filepath.Base(file), name, line
}

func (l *Logger) Trace(format string, args ...any) { l.log(LevelTrace, 1, format, args...) }

func (l *Logger) Debug(format string, args ...any) { l.log(LevelDebug, 1, format, args...) }

func (l *Logger) Info(format string, args ...any) { l.log(LevelInfo, 1, format, args...) }

func (l *Logger) Success(format string, args ...any) { l.log(LevelSuccess, 1, format, args...) }

func (l *Logger) Warn(format string, args ...any) { l.log(LevelWarn, 1, format, args...) }

func (l *Logger) Error(format string, args ...any) { l.log(LevelError, 1, format, args...) }

func (l *Logger) Subscribe(sub Subscriber) func() {
	l = l.root()
	l.mu.Lock()
	id := l.nextSubID
	l.nextSubID++
	l.subs[id] = sub
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}

func (l *Logger) Buffer() []Entry {
	l = l.root()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.buffer))
	copy(out, l.buffer)
	return out
}

// BufferAfter returns entries after afterID, filtered by level and a
// case-insensitive search string.
func (l *Logger) BufferAfter(afterID int64, level string, search string) []Entry {
	l = l.root()
	l.mu.Lock()
	entries := l.buffer
	snapshot := make([]Entry, len(entries))
	copy(snapshot, entries)
	l.mu.Unlock()

	if afterID > 0 {
		idx := -1
		for i, entry := range snapshot {
			if entry.ID > afterID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil
		}
		snapshot = snapshot[idx:]
	}
	if level != "" && level != "ALL" {
		filtered := snapshot[:0]
		for _, entry := range snapshot {
			if entry.Level == level {
				filtered = append(filtered, entry)
			}
		}
		snapshot = filtered
	}
	if search != "" {
		lower := strings.ToLower(search)
		filtered := snapshot[:0]
		for _, entry := range snapshot {
			if strings.Contains(strings.ToLower(entry.Message), lower) ||
				strings.Contains(strings.ToLower(entry.Filename), lower) ||
				strings.Contains(strings.ToLower(entry.Func), lower) {
				filtered = append(filtered, entry)
			}
		}
		snapshot = filtered
	}
	return snapshot
}

func (l *Logger) ApplyConfigLevel(configLevel string) {
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		_ = l.SetLevel(ParseLevel(raw))
		return
	}
	if configLevel != "" {
		_ = l.SetLevel(ParseLevel(configLevel))
	}
}

func EntryFromJSON(data []byte) (Entry, error) {
	var entry Entry
	err := json.Unmarshal(data, &entry)
	return entry, err
}

func (e Entry) String() string {
	return e.Timestamp + " [" + e.Level + "] " + e.Filename + ":" + strconv.Itoa(e.Line) + ": " + e.Message
}
