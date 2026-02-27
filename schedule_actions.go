package main

import (
	"strings"
)

func (s *Service) handleServerScheduleAction(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}

	action := strings.ToLower(strings.TrimSpace(asString(message["scheduleAction"])))
	if action == "" {
		action = strings.ToLower(strings.TrimSpace(asString(message["action"])))
	}
	if action == "" {
		s.sendServerError(serverID, "missing schedule action")
		return
	}

	switch action {
	case "command":
		command := strings.TrimSpace(asString(message["command"]))
		if command == "" {
			command = strings.TrimSpace(asString(message["payload"]))
		}
		if command == "" {
			s.sendServerError(serverID, "missing schedule command payload")
			return
		}
		s.handleServerCommand(map[string]interface{}{
			"serverId": serverID,
			"command":  command,
		})
	case "power":
		powerAction := strings.TrimSpace(asString(message["powerAction"]))
		if powerAction == "" {
			powerAction = strings.TrimSpace(asString(message["payload"]))
		}
		if powerAction == "" {
			s.sendServerError(serverID, "missing schedule power payload")
			return
		}
		s.handlePowerAction(map[string]interface{}{
			"serverId": serverID,
			"action":   powerAction,
		})
	case "backup":
		// Backups are queued and executed on panel workers (job queue), not in connector runtime.
		s.sendServerError(serverID, "backup schedule action is handled by panel job queue")
	default:
		s.sendServerError(serverID, "unsupported schedule action")
	}
}
