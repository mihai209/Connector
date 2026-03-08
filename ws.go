package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type authFatalError struct {
	reason string
}

const (
	wsReadLimitMinMB int64 = 8
	wsReadLimitMaxMB int64 = 1024
)

func (e *authFatalError) Error() string {
	if strings.TrimSpace(e.reason) == "" {
		return "panel auth failed"
	}
	return fmt.Sprintf("panel auth failed: %s", e.reason)
}

func clampWSReadLimitMB(limitMb int64) int64 {
	if limitMb < wsReadLimitMinMB {
		return wsReadLimitMinMB
	}
	if limitMb > wsReadLimitMaxMB {
		return wsReadLimitMaxMB
	}
	return limitMb
}

func (s *Service) getWSReadLimitMB() int64 {
	s.wsConnMu.RLock()
	current := s.wsReadLimitMB
	s.wsConnMu.RUnlock()
	if current <= 0 {
		return clampWSReadLimitMB(defaultWSReadLimitMB)
	}
	return clampWSReadLimitMB(current)
}

func (s *Service) applyWSReadLimitMB(limitMb int64, source string) {
	normalized := clampWSReadLimitMB(limitMb)
	if strings.TrimSpace(source) == "" {
		source = "runtime"
	}
	readLimitBytes := normalized * 1024 * 1024
	previous := int64(0)

	s.wsConnMu.Lock()
	previous = s.wsReadLimitMB
	s.wsReadLimitMB = normalized
	if s.wsConn != nil {
		s.wsConn.SetReadLimit(readLimitBytes)
	}
	s.wsConnMu.Unlock()

	if previous != normalized {
		bootInfo("updated websocket read limit mb=%d source=%s", normalized, source)
	}
}

func (s *Service) dispatchWSHandler(name string, handler func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				bootWarn("panic recovered in ws handler=%s panic=%v", name, recovered)
				bootWarn("panic stack handler=%s stack=%s", name, strings.TrimSpace(string(debug.Stack())))
			}
		}()
		handler()
	}()
}

func (s *Service) runWSLoop() {
	reconnectDelay := wsReconnectDelay

	for {
		startedAt := time.Now()
		if err := s.connectAndServeWS(); err != nil {
			var authErr *authFatalError
			if errors.As(err, &authErr) {
				bootFatal("%s", authErr.Error())
				os.Exit(1)
			}
			bootInfo("websocket disconnected error=%v", err)
		}

		sleepDelay := reconnectDelay
		if sleepDelay < wsReconnectDelay {
			sleepDelay = wsReconnectDelay
		}

		if time.Since(startedAt) >= wsReconnectResetAfter {
			reconnectDelay = wsReconnectDelay
		} else {
			nextDelay := reconnectDelay * 2
			if nextDelay > wsReconnectMaxDelay {
				nextDelay = wsReconnectMaxDelay
			}
			reconnectDelay = nextDelay
		}

		bootInfo("retrying websocket connection delay=%s", sleepDelay)
		time.Sleep(sleepDelay)
	}
}

func (s *Service) connectAndServeWS() error {
	if err := validatePanelOriginAllowed(s.cfg.Panel.URL, s.cfg.Panel.AllowedURLs); err != nil {
		return err
	}

	wsURL, err := buildPanelWSURL(s.cfg.Panel.URL)
	if err != nil {
		return err
	}
	bootInfo("connecting websocket endpoint=%s", wsURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	readLimitBytes := s.getWSReadLimitMB() * 1024 * 1024
	conn.SetReadLimit(readLimitBytes)

	s.wsConnMu.Lock()
	s.wsConn = conn
	s.wsConnMu.Unlock()
	s.setWSConnected(true)
	defer func() {
		s.wsConnMu.Lock()
		if s.wsConn == conn {
			s.wsConn = nil
		}
		s.wsConnMu.Unlock()
		s.setWSConnected(false)
	}()

	if err := s.sendJSON(map[string]interface{}{
		"type":  "auth",
		"id":    s.cfg.Connector.ID,
		"token": s.cfg.Connector.Token,
	}); err != nil {
		return err
	}
	bootInfo("sent websocket auth payload connector_id=%d", s.cfg.Connector.ID)

	done := make(chan struct{})
	go s.startHeartbeat(done)
	defer close(done)

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			s.cleanupAllStreams()
			if strings.Contains(strings.ToLower(err.Error()), "read limit exceeded") {
				bootWarn(
					"websocket read limit exceeded (limit_mb=%d). Increase system.wsReadLimitMb or CONNECTOR_WS_READ_LIMIT_MB if needed.",
					s.getWSReadLimitMB(),
				)
			}
			if closeErr, ok := err.(*websocket.CloseError); ok {
				reason := strings.ToLower(strings.TrimSpace(closeErr.Text))
				if closeErr.Code == 4003 || strings.Contains(reason, "invalid token") || strings.Contains(reason, "token regenerated") || strings.Contains(reason, "token revoked") {
					return &authFatalError{reason: closeErr.Text}
				}
			}
			return err
		}
		if err := s.handleWSMessage(payload); err != nil {
			bootWarn("failed to handle ws payload error=%v", err)
			var authErr *authFatalError
			if errors.As(err, &authErr) {
				return authErr
			}
		}
	}
}

