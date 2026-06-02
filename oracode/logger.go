package oracode

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"
)

// Log is the package-level logger. Always writes to stderr.
// Call InitLogger before using any subsystem loggers.
var Log *slog.Logger

var (
	logMu   sync.Mutex
	logFile *os.File
)

// LogConfig controls logger behaviour at startup.
type LogConfig struct {
	Level  string // "debug" | "info" | "warn" | "error"
	Format string // "text" | "json"
	File   string // optional: absolute path to log file (tees stderr + file)
}

// InitLogger must be called once in main() before any other OraCode code runs.
// It is safe to call multiple times (replaces the previous logger).
func InitLogger(cfg LogConfig) error {
	logMu.Lock()
	defer logMu.Unlock()

	// Close previous log file if any.
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}

	level := parseLevel(cfg.Level)

	// MCP hard rule: stdout is JSON-RPC only. All logs go to stderr.
	var w io.Writer = os.Stderr

	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("oracode logger: cannot open log file %s: %w", cfg.File, err)
		}
		logFile = f
		w = io.MultiWriter(os.Stderr, f)
	}

	opts := &slog.HandlerOptions{
		Level: level,
		// Include source file:line only in debug mode to keep info logs tight.
		AddSource: level == slog.LevelDebug,
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	Log = slog.New(handler)
	return nil
}

// CloseLogger flushes and closes the log file if one is open.
// Call this in a defer in main().
func CloseLogger() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// parseLevel converts the string flag value to a slog.Level.
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ---------------------------------------------------------------------------
// Subsystem loggers
// Each subsystem adds a "sub" key so log lines are filterable.
// ---------------------------------------------------------------------------

func IndexerLog() *slog.Logger  { return Log.With("sub", "indexer") }
func EffectLog() *slog.Logger   { return Log.With("sub", "effect") }
func WatcherLog() *slog.Logger  { return Log.With("sub", "watcher") }
func MCPLog() *slog.Logger      { return Log.With("sub", "mcp") }
func RESTLog() *slog.Logger     { return Log.With("sub", "rest") }
func GraphLog() *slog.Logger    { return Log.With("sub", "graph") }
func ValidatorLog() *slog.Logger { return Log.With("sub", "validator") }

// ---------------------------------------------------------------------------
// Tool call instrumentation
// ---------------------------------------------------------------------------

// ToolSpan records the start of an MCP tool call and returns a done() function
// to call when the tool returns. The done() function logs duration and outcome.
//
// Usage:
//   done := ToolSpan(ctx, "scalpel_effect_modality", slog.String("symbol", name))
//   defer done(err)
func ToolSpan(ctx context.Context, tool string, args ...any) func(resultBytes int, err error) {
	start := time.Now()
	MCPLog().InfoContext(ctx, "tool.start",
		append([]any{"tool", tool}, args...)...)

	return func(resultBytes int, err error) {
		dur := time.Since(start)
		if err != nil {
			MCPLog().ErrorContext(ctx, "tool.error",
				"tool", tool,
				"duration_ms", dur.Milliseconds(),
				"error", err,
			)
			return
		}
		MCPLog().InfoContext(ctx, "tool.done",
			"tool", tool,
			"duration_ms", dur.Milliseconds(),
			"result_bytes", resultBytes,
		)
	}
}

// ---------------------------------------------------------------------------
// Graph build instrumentation
// ---------------------------------------------------------------------------

// GraphBuildSpan records the start of a graph build (effect, sql, proto, vue).
// Returns a done() function to call when the build completes.
//
// Usage:
//   done := GraphBuildSpan("effect_graph", 0)
//   defer done(newVersion, symbolCount, err)
func GraphBuildSpan(graphName string, prevVersion int) func(newVersion, symbols int, err error) {
	start := time.Now()
	GraphLog().Info("graph.build.start",
		"graph", graphName,
		"prev_version", prevVersion,
	)
	return func(newVersion, symbols int, err error) {
		dur := time.Since(start)
		if err != nil {
			GraphLog().Error("graph.build.error",
				"graph", graphName,
				"duration_ms", dur.Milliseconds(),
				"error", err,
			)
			return
		}
		GraphLog().Info("graph.build.done",
			"graph", graphName,
			"version", newVersion,
			"symbols", symbols,
			"duration_ms", dur.Milliseconds(),
		)
	}
}

