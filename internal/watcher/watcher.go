package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounceDuration = 200 * time.Millisecond

// EventType describes the kind of filesystem change observed.
type EventType string

const (
	EventCreate EventType = "create"
	EventModify EventType = "modify"
	EventDelete EventType = "delete"
	EventRename EventType = "rename"
)

// Event describes a single filesystem change.
type Event struct {
	Type  EventType
	Path  string
	IsDir bool
}

// Watcher watches a directory tree recursively for filesystem changes.
type Watcher struct {
	rootDir  string
	onChange func(Event)
	fsw      *fsnotify.Watcher

	mu      sync.Mutex
	timers  map[string]*time.Timer
	stopped chan struct{}
}

// New creates a Watcher that calls onChange for each debounced filesystem event.
func New(rootDir string, onChange func(Event)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: create fsnotify watcher: %w", err)
	}

	return &Watcher{
		rootDir:  rootDir,
		onChange: onChange,
		fsw:      fsw,
		timers:   make(map[string]*time.Timer),
		stopped:  make(chan struct{}),
	}, nil
}

// Start walks the root directory and begins watching all subdirectories.
func (w *Watcher) Start() error {
	// Walk and register all existing directories.
	if err := filepath.WalkDir(w.rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.fsw.Add(path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("watcher: walk %s: %w", w.rootDir, err)
	}

	go w.eventLoop()
	return nil
}

// Stop shuts down the watcher and releases resources.
func (w *Watcher) Stop() {
	close(w.stopped)
	w.fsw.Close()
}

// eventLoop receives raw fsnotify events, debounces them, and calls onChange.
func (w *Watcher) eventLoop() {
	for {
		select {
		case <-w.stopped:
			return

		case fsEvent, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.scheduleDebounced(fsEvent)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			_ = err // Non-fatal watcher errors are ignored.
		}
	}
}

// scheduleDebounced deduplicates events for the same path within debounceDuration.
func (w *Watcher) scheduleDebounced(fsEvent fsnotify.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()

	path := fsEvent.Name
	evType := fsnotifyToEventType(fsEvent.Op)

	if t, ok := w.timers[path]; ok {
		t.Stop()
	}

	w.timers[path] = time.AfterFunc(debounceDuration, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		info, err := os.Stat(path)
		isDir := err == nil && info.IsDir()

		// If a new directory was created, start watching it.
		if isDir && (evType == EventCreate) {
			_ = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr == nil && d.IsDir() {
					_ = w.fsw.Add(p)
				}
				return nil
			})
		}

		w.onChange(Event{
			Type:  evType,
			Path:  path,
			IsDir: isDir,
		})
	})
}

// fsnotifyToEventType maps an fsnotify Op to our EventType.
func fsnotifyToEventType(op fsnotify.Op) EventType {
	switch {
	case op&fsnotify.Create != 0:
		return EventCreate
	case op&fsnotify.Write != 0:
		return EventModify
	case op&fsnotify.Remove != 0:
		return EventDelete
	case op&fsnotify.Rename != 0:
		return EventRename
	default:
		return EventModify
	}
}