func (s *Service) handleWSMessage(payload []byte) error {
	var envelope map[string]interface{}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return err
	}
	messageType := strings.ToLower(strings.TrimSpace(asString(envelope["type"])))
	if messageType == "" {
		return nil
	}

	switch messageType {
	case "auth_success":
		bootInfo("websocket auth succeeded connector_id=%d", s.cfg.Connector.ID)
	case "auth_fail":
		return &authFatalError{reason: asString(envelope["error"])}
	case "connector_set_ws_read_limit":
		limitMb := asInt(envelope["limitMb"])
		if limitMb <= 0 {
			limitMb = asInt(envelope["wsReadLimitMb"])
		}
		if limitMb > 0 {
			s.applyWSReadLimitMB(int64(limitMb), asString(envelope["source"]))
		}
	case "install_server":
		s.dispatchWSHandler("install_server", func() { s.handleInstallServer(envelope) })
	case "server_power":
		s.dispatchWSHandler("server_power", func() { s.handlePowerAction(envelope) })
	case "server_logs":
		s.dispatchWSHandler("server_logs", func() { s.handleServerLogs(envelope) })
	case "server_command":
		s.dispatchWSHandler("server_command", func() { s.handleServerCommand(envelope) })
	case "import_sftp_files":
		s.dispatchWSHandler("import_sftp_files", func() { s.handleImportSFTPFiles(envelope) })
	case "fix_server_permissions":
		s.dispatchWSHandler("fix_server_permissions", func() { s.handleFixServerPermissions(envelope) })
	case "log_cleanup":
		s.dispatchWSHandler("log_cleanup", func() { s.handleLogCleanup(envelope) })
	case "check_server_status":
		s.dispatchWSHandler("check_server_status", func() { s.handleCheckServerStatus(envelope) })
	case "check_eula":
		s.dispatchWSHandler("check_eula", func() { s.handleCheckEULA(envelope) })
	case "accept_eula":
		s.dispatchWSHandler("accept_eula", func() { s.handleAcceptEULA(envelope) })
	case "delete_server":
		s.dispatchWSHandler("delete_server", func() { s.handleDeleteServer(envelope) })
	case "read_file":
		s.dispatchWSHandler("read_file", func() { s.handleReadFile(envelope) })
	case "write_file":
		s.dispatchWSHandler("write_file", func() { s.handleWriteFile(envelope) })
	case "list_file_versions":
		s.dispatchWSHandler("list_file_versions", func() { s.handleListFileVersions(envelope) })
	case "read_file_version":
		s.dispatchWSHandler("read_file_version", func() { s.handleReadFileVersion(envelope) })
	case "extract_archive":
		s.dispatchWSHandler("extract_archive", func() { s.handleExtractArchive(envelope) })
	case "download_file":
		s.dispatchWSHandler("download_file", func() { s.handleDownloadFile(envelope) })
	case "dependency_mirror_check":
		s.dispatchWSHandler("dependency_mirror_check", func() { s.handleDependencyMirrorCheck(envelope) })
	case "search_files":
		s.dispatchWSHandler("search_files", func() { s.handleSearchFiles(envelope) })
	case "list_files", "create_folder", "create_file", "rename_file", "delete_files", "set_permissions":
		s.dispatchWSHandler(messageType, func() { s.handleFilesAction(messageType, envelope) })
	case "file_action", "server_file_action":
		if handled := s.dispatchFileActionAlias(envelope); handled {
			// handled
		}
	case "schedule_action", "server_schedule_action":
		s.dispatchWSHandler("server_schedule_action", func() { s.handleServerScheduleAction(envelope) })
	case "apply_resource_limits", "server_apply_resource_limits", "server_resource_limits_apply":
		s.dispatchWSHandler("server_apply_resource_limits", func() { s.handleApplyResourceLimits(envelope) })
	default:
		// keep compatibility with future panel messages.
	}

	return nil
}

func (s *Service) dispatchFileActionAlias(envelope map[string]interface{}) bool {
	action := strings.ToLower(strings.TrimSpace(asString(envelope["action"])))
	if action == "" {
		action = strings.ToLower(strings.TrimSpace(asString(envelope["fileAction"])))
	}
	switch action {
	case "search_files":
		s.dispatchWSHandler("search_files", func() { s.handleSearchFiles(envelope) })
		return true
	case "list_files", "create_folder", "create_file", "rename_file", "delete_files", "set_permissions":
		s.dispatchWSHandler(action, func() { s.handleFilesAction(action, envelope) })
		return true
	case "read_file":
		s.dispatchWSHandler("read_file", func() { s.handleReadFile(envelope) })
		return true
	case "write_file":
		s.dispatchWSHandler("write_file", func() { s.handleWriteFile(envelope) })
		return true
	case "list_file_versions":
		s.dispatchWSHandler("list_file_versions", func() { s.handleListFileVersions(envelope) })
		return true
	case "read_file_version":
		s.dispatchWSHandler("read_file_version", func() { s.handleReadFileVersion(envelope) })
		return true
	case "extract_archive":
		s.dispatchWSHandler("extract_archive", func() { s.handleExtractArchive(envelope) })
		return true
	case "download_file":
		s.dispatchWSHandler("download_file", func() { s.handleDownloadFile(envelope) })
		return true
	default:
		return false
	}
}

func buildPanelWSURL(panelURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(panelURL))
	if err != nil {
		return "", err
	}
	if strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/connector"
	u.RawQuery = ""
	return u.String(), nil
}

func validatePanelOriginAllowed(panelURL string, allowedOrigins []string) error {
	if len(allowedOrigins) == 0 {
		return nil
	}

	origin, err := extractURLOrigin(panelURL)
	if err != nil {
		return &authFatalError{reason: fmt.Sprintf("invalid panel.url: %v", err)}
	}

	for _, allowed := range allowedOrigins {
		allowedValue := strings.TrimSpace(strings.ToLower(allowed))
		if allowedValue == "" {
			continue
		}
		if allowedValue == "*" || allowedValue == strings.ToLower(origin) {
			return nil
		}
	}

	return &authFatalError{reason: fmt.Sprintf("panel origin %s not in allowed list %v", origin, allowedOrigins)}
}