// ---------------------------------------------------------------------------
// Reindex instrumentation
// ---------------------------------------------------------------------------

// ReindexSpan records the start of a reindex (full or partial).
//
// Usage:
//   done := ReindexSpan("full", "")
//   done := ReindexSpan("partial", "internal/inventory/service.go")
//   defer done(filesIndexed, err)
func ReindexSpan(kind, file string) func(files int, err error) {
	start := time.Now()
	args := []any{"kind", kind}
	if file != "" {
		args = append(args, "file", file)
	}
	IndexerLog().Info("reindex.start", args...)

	return func(files int, err error) {
		dur := time.Since(start)
		if err != nil {
			IndexerLog().Error("reindex.error",
				append(args, "duration_ms", dur.Milliseconds(), "error", err)...)
			return
		}
		IndexerLog().Info("reindex.done",
			append(args, "files", files, "duration_ms", dur.Milliseconds())...)
	}
}

// ---------------------------------------------------------------------------
// Memory snapshot (debug builds / explicit calls)
// ---------------------------------------------------------------------------

// LogMemory logs current heap allocation. Call at graph build completion
// or after full reindex to verify the 100MB ceiling is respected.
func LogMemory(label string) {
	if Log == nil {
		return
	}
	if Log.Enabled(context.Background(), slog.LevelDebug) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		Log.LogAttrs(context.Background(), slog.LevelDebug, "memory.snapshot",
			slog.String("label", label),
			slog.Uint64("heap_alloc_mb", m.HeapAlloc/1024/1024),
			slog.Uint64("heap_sys_mb", m.HeapSys/1024/1024),
			slog.Int("goroutines", runtime.NumGoroutine()),
		)
	}
}

// ---------------------------------------------------------------------------
// Graph cache hit/miss logging
// ---------------------------------------------------------------------------

// LogCacheHit logs a graph cache hit (graph served from memory, not disk).
func LogCacheHit(graphName string) {
	log := GraphLog()
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.LogAttrs(context.Background(), slog.LevelDebug, "graph.cache.hit",
			slog.String("graph", graphName),
		)
	}
}

// LogCacheMiss logs a graph cache miss (graph loaded from disk).
func LogCacheMiss(graphName, reason string) {
	log := GraphLog()
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.LogAttrs(context.Background(), slog.LevelDebug, "graph.cache.miss",
			slog.String("graph", graphName),
			slog.String("reason", reason),
		)
	}
}

// LogCacheInvalidated logs when the graph cache is cleared due to file changes.
func LogCacheInvalidated(graphName, trigger string) {
	log := GraphLog()
	if log.Enabled(context.Background(), slog.LevelInfo) {
		log.LogAttrs(context.Background(), slog.LevelInfo, "graph.cache.invalidated",
			slog.String("graph", graphName),
			slog.String("trigger", trigger),
		)
	}
}

// ---------------------------------------------------------------------------
// Constraint validation logging
// ---------------------------------------------------------------------------

// LogConstraintPass logs a successful constraint check.
func LogConstraintPass(constraintID, symbol string) {
	log := ValidatorLog()
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.LogAttrs(context.Background(), slog.LevelDebug, "constraint.pass",
			slog.String("id", constraintID),
			slog.String("symbol", symbol),
		)
	}
}

// LogConstraintFail logs a constraint violation.
func LogConstraintFail(constraintID, symbol, message string) {
	log := ValidatorLog()
	if log.Enabled(context.Background(), slog.LevelWarn) {
		log.LogAttrs(context.Background(), slog.LevelWarn, "constraint.fail",
			slog.String("id", constraintID),
			slog.String("symbol", symbol),
			slog.String("message", message),
		)
	}
}

// ---------------------------------------------------------------------------
// Stale graph warning
// ---------------------------------------------------------------------------

// LogStaleGraph warns when a tool query detects the graph may be stale.
func LogStaleGraph(graphName string, graphVersion int, lastChange time.Time) {
	log := GraphLog()
	if log.Enabled(context.Background(), slog.LevelWarn) {
		log.LogAttrs(context.Background(), slog.LevelWarn, "graph.stale",
			slog.String("graph", graphName),
			slog.Int("version", graphVersion),
			slog.String("last_file_change", lastChange.Format(time.RFC3339)),
		)
	}
}
