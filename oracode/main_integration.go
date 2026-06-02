//go:build ignore
// This file is a merge guide only. Its contents have been applied to main.go.
// main.go — add these flags and init block.
// This shows only the logging-relevant additions.
// Merge into your existing main.go.

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/your-org/oracode/oracode"
)

func main() {
	// --- Existing flags (already in your main.go) ---
	workspace := flag.String("workspace", "", "Workspace root path")
	serve     := flag.Bool("serve", false, "Start REST server")
	mcp       := flag.Bool("mcp", false, "Start MCP stdio server")
	port      := flag.Int("port", 9002, "REST server port")
	watch     := flag.Bool("watch", false, "Enable file watcher")
	effects   := flag.Bool("effects", false, "Enable effect modality extraction")

	// --- New logging flags ---
	logLevel  := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "Log format: text|json")
	logFile   := flag.String("log-file", "", "Optional log file path (tees stderr + file)")

	flag.Parse()

	// --- Initialize logger FIRST, before anything else ---
	if err := oracode.InitLogger(oracode.LogConfig{
		Level:  *logLevel,
		Format: *logFormat,
		File:   *logFile,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer oracode.CloseLogger()

	// Log startup configuration.
	oracode.Log.Info("oracode.start",
		"workspace", *workspace,
		"mode", mode(*serve, *mcp),
		"effects", *effects,
		"log_level", *logLevel,
		"log_format", *logFormat,
	)

	// Memory snapshot at startup.
	oracode.LogMemory("startup")

	// --- Rest of your existing main() logic below ---
	// ...
}

// mode returns a string describing the startup mode for the startup log line.
func mode(serve, mcp bool) string {
	switch {
	case mcp:
		return "mcp"
	case serve:
		return "rest"
	default:
		return "cli"
	}
}
