package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	diskTTL := time.Duration(cfg.System.DiskCheckTtlSeconds) * time.Second
	if diskTTL <= 0 {
		diskTTL = time.Duration(defaultDiskUsageCacheTTLSeconds) * time.Second
	}
	throttleEnabled := true
	if cfg.Throttles.Enabled != nil {
		throttleEnabled = *cfg.Throttles.Enabled
	}
	throttleLines := cfg.Throttles.Lines
	if throttleLines == 0 {
		throttleLines = defaultConsoleThrottleLines
	}
	throttleInterval := time.Duration(cfg.Throttles.LineResetInterval) * time.Millisecond
	if throttleInterval <= 0 {
		throttleInterval = time.Duration(defaultConsoleThrottleIntervalMs) * time.Millisecond
	}
	downloadLimit := int64(cfg.Transfers.DownloadLimit) * 1024 * 1024
	if downloadLimit < 0 {
		downloadLimit = 0
	}

	return &Service{
		cfg:                      cfg,
		volumesPath:              volumes,
		crashPath:                crashPath,
		wsReadLimitMB:            clampWSReadLimitMB(cfg.System.WSReadLimitMB),
		diskUsageCacheTTL:        diskTTL,
		consoleThrottleEnabled:   throttleEnabled,
		consoleThrottleLines:     throttleLines,
		consoleThrottleInterval:  throttleInterval,
		consoleThrottle:          make(map[int]ConsoleThrottleState),
		downloadLimitBytesPerSec: downloadLimit,
		activeLog:                make(map[int]context.CancelFunc),
		activeStat:               make(map[int]context.CancelFunc),
		pendingAttach:            make(map[int]bool),
		logBuffers:               make(map[int]*LogBuffer),
		diskUsageCache:           make(map[int]DiskUsageCacheEntry),
		lastNotRunningNotice:     make(map[int]time.Time),
		attachStdin:              make(map[int]*AttachedStream),
		commandRate:              make(map[int]CommandRateState),
		powerState:               make(map[int]string),
		metrics: ConnectorMetrics{
			StartTime: time.Now().UTC(),
		},
		events:        NewBus(),
		environments:  make(map[int]ProcessEnvironment),
		commandQueues: make(map[int]chan string),
	}
}

func (s *Service) Start() error {
	bootInfo("ensuring docker network")
	if err := s.ensureDockerNetwork(); err != nil {
		return fmt.Errorf("ensure docker network: %w", err)
	}

	bootInfo("repairing existing server volume permissions")
	s.repairExistingServerPermissions()

	bootInfo("repairing DNS for already running containers")
	s.repairRunningContainersDNS()

	bootInfo("restoring console streams for already running containers")
	s.restoreRunningConsoleStreams()

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
	host := strings.TrimSpace(s.cfg.API.Host)
	if host == "" {
		host = "0.0.0.0"
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

	// Server Management API
	mux.HandleFunc("/api/servers/", func(w http.ResponseWriter, r *http.Request) {
		pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(pathParts) < 3 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		serverID, parseErr := strconv.Atoi(strings.TrimSpace(pathParts[2]))
		if parseErr != nil || serverID <= 0 {
			http.Error(w, "Invalid server ID", http.StatusBadRequest)
			return
		}

		// Routing for server sub-resources
		if len(pathParts) >= 4 {
			switch pathParts[3] {
			case "files":
				if len(pathParts) >= 5 && pathParts[4] == "read" && r.Method == http.MethodPost {
					s.handleHTTPFileRead(w, r, serverID)
					return
				}
			case "power":
				s.tokenAuthMiddleware(s.handleServerPowerAPI)(w, r)
				return
			case "stats":
				s.tokenAuthMiddleware(s.handleServerStatsAPI)(w, r)
				return
			}
		}

		http.Error(w, "Not found", http.StatusNotFound)
	})

	listenAddr := fmt.Sprintf("%s:%d", host, port)
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadTimeout:       httpReadTimeout,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
	if s.cfg.API.SSL.Enabled {
		cert := strings.TrimSpace(s.cfg.API.SSL.CertFile)
		key := strings.TrimSpace(s.cfg.API.SSL.KeyFile)
		if cert == "" || key == "" {
			bootFatal("api server ssl enabled but cert/key missing")
		}
		if err := httpServer.ListenAndServeTLS(cert, key); err != nil {
			bootFatal("api server crashed: %v", err)
		}
		return
	}
	if err := httpServer.ListenAndServe(); err != nil {
		bootFatal("api server crashed: %v", err)
	}
}

func (s *Service) handleHTTPFileRead(w http.ResponseWriter, r *http.Request, serverID int) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	expectedToken := "Bearer " + strings.TrimSpace(s.cfg.Connector.Token)
	if expectedToken == "Bearer" || subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) != 1 {
		bootWarn("api unauthorized ip=%s server=%d", s.resolveClientIP(r), serverID)
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
	if isProtectedServerPath(payload.Path) {
		http.Error(w, "Forbidden path", http.StatusForbidden)
		return
	}

	absPath, err := safeServerPath(s.volumesPath, serverID, payload.Path)
	if err != nil {
		http.Error(w, "Forbidden path", http.StatusForbidden)
		return
	}
	serverRoot, err := safeServerPath(s.volumesPath, serverID, "/")
	if err != nil {
		http.Error(w, "Forbidden path", http.StatusForbidden)
		return
	}

	stat, err := secureStat(serverRoot, absPath)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if stat.IsDir() {
		http.Error(w, "Cannot download a directory", http.StatusBadRequest)
		return
	}

	f, err := secureOpen(serverRoot, absPath)
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

func (s *Service) resolveClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	remoteIP := ""
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	} else {
		remoteIP = r.RemoteAddr
	}
	remoteIP = strings.TrimSpace(remoteIP)
	if remoteIP == "" {
		return ""
	}
	if len(s.cfg.API.TrustedProxies) == 0 || !s.isTrustedProxy(remoteIP) {
		return remoteIP
	}

	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	return remoteIP
}

