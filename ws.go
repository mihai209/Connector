package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type authFatalError struct {
	reason string
}

func (e *authFatalError) Error() string {
	if strings.TrimSpace(e.reason) == "" {
		return "panel auth failed"
	}
	return fmt.Sprintf("panel auth failed: %s", e.reason)
}

func (s *Service) runWSLoop() {
	for {
		if err := s.connectAndServeWS(); err != nil {
			var authErr *authFatalError
			if errors.As(err, &authErr) {
				bootFatal("%s", authErr.Error())
				os.Exit(1)
			}
			bootInfo("websocket disconnected error=%v", err)
		}
		bootInfo("retrying websocket connection delay=%s", wsReconnectDelay)
		time.Sleep(wsReconnectDelay)
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

	s.wsConnMu.Lock()
	s.wsConn = conn
	s.wsConnMu.Unlock()
	defer func() {
		s.wsConnMu.Lock()
		if s.wsConn == conn {
			s.wsConn = nil
		}
		s.wsConnMu.Unlock()
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
	case "install_server":
		go s.handleInstallServer(envelope)
	case "server_power":
		go s.handlePowerAction(envelope)
	case "server_logs":
		go s.handleServerLogs(envelope)
	case "server_command":
		go s.handleServerCommand(envelope)
	case "import_sftp_files":
		go s.handleImportSFTPFiles(envelope)
	case "log_cleanup":
		go s.handleLogCleanup(envelope)
	case "check_server_status":
		go s.handleCheckServerStatus(envelope)
	case "check_eula":
		go s.handleCheckEULA(envelope)
	case "accept_eula":
		go s.handleAcceptEULA(envelope)
	case "delete_server":
		go s.handleDeleteServer(envelope)
	case "read_file":
		go s.handleReadFile(envelope)
	case "write_file":
		go s.handleWriteFile(envelope)
	case "list_file_versions":
		go s.handleListFileVersions(envelope)
	case "read_file_version":
		go s.handleReadFileVersion(envelope)
	case "download_file":
		go s.handleDownloadFile(envelope)
	case "list_files", "create_folder", "create_file", "rename_file", "delete_files", "set_permissions":
		go s.handleFilesAction(messageType, envelope)
	case "file_action", "server_file_action":
		if handled := s.dispatchFileActionAlias(envelope); handled {
			// handled
		}
	case "schedule_action", "server_schedule_action":
		go s.handleServerScheduleAction(envelope)
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
	case "list_files", "create_folder", "create_file", "rename_file", "delete_files", "set_permissions":
		go s.handleFilesAction(action, envelope)
		return true
	case "read_file":
		go s.handleReadFile(envelope)
		return true
	case "write_file":
		go s.handleWriteFile(envelope)
		return true
	case "list_file_versions":
		go s.handleListFileVersions(envelope)
		return true
	case "read_file_version":
		go s.handleReadFileVersion(envelope)
		return true
	case "download_file":
		go s.handleDownloadFile(envelope)
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
