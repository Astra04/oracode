// watcher_logging.go
// Shows how to instrument watcher.go event handling with logging.
// Merge these patterns into your existing watcher.go.

package oracode

import (
	"strings"
	"time"
)

// loggedFileChange wraps the existing file change handler to add logging.
// In your watcher.go, replace the raw handler call with this pattern.
func (idx *Index) loggedFileChange(path, event string) {
	WatcherLog().Info("file.changed",
		"path", path,
		"event", event,
	)

	// If the changed file affects a graph, invalidate its cache and log.
	switch {
	case isGoFile(path):
		WatcherLog().Info("graph.invalidated",
			"graph", "effect_graph",
			"trigger", path,
		)
		// Your existing invalidation + debounced reindex call here.

	case isVueFile(path):
		WatcherLog().Info("graph.invalidated",
			"graph", "vue_surface",
			"trigger", path,
		)

	case isSQLMigration(path):
		WatcherLog().Info("graph.invalidated",
			"graph", "sql_graph",
			"trigger", path,
		)

	case isProtoFile(path):
		WatcherLog().Info("graph.invalidated",
			"graph", "proto_graph",
			"trigger", path,
		)
	}
}

// loggedDebounceFlush wraps the debounce flush to log when a batch of changes
// triggers a reindex after the quiet period.
func loggedDebounceFlush(changedFiles []string, since time.Time) {
	WatcherLog().Info("debounce.flush",
		"files_changed", len(changedFiles),
		"elapsed_ms", time.Since(since).Milliseconds(),
	)
}

func isGoFile(path string) bool   { return len(path) > 3 && path[len(path)-3:] == ".go" }
func isVueFile(path string) bool  { return len(path) > 4 && path[len(path)-4:] == ".vue" }
func isProtoFile(path string) bool { return len(path) > 6 && path[len(path)-6:] == ".proto" }

// isSQLMigration identifies SQL migration files by path convention.
// Adjust the path prefix to match your migrations directory.
func isSQLMigration(path string) bool {
	return len(path) > 10 &&
		(strings.Contains(path, "migration") || strings.Contains(path, "migrate")) &&
		path[len(path)-4:] == ".sql"
}
