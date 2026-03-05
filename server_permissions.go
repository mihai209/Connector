package main

import (
	"fmt"
	"strings"
)

func (s *Service) handleFixServerPermissions(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if serverID <= 0 {
		return
	}

	s.sendActionAck(serverID, "fix_permissions", "accepted", "Permission repair accepted.", requestID, nil)

	serverPath, err := safeServerPath(s.volumesPath, serverID, "/")
	if err != nil {
		s.sendActionAck(serverID, "fix_permissions", "failed", err.Error(), requestID, nil)
		_ = s.sendJSON(map[string]interface{}{
			"type":      "fix_server_permissions_result",
			"serverId":  serverID,
			"success":   false,
			"error":     err.Error(),
			"requestId": requestID,
		})
		return
	}

	if err := s.fixServerPermissions(serverPath); err != nil {
		s.sendActionAck(serverID, "fix_permissions", "failed", err.Error(), requestID, nil)
		s.sendConsoleOutput(serverID, fmt.Sprintf("[!] Permission repair failed: %v\n", err))
		_ = s.sendJSON(map[string]interface{}{
			"type":      "fix_server_permissions_result",
			"serverId":  serverID,
			"success":   false,
			"error":     err.Error(),
			"path":      serverPath,
			"requestId": requestID,
		})
		return
	}

	s.sendActionAck(serverID, "fix_permissions", "executed", "Server file permissions repaired.", requestID, nil)
	s.sendConsoleOutput(serverID, "[*] Server file permissions repaired successfully.\n")
	_ = s.sendJSON(map[string]interface{}{
		"type":      "fix_server_permissions_result",
		"serverId":  serverID,
		"success":   true,
		"path":      serverPath,
		"requestId": requestID,
	})
}
