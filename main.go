package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"oracode_project/oracode"
)

func main() {
	workspace := flag.String("workspace", ".", "Path to the target codebase")
	targetFile := flag.String("file", "", "Target file to refactor, relative to workspace")
	verify := flag.Bool("verify", false, "Run workspace validation after refactoring")
	dryRun := flag.Bool("dry-run", false, "Print planned edits without writing files")
	jsonOut := flag.Bool("json", false, "Print machine-readable JSON reports")

	// Scalpel flags
	serve := flag.Bool("serve", false, "Start the Scalpel semantic index server")
	port := flag.Int("port", 9002, "Port for Scalpel server")
	watch := flag.Bool("watch", false, "Enable incremental file watching")
	mcpMode := flag.Bool("mcp", false, "Start as an MCP server over stdio (for AI agent use)")
	effects := flag.Bool("effects", false, "Build effect modality graph during indexing (experimental)")
	allGraphs := flag.Bool("all-graphs", false, "Build all static graphs (SQL, Proto, Vue) during indexing")
	semantic := flag.Bool("semantic", false, "Run type-aware semantic pass (go/packages) to upgrade effect graph")

	// Logging flags
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "Log format: text|json")
	logFile := flag.String("log-file", "", "Optional log file path (tees stderr + file)")

	flag.Parse()

	// Initialise logger — must be the very first thing after flag.Parse().
	if err := oracode.InitLogger(oracode.LogConfig{
		Level:  *logLevel,
		Format: *logFormat,
		File:   *logFile,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer oracode.CloseLogger()

	oracode.Log.Info("oracode.start",
		"workspace", *workspace,
		"log_level", *logLevel,
		"log_format", *logFormat,
	)

	engine := oracode.NewEngine(*workspace)
	engine.DryRun = *dryRun

	if *mcpMode {
		// Suppress unused flag variables (used only by non-MCP paths)
		_ = *effects
		_ = *allGraphs
		_ = *semantic
		_ = *watch

		idx, err := oracode.NewIndex(engine.Security)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oracode: init index: %v\n", err)
			os.Exit(1)
		}
		mcp := oracode.NewMCPServer(idx)
		defer mcp.Close()
		// Fast bootstrap: no WalkDirectory, no Watch, no graphs.
		// Indexing happens lazily on the first tool call that needs it.
		// This reduces MCP server startup from ~10-30s to <100ms.
		if err := mcp.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "oracode mcp error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *serve {
		idx, err := oracode.NewIndex(engine.Security)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oracode: init index: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Building initial index...")
		if err := idx.WalkDirectory(); err != nil {
			fmt.Fprintf(os.Stderr, "oracode: walk: %v\n", err)
			os.Exit(1)
		}
		if *semantic {
			if err := idx.RunSemanticPass(); err != nil {
				fmt.Fprintf(os.Stderr, "oracode: semantic pass failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "oracode: semantic pass completed, effect graph upgraded\n")
			}
		}
		if *watch {
			if err := idx.Watch(); err != nil {
				fmt.Fprintf(os.Stderr, "oracode: watch: %v\n", err)
				os.Exit(1)
			}
		}
		server := oracode.NewServer(idx)
		if err := server.Start(*port); err != nil {
			fmt.Fprintf(os.Stderr, "oracode server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *targetFile != "" {
		report, err := engine.ProcessFile(*targetFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oracode: %v\n", err)
			os.Exit(1)
		}
		if *jsonOut {
			if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
				fmt.Fprintf(os.Stderr, "oracode: encode report: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Print(report.String())
		}
	}

	if *verify {
		ok, logs := engine.VerifyWorkspace()
		if !ok {
			fmt.Fprintf(os.Stderr, "%s\n", logs)
			os.Exit(1)
		}
		fmt.Println(logs)
	}
}
