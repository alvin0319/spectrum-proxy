package main

import (
	"fmt"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
)

// ResourcePackServer handles serving resource packs over HTTP
type ResourcePackServer struct {
	// packs is a map of UUID -> resource pack
	packs     map[string]*resource.Pack
	packMutex sync.RWMutex
	// contentCache is a map of UUID -> cached content
	contentCache      map[string][]byte
	contentCacheMutex sync.RWMutex
	// basePath is the path to the resource packs directory
	basePath string
	// logger for the server
	logger *slog.Logger
	// server is the HTTP server
	server *http.Server
	// ready is a channel that signals when the server is ready
	ready chan struct{}
}

// NewResourcePackServer creates a new resource pack HTTP server
func NewResourcePackServer(packs []*resource.Pack, port int, logger *slog.Logger) (*ResourcePackServer, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	basePath := path.Join(wd, "resource_packs")

	// Create a map of UUID -> resource pack
	packMap := make(map[string]*resource.Pack)
	// Create content cache map
	contentCache := make(map[string][]byte)

	for _, pack := range packs {
		uuid := pack.UUID().String()
		packMap[uuid] = pack

		// Pre-load the resource pack content into memory
		content := make([]byte, pack.Len())
		_, err := pack.ReadAt(content, 0)
		if err != nil {
			logger.Error("Failed to cache resource pack", "uuid", uuid, "error", err)
			continue
		}
		contentCache[uuid] = content
		logger.Debug("Cached resource pack", "uuid", uuid, "size", len(content))
	}

	s := &ResourcePackServer{
		packs:             packMap,
		packMutex:         sync.RWMutex{},
		contentCache:      contentCache,
		contentCacheMutex: sync.RWMutex{},
		basePath:          basePath,
		logger:            logger,
		server: &http.Server{
			Addr: fmt.Sprintf(":%d", port),
		},
		ready: make(chan struct{}),
	}

	// Set up HTTP handler
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRequest)
	s.server.Handler = mux

	return s, nil
}

// Start starts the HTTP server
func (s *ResourcePackServer) Start() error {
	s.logger.Info("Starting resource pack HTTP server", "address", s.server.Addr)

	// Start a listener to check if we can bind to the port
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}

	// Signal that the server is ready to accept connections
	close(s.ready)

	// Use the listener with the HTTP server
	return s.server.Serve(listener)
}

// WaitForReady waits for the server to be ready to accept connections
func (s *ResourcePackServer) WaitForReady() {
	<-s.ready
}

// Close shuts down the HTTP server
func (s *ResourcePackServer) Close() error {
	return s.server.Close()
}

// UpdatePacks updates the resource packs in the server
func (s *ResourcePackServer) UpdatePacks(packs []*resource.Pack) {
	s.packMutex.Lock()
	defer s.packMutex.Unlock()

	// Create a new map of UUID -> resource pack
	packMap := make(map[string]*resource.Pack)
	for _, pack := range packs {
		packMap[pack.UUID().String()] = pack
	}
	s.packs = packMap
}

// handleRequest handles HTTP requests for resource packs
func (s *ResourcePackServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Get the UUID from the path
	path := strings.TrimPrefix(r.URL.Path, "/")

	s.logger.Debug("Received request", "path", path)

	// If the path is empty, return 404
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Sanitize the UUID to prevent path traversal
	if strings.Contains(path, "..") || strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}

	s.packMutex.RLock()
	pack, ok := s.packs[path]
	s.packMutex.RUnlock()

	if !ok {
		s.logger.Debug("Resource pack not found", "uuid", path)
		http.NotFound(w, r)
		return
	}

	s.contentCacheMutex.RLock()
	content, ok := s.contentCache[path]
	s.contentCacheMutex.RUnlock()

	if !ok {
		s.logger.Debug("Resource pack not cached, reading from pack", "uuid", path)
		content = make([]byte, pack.Len())
		_, err := pack.ReadAt(content, 0)
		if err != nil {
			s.logger.Error("Failed to read resource pack", "uuid", path, "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Cache the content
		s.contentCacheMutex.Lock()
		s.contentCache[path] = content
		s.contentCacheMutex.Unlock()
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.mcpack", path))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))

	if _, err := w.Write(content); err != nil {
		s.logger.Error("Failed to write resource pack to response", "uuid", path, "error", err)
		return
	}

	s.logger.Debug("Served resource pack", "uuid", path, "size", len(content))
}

// ModifyResourcePackForCDN modifies resource packs to use HTTP URLs instead of direct content
func ModifyResourcePackForCDN(packs []*resource.Pack, baseURL string) []*resource.Pack {
	modifiedPacks := make([]*resource.Pack, len(packs))

	for i, pack := range packs {
		// Create URL based on the pack's UUID
		url := fmt.Sprintf("%s/%s", baseURL, pack.UUID().String())

		// Create a modified pack with the URL
		modifiedPack := resource.MustReadURL(url)

		modifiedPacks[i] = modifiedPack
	}

	return modifiedPacks
}
