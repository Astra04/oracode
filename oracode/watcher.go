package oracode

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

func (idx *Index) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	root := idx.Policy.WorkspaceRoot

	// Add all existing directories
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if idx.Policy.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return watcher.Add(path)
		}
		return nil
	})
	if err != nil {
		return err
	}

	WatcherLog().Info("watcher.started")

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// Re-index on Write or Create
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					info, err := os.Stat(event.Name)
					if err == nil && info.IsDir() {
						if !idx.Policy.ShouldSkipDir(info.Name()) {
							watcher.Add(event.Name)
						}
						continue
					}

					lang, supported := DetectOraLanguage(event.Name)
					if supported {
						rel, err := filepath.Rel(root, event.Name)
						if err == nil {
							WatcherLog().Info("file.changed", "path", rel, "event", event.Op.String())
							err := idx.IndexFile(rel, lang)
							if err == nil {
								_ = idx.RefreshRouteGraph()
								if lang == LanguageGo {
									idx.MarkEffectGraphDirty()
									WatcherLog().Info("graph.invalidated",
										"graph", "effect_graph",
										"trigger", rel,
									)
								}
								WatcherLog().Info("file.reindexed", "file", rel)
							} else {
								WatcherLog().Error("file.reindex_failed", "file", rel, "error", err)
							}
						}
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				WatcherLog().Error("watcher.error", "error", err)
			}
		}
	}()

	return nil
}