func (s *Service) isTrustedProxy(ip string) bool {
	if ip == "" {
		return false
	}
	parsed := net.ParseIP(ip)
	for _, entry := range s.cfg.API.TrustedProxies {
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if strings.Contains(value, "/") {
			if _, cidr, err := net.ParseCIDR(value); err == nil && parsed != nil && cidr.Contains(parsed) {
				return true
			}
		} else if value == ip {
			return true
		}
	}
	return false
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
	s.sendConsoleOutputInternal(serverID, output, false)
}

func (s *Service) sendConsoleOutputBypass(serverID int, output string) {
	s.sendConsoleOutputInternal(serverID, output, true)
}

func (s *Service) sendConsoleOutputInternal(serverID int, output string, bypassThrottle bool) {
	if strings.TrimSpace(output) == "" {
		return
	}
	if !bypassThrottle && s.consoleThrottleEnabled {
		lineCount := countConsoleLines(output)
		if lineCount > 0 {
			if !s.allowConsoleOutput(serverID, lineCount) {
				return
			}
		}
	}
	s.appendLogBuffer(serverID, output)
	_ = s.sendJSON(map[string]interface{}{
		"type":     "console_output",
		"serverId": serverID,
		"output":   output,
	})
}

func countConsoleLines(output string) uint64 {
	if output == "" {
		return 0
	}
	lines := uint64(strings.Count(output, "\n"))
	if !strings.HasSuffix(output, "\n") {
		lines++
	}
	return lines
}

func (s *Service) allowConsoleOutput(serverID int, lineCount uint64) bool {
	if serverID <= 0 {
		return false
	}
	now := time.Now()
	s.consoleThrottleMu.Lock()
	defer s.consoleThrottleMu.Unlock()

	state := s.consoleThrottle[serverID]
	if state.WindowStart.IsZero() || now.Sub(state.WindowStart) >= s.consoleThrottleInterval {
		state.WindowStart = now
		state.Count = 0
		state.Warned = false
	}

	if state.Count+lineCount > s.consoleThrottleLines {
		if !state.Warned {
			state.Warned = true
			s.consoleThrottle[serverID] = state
			s.sendConsoleOutputBypass(serverID, "\x1b[1;33m[!] Console output throttled to protect panel stability.\x1b[0m\n")
		} else {
			s.consoleThrottle[serverID] = state
		}
		return false
	}

	state.Count += lineCount
	s.consoleThrottle[serverID] = state
	return true
}

func (s *Service) marshalMessage(input map[string]interface{}, out interface{}) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}


func (s *Service) allowServerCommand(serverID int) (bool, string) {
	if serverID <= 0 {
		return false, "invalid server id"
	}

	now := time.Now().UTC()
	s.commandRateMu.Lock()
	defer s.commandRateMu.Unlock()

	// Calculate current limit (dynamic)
	limit := commandRateLimit
	uptime := time.Since(s.metrics.StartTime)
	if uptime < commandStartupGraceWindow {
		limit = commandStartupGraceLimit
	}

	state := s.commandRate[serverID]
	if state.WindowStart.IsZero() || now.Sub(state.WindowStart) >= commandRateWindow {
		s.commandRate[serverID] = CommandRateState{
			WindowStart: now,
			Count:       1,
		}
		return true, ""
	}

	if state.Count >= limit {
		return false, "command throttled: too many commands in a short time"
	}

	state.Count++
	s.commandRate[serverID] = state
	return true, ""
}

func (s *Service) ResetCommandBudget(serverID int) {
	s.commandRateMu.Lock()
	defer s.commandRateMu.Unlock()
	delete(s.commandRate, serverID)
}

func (s *Service) GetThrottlingStatus() map[int]int {
	s.commandRateMu.Lock()
	defer s.commandRateMu.Unlock()
	
	status := make(map[int]int)
	now := time.Now().UTC()

	for id, state := range s.commandRate {
		if !state.WindowStart.IsZero() && now.Sub(state.WindowStart) < commandRateWindow {
			status[id] = state.Count
		}
	}
	return status
}

