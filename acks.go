package main

import (
	"strings"
	"time"
)

func (s *Service) sendActionAck(serverID int, actionType, phase, message, requestID string, extra map[string]interface{}) {
	if serverID <= 0 {
		return
	}

	payload := map[string]interface{}{
		"type":       "server_action_ack",
		"serverId":   serverID,
		"actionType": strings.TrimSpace(actionType),
		"phase":      strings.TrimSpace(phase),
		"message":    strings.TrimSpace(message),
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	}
	if strings.TrimSpace(requestID) != "" {
		payload["requestId"] = strings.TrimSpace(requestID)
	}
	for key, value := range extra {
		payload[key] = value
	}
	_ = s.sendJSON(payload)
}
