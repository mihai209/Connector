package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	httpReadTimeout       = 15 * time.Second
	httpReadHeaderTimeout = 10 * time.Second
	httpWriteTimeout      = 60 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
	httpMaxBodyBytes      = int64(1 << 20)
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
		commandRate:          make(map[int]CommandRateState),
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

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot := s.metricsSnapshot()
		uptimeSeconds := time.Since(snapshot.StartTime).Seconds()
		if uptimeSeconds < 0 {
			uptimeSeconds = 0
		}

		payload := map[string]interface{}{
			"ok":             true,
			"ws_connected":   snapshot.WSConnected,
			"uptime_seconds": int64(uptimeSeconds),
			"time":           time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		snapshot := s.metricsSnapshot()
		ready := snapshot.WSConnected
		statusCode := http.StatusOK
		if !ready {
			statusCode = http.StatusServiceUnavailable
		}

		payload := map[string]interface{}{
			"ready":        ready,
			"ws_connected": snapshot.WSConnected,
			"time":         time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(payload)
	})

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

		serverID, parseErr := strconv.Atoi(strings.TrimSpace(pathParts[2]))
		if parseErr != nil || serverID <= 0 {
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
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadTimeout:       httpReadTimeout,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		bootFatal("api server crashed: %v", err)
	}
}

func (s *Service) handleHTTPFileRead(w http.ResponseWriter, r *http.Request, serverID int) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	expectedToken := "Bearer " + strings.TrimSpace(s.cfg.Connector.Token)
	if expectedToken == "Bearer" || subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var payload struct {
		Path string `json:"path"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httpMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
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
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err := conn.WriteJSON(v)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
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

func (s *Service) allowServerCommand(serverID int) bool {
	if serverID <= 0 {
		return false
	}

	now := time.Now().UTC()
	s.commandRateMu.Lock()
	defer s.commandRateMu.Unlock()

	state := s.commandRate[serverID]
	if state.WindowStart.IsZero() || now.Sub(state.WindowStart) >= commandRateWindow {
		s.commandRate[serverID] = CommandRateState{
			WindowStart: now,
			Count:       1,
		}
		return true
	}

	if state.Count >= commandRateLimit {
		return false
	}

	state.Count++
	s.commandRate[serverID] = state
	return true
}