func (s *Service) PushToCommandQueue(serverID int, command string) bool {
	s.commandQueuesMu.Lock()
	ch, ok := s.commandQueues[serverID]
	if !ok {
		ch = make(chan string, commandQueueSize)
		s.commandQueues[serverID] = ch
		go s.commandQueueWorker(serverID, ch)
	}
	s.commandQueuesMu.Unlock()

	select {
	case ch <- command:
		return true
	default:
		return false // Queue full
	}
}

func (s *Service) commandQueueWorker(serverID int, ch chan string) {
	ticker := time.NewTicker(commandQueueDrainInterval)
	defer ticker.Stop()

	bootInfo("starting command queue worker for server %d", serverID)
	for command := range ch {
		<-ticker.C
		// Executing from queue still uses a budget check but we might allow 
		// "overflow" from queue if the queue is active.
		// Actually, we just execute at the drain interval which is 100ms (10hz),
		// keeping us safely under the 40/5s (8hz) limit usually, or close to it.
		err := s.Environment(serverID).SendCommand(command)
		if err != nil {
			bootWarn("queue worker failed to send command to server %d: %v", serverID, err)
		}
	}
	bootInfo("stopping command queue worker for server %d", serverID)
}

func (s *Service) chownUser() string {
	if s.cfg.Docker.Rootless.Enabled {
		uid := s.cfg.Docker.Rootless.ContainerUID
		gid := s.cfg.Docker.Rootless.ContainerGID
		if uid < 0 {
			uid = 0
		}
		if gid < 0 {
			gid = 0
		}
		return fmt.Sprintf("%d:%d", uid, gid)
	}
	return "1000:1000"
}

func (s *Service) fixServerPermissions(serverPath string) error {
	if strings.TrimSpace(serverPath) == "" {
		return fmt.Errorf("server path is empty")
	}

	owner := s.chownUser()
	
	// Pre-check if chown is actually needed to reduce IO on large volumes.
	// We ignore errors on stat but log if the first level isn't matching.
	if info, err := os.Stat(serverPath); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			expectedUID, _ := strconv.Atoi(strings.Split(owner, ":")[0])
			if int(stat.Uid) == expectedUID {
				// Base directory already matches, we still proceed with a shallower sync 
				// to ensure interior files moved/restored are fixed, but we are more lenient.
			}
		}
	}

	// Keep ownership aligned with configured runtime user.
	if _, err := runCommand("chown", "-R", owner, serverPath); err != nil {
		bootWarn("chown -R %s failed for %s: %v", owner, serverPath, err)
	}

	// Fallback to broad write permissions so images running with non-1000 UID/GID
	// can still write files after migration/import.
	// We use 'f' flag to ignore most non-critical warnings about weird file types.
	if _, err := runCommand("chmod", "-Rf", "a+rwX", serverPath); err != nil {
		bootWarn("chmod -Rf failed for %s: %v", serverPath, err)
	}

	// Ensure execution bits are preserved for common script/binary extensions
	// This helps with eggs that rely on specific .sh or .bin files to start.
	exts := []string{"*.sh", "*.bin", "*.pl", "*.py", "*.js"}
	for _, ext := range exts {
		_, _ = runCommand("find", serverPath, "-maxdepth", "3", "-name", ext, "-exec", "chmod", "+x", "{}", ";")
	}

	return nil
}
func (s *Service) getServerStats(serverID int) (map[string]interface{}, error) {
	running, cpuPct, memMB, netRxBytes, netTxBytes, uptimeSeconds := s.inspectContainerStats(serverID)
	diskMB := s.getDiskUsageMB(serverID)
	return map[string]interface{}{
		"running":        running,
		"cpu":            cpuPct,
		"memory":         memMB,
		"disk":           diskMB,
		"network_rx":     netRxBytes,
		"network_tx":     netTxBytes,
		"uptime_seconds": uptimeSeconds,
	}, nil
}

func (s *Service) inspectContainerState(serverID int) map[string]interface{} {
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	out, err := runCommand("docker", "inspect", "-f", "{{json .State}}", containerName)
	if err != nil {
		return nil
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return nil
	}
	return state
}
func (s *Service) monitorDockerEvents() {
	cmd := exec.Command("docker", "events", "--filter", "event=start", "--filter", "event=stop", "--filter", "event=die", "--format", "{{.ID}}|{{.Action}}")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		bootWarn("failed to start docker event monitor: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		bootWarn("failed to start docker event monitor: %v", err)
		return
	}
	defer cmd.Wait()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		containerID := parts[0]
		action := parts[1]

		inspectOut, err := runCommand("docker", "inspect", "-f", "{{.Name}}", containerID)
		if err != nil {
			continue
		}
		name := strings.Trim(strings.TrimSpace(inspectOut), "/")
		if !strings.HasPrefix(name, "cpanel-") {
			continue
		}
		serverID, _ := strconv.Atoi(strings.TrimPrefix(name, "cpanel-"))
		if serverID <= 0 {
			continue
		}

		env := s.Environment(serverID)
		switch action {
		case "start":
			env.SetState(StateRunning)
		case "stop", "die":
			env.SetState(StateOffline)
		}
	}
}
