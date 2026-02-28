package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func NewService(cfg Config, volumesPath string) *Service {
	volumes := strings.TrimSpace(volumesPath)
	if volumes == "" {
		volumes = strings.TrimSpace(cfg.SFTP.Directory)
	}
	if volumes == "" {
		volumes = defaultVolumesPath
	}
	crashPath := filepath.Join(filepath.Dir(volumes), "debug-bundles")

	return &Service{
		cfg:                  cfg,
		volumesPath:          volumes,
		crashPath:            crashPath,
		activeLog:            make(map[int]context.CancelFunc),
		activeStat:           make(map[int]context.CancelFunc),
		pendingAttach:        make(map[int]bool),
		logBuffers:           make(map[int]*LogBuffer),
		diskUsageCache:       make(map[int]DiskUsageCacheEntry),
		lastNotRunningNotice: make(map[int]time.Time),
		attachStdin:          make(map[int]*AttachedStream),
		metrics: ConnectorMetrics{
			StartTime: time.Now().UTC(),
		},
	}
}

func (s *Service) Start() error {
	bootInfo("ensuring docker network")
	if err := s.ensureDockerNetwork(); err != nil {
		return fmt.Errorf("ensure docker network: %w", err)
	}

	bootInfo("starting sftp subsystem")
	if err := s.startSFTPServer(); err != nil {
		return fmt.Errorf("start sftp: %w", err)
	}

	bootInfo("starting docker event monitor")
	go s.monitorDockerEvents()
	bootInfo("starting websocket connector loop")
	go s.runWSLoop()

	bootInfo("starting http api server")
	go s.startAPIServer()

	// Wait forever
	select {}
}

func (s *Service) startAPIServer() {
	port := s.cfg.API.Port
	if port <= 0 {
		port = defaultAPIPort
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.handleMetrics(w, r)
	})

	mux.HandleFunc("/api/servers/", func(w http.ResponseWriter, r *http.Request) {
		// Example target: /api/servers/123/files/read
		pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(pathParts) < 4 || pathParts[0] != "api" || pathParts[1] != "servers" || pathParts[3] != "files" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		serverIDRaw := pathParts[2]
		var serverID int
		fmt.Sscanf(serverIDRaw, "%d", &serverID)
		if serverID <= 0 {
			http.Error(w, "Invalid server ID", http.StatusBadRequest)
			return
		}

		if r.Method == http.MethodPost && len(pathParts) >= 5 && pathParts[4] == "read" {
			s.handleHTTPFileRead(w, r, serverID)
			return
		}

		http.Error(w, "Not found", http.StatusNotFound)
	})

	listenAddr := fmt.Sprintf(":%d", port)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		bootFatal("api server crashed: %v", err)
	}
}

func (s *Service) handleHTTPFileRead(w http.ResponseWriter, r *http.Request, serverID int) {
	authHeader := r.Header.Get("Authorization")
	expectedToken := "Bearer " + s.cfg.Connector.Token
	if authHeader != expectedToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var payload struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if strings.TrimSpace(payload.Path) == "" {
		http.Error(w, "Path is required", http.StatusBadRequest)
		return
	}

	absPath, err := safeServerPath(s.volumesPath, serverID, payload.Path)
	if err != nil {
		http.Error(w, "Forbidden path", http.StatusForbidden)
		return
	}

	stat, err := os.Stat(absPath)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if stat.IsDir() {
		http.Error(w, "Cannot download a directory", http.StatusBadRequest)
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		http.Error(w, "Failed to open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))

	filename := filepath.Base(absPath)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	io.Copy(w, f)
}

func (s *Service) sendJSON(v interface{}) error {
	s.wsConnMu.RLock()
	conn := s.wsConn
	s.wsConnMu.RUnlock()
	if conn == nil {
		return fmt.Errorf("websocket not connected")
	}

	s.wsWriteMu.Lock()
	defer s.wsWriteMu.Unlock()
	return conn.WriteJSON(v)
}

func (s *Service) sendError(message string, serverID ...int) {
	payload := map[string]interface{}{
		"type":    "error",
		"message": message,
	}
	if len(serverID) > 0 && serverID[0] > 0 {
		payload["serverId"] = serverID[0]
	}
	_ = s.sendJSON(payload)
}

func (s *Service) sendServerError(serverID int, message string) {
	s.sendError(message, serverID)
}

func (s *Service) sendConsoleOutput(serverID int, output string) {
	if strings.TrimSpace(output) == "" {
		return
	}
	s.appendLogBuffer(serverID, output)
	_ = s.sendJSON(map[string]interface{}{
		"type":     "console_output",
		"serverId": serverID,
		"output":   output,
	})
}

func (s *Service) marshalMessage(input map[string]interface{}, out interface{}) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
