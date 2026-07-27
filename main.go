package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "2026-07-28.2"

type server struct {
	root   string
	osRoot *os.Root
	token  string
	qb     *qbit

	locks *lockTable

	streams *streamRegistry

	deleteMu       sync.Mutex
	deleteBatches  map[string]*deleteBatch
	deleteDebounce time.Duration

	operationTimeout  time.Duration
	pollInterval      time.Duration
	statePollInterval time.Duration
	enableReserve     int64
	availableBytes    func(string) (int64, error)

	viewed   *viewedStore
	playback playbackConfig

	torrentDir  string
	allowURLAdd bool

	events     *eventHub
	operations *operations
	statePoke  chan struct{}
	catalog    atomic.Pointer[catalog]

	draining atomic.Bool
	inflight sync.WaitGroup
	probe    qbitProbe
	initOnce sync.Once
}

func (s *server) ensureRuntime() {
	s.initOnce.Do(func() {
		if s.locks == nil {
			s.locks = newLockTable()
		}
		if s.streams == nil {
			s.streams = newStreamRegistry()
		}
		if s.events == nil {
			s.events = newEventHub()
		}
		if s.operations == nil {
			s.operations = newOperations(0)
		}
		if s.statePoke == nil {
			s.statePoke = make(chan struct{}, 1)
		}
		if s.statePollInterval <= 0 {
			s.statePollInterval = 2 * time.Second
		}
		if s.osRoot == nil && s.root != "" {
			if r, err := os.OpenRoot(s.root); err == nil {
				s.osRoot = r
			} else {
				log.Printf("open root %q: %v", s.root, err)
			}
		}
	})
}

func (s *server) lockTorrent(hash string) func() {
	s.ensureRuntime()
	return s.locks.lock(hash)
}

func (s *server) streamReg() *streamRegistry {
	s.ensureRuntime()
	return s.streams
}

func (s *server) fsRoot() *os.Root {
	s.ensureRuntime()
	return s.osRoot
}

func (s *server) ops() *operations {
	s.ensureRuntime()
	return s.operations
}

func (s *server) hub() *eventHub {
	s.ensureRuntime()
	return s.events
}

func (s *server) currentCatalog() *catalog {
	if cat := s.catalog.Load(); cat != nil {
		return cat
	}
	cat := s.buildCatalog()
	s.catalog.Store(cat)
	return cat
}

func (s *server) lookupItem(id string) (*catalog, *catalogItem) {
	cat := s.currentCatalog()
	if item := cat.item(id); item != nil {
		return cat, item
	}
	cat = s.buildCatalog()
	s.catalog.Store(cat)
	return cat, cat.item(id)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, target *time.Duration) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("%s must be a positive duration", key)
	}
	*target = d
}

func main() {
	root, err := filepath.EvalSymlinks(envOr("TORRMEDIA_ROOT", "/var/media/library"))
	if err != nil {
		log.Fatalf("TORRMEDIA_ROOT: %v", err)
	}
	root = strings.TrimSuffix(root, "/")
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		log.Fatalf("TORRMEDIA_ROOT: %v", err)
	}
	defer osRoot.Close()

	s := &server{
		root:              root,
		osRoot:            osRoot,
		token:             os.Getenv("TORRMEDIA_TOKEN"),
		deleteDebounce:    30 * time.Second,
		operationTimeout:  5 * time.Minute,
		pollInterval:      250 * time.Millisecond,
		statePollInterval: 2 * time.Second,
		enableReserve:     512 << 20,
		availableBytes:    availableFilesystemBytes,
		playback:          defaultPlaybackConfig(),
		torrentDir:        envOr("TORRMEDIA_TORRENT_DIR", "/etc/torrmedia/torrents"),
		allowURLAdd:       os.Getenv("TORRMEDIA_ALLOW_URL_ADD") == "true",
	}
	if err := parsePlaybackConfig(&s.playback); err != nil {
		log.Fatal(err)
	}
	s.viewed, err = newViewedStore(envOr("TORRMEDIA_VIEWED_FILE", "/var/lib/torrmedia/viewed.json"))
	if err != nil {
		log.Fatalf("TORRMEDIA_VIEWED_FILE: %v", err)
	}
	envDuration("TORRMEDIA_OPERATION_TIMEOUT", &s.operationTimeout)
	envDuration("TORRMEDIA_DELETE_DEBOUNCE", &s.deleteDebounce)
	envDuration("TORRMEDIA_STATE_POLL_INTERVAL", &s.statePollInterval)

	if qu := os.Getenv("QBIT_URL"); qu != "" {
		if s.qb, err = newQbit(qu, os.Getenv("QBIT_USER"), os.Getenv("QBIT_PASS")); err != nil {
			log.Fatalf("QBIT_URL: %v", err)
		}
	}
	listen := envOr("TORRMEDIA_LISTEN", ":8091")
	if s.token == "" {
		log.Printf("WARNING: TORRMEDIA_TOKEN is not set — the API is open to anyone who can reach %s. "+
			"Set TORRMEDIA_TOKEN or keep the port behind a firewall/VPN (see README).", listen)
	}

	s.ensureRuntime()
	s.operations.notify = s.publishOperation

	srv := &http.Server{
		Addr:              listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("torrmedia %s (contract %d): listen=%s root=%s qbit=%t",
		version, contractVersion, listen, s.root, s.qb != nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pumpDone := make(chan struct{})
	go s.runStatePump(pumpDone)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		close(pumpDone)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		stop()
		log.Printf("torrmedia: shutdown requested, draining")
		s.draining.Store(true)
		s.cancelAllQueuedDeletes()

		done := make(chan struct{})
		go func() { s.inflight.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(s.operationTimeout + 30*time.Second):
			log.Printf("torrmedia: in-flight operations did not finish in time")
		}
		close(pumpDone)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("torrmedia: shutdown: %v", err)
		}
		log.Printf("torrmedia: stopped cleanly")
	}
}
