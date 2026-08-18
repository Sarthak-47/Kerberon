package config

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Watcher reloads a config file on demand and reports the result.
//
// It watches by polling the file's modification time rather than using
// fsnotify. A dependency is not free — this project's whole proposition is a
// single binary with nothing behind it — and a poll every couple of seconds is
// indistinguishable in practice from an inotify event for a file a human edits
// by hand. SIGHUP remains the explicit path for anyone who wants it immediate.
type Watcher struct {
	path     string
	current  atomic.Pointer[Config]
	lastMod  time.Time
	lastSize int64
	interval time.Duration
	log      *slog.Logger

	// OnReload is called with the new config after a successful swap.
	OnReload func(*Config)
}

// PollInterval is how often the file is checked when watching.
const PollInterval = 2 * time.Second

// NewWatcher loads the file once and returns a Watcher holding it.
func NewWatcher(path string, log *slog.Logger) (*Watcher, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	w := &Watcher{path: path, interval: PollInterval, log: log}
	w.current.Store(cfg)
	if st, err := os.Stat(path); err == nil {
		w.lastMod, w.lastSize = st.ModTime(), st.Size()
	}
	return w, nil
}

// Current returns the configuration in force.
//
// The pointer is swapped atomically, so a caller reading it mid-reload gets
// either the old config or the new one, never a half-applied mixture.
func (w *Watcher) Current() *Config { return w.current.Load() }

// Reload re-reads and validates the file, swapping it in only if it is good.
//
// An invalid config never replaces a valid running one. Kerberon is on the
// critical path for incident response, and a typo taking paging offline would
// be a far worse outcome than running briefly on a stale config.
func (w *Watcher) Reload() error {
	cfg, err := Load(w.path)
	if err != nil {
		w.log.Error("config reload rejected; keeping the running configuration",
			"path", w.path, "error", err)
		return err
	}

	w.current.Store(cfg)
	w.log.Info("configuration reloaded",
		"path", w.path,
		"users", len(cfg.Users), "teams", len(cfg.Teams),
		"schedules", len(cfg.Schedules), "routes", len(cfg.Routes))

	if w.OnReload != nil {
		w.OnReload(cfg)
	}
	return nil
}

// Watch polls for changes until ctx is cancelled.
//
// reloadSignal is typically fed by SIGHUP. Both paths converge on Reload, so a
// bad file is rejected identically however the reload was asked for.
func (w *Watcher) Watch(ctx context.Context, reloadSignal <-chan os.Signal) error {
	ticker := time.NewTicker(w.interval) //kerberon:allow-clock -- polls a real file's mtime; a fake clock cannot make the filesystem change
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-reloadSignal:
			w.log.Info("reload requested by signal", "path", w.path)
			_ = w.Reload() // already logged; a bad file must not stop the watcher

		case <-ticker.C:
			st, err := os.Stat(w.path)
			if err != nil {
				// The file being briefly absent is normal: many editors write
				// by rename. Saying nothing avoids a log full of noise every
				// time somebody saves.
				continue
			}
			if st.ModTime().Equal(w.lastMod) && st.Size() == w.lastSize {
				continue
			}
			w.lastMod, w.lastSize = st.ModTime(), st.Size()
			w.log.Info("configuration file changed on disk", "path", w.path)
			_ = w.Reload()
		}
	}
}
