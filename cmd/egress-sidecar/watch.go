/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
)

// watchFileReload fsnotify-watches path's DIRECTORY and calls reload() on any change until stop is
// closed. It watches the directory, not the file, because a ConfigMap-projected mount updates the file
// via an atomic ..data symlink swap — a file-level watch would go deaf after the first update. Shared by
// the tool-policy and routes hot-reload watchers (M82 / J7): they differ only in the reload closure.
// what labels the logs. A watcher construction/add error is logged and the function returns — the value
// stays fixed at startup (a visible degradation, never a crash).
func watchFileReload(path string, log logr.Logger, stop <-chan struct{}, what string, reload func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Info("egress: "+what+" watch disabled (fsnotify init failed) — fixed at startup", "err", err.Error())
		return
	}
	defer func() { _ = w.Close() }()

	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		log.Info("egress: "+what+" watch disabled (watch add failed) — fixed at startup", "dir", dir, "err", err.Error())
		return
	}

	for {
		select {
		case <-stop:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				reload()
			}
		case werr, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Info("egress: "+what+" watch error (keeping last-good)", "err", werr.Error())
		}
	}
}
