package main

import (
	"fmt"
	"strings"
)

func (s *Service) handleServerScheduleAction(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}

	action := strings.ToLower(strings.TrimSpace(asString(message["scheduleAction"])))
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if action == "" {
		action = strings.ToLower(strings.TrimSpace(asString(message["action"])))
	}
	if action == "" {
		s.recordScheduleMetric("failed")
		s.sendActionAck(serverID, "schedule", "failed", "missing schedule action", requestID, nil)
		s.sendServerError(serverID, "missing schedule action")
		return
	}

	s.sendActionAck(serverID, "schedule", "accepted", fmt.Sprintf("Schedule action '%s' accepted.", action), requestID, map[string]interface{}{
		"scheduleAction": action,
	})

	switch action {
	case "command":
		command := strings.TrimSpace(asString(message["command"]))
		if command == "" {
			command = strings.TrimSpace(asString(message["payload"]))
		}
		if command == "" {
			s.recordScheduleMetric("failed")
			s.sendActionAck(serverID, "schedule", "failed", "missing schedule command payload", requestID, map[string]interface{}{
				"scheduleAction": action,
			})
			s.sendServerError(serverID, "missing schedule command payload")
			return
		}
		if err := s.executeServerCommand(serverID, command); err != nil {
			s.recordScheduleMetric("failed")
			s.sendActionAck(serverID, "schedule", "failed", err.Error(), requestID, map[string]interface{}{
				"scheduleAction": action,
				"command":        command,
			})
			s.sendServerError(serverID, err.Error())
			return
		}
		s.recordScheduleMetric("executed")
		s.sendActionAck(serverID, "schedule", "executed", "Schedule command executed.", requestID, map[string]interface{}{
			"scheduleAction": action,
			"command":        command,
		})
	case "power":
		powerAction := strings.TrimSpace(asString(message["powerAction"]))
		if powerAction == "" {
			powerAction = strings.TrimSpace(asString(message["payload"]))
		}
		if powerAction == "" {
			s.recordScheduleMetric("failed")
			s.sendActionAck(serverID, "schedule", "failed", "missing schedule power payload", requestID, map[string]interface{}{
				"scheduleAction": action,
			})
			s.sendServerError(serverID, "missing schedule power payload")
			return
		}
		if err := s.executePowerAction(serverID, strings.ToLower(strings.TrimSpace(powerAction)), ""); err != nil {
			s.recordScheduleMetric("failed")
			s.sendActionAck(serverID, "schedule", "failed", err.Error(), requestID, map[string]interface{}{
				"scheduleAction": action,
				"powerAction":    powerAction,
			})
			s.sendServerError(serverID, err.Error())
			return
		}
		s.recordScheduleMetric("executed")
		s.sendActionAck(serverID, "schedule", "executed", "Schedule power action executed.", requestID, map[string]interface{}{
			"scheduleAction": action,
			"powerAction":    powerAction,
		})
	case "backup":
		// Backups are queued and executed on panel workers (job queue), not in connector runtime.
		s.recordScheduleMetric("failed")
		s.sendActionAck(serverID, "schedule", "failed", "backup schedule action is handled by panel job queue", requestID, map[string]interface{}{
			"scheduleAction": action,
		})
		s.sendServerError(serverID, "backup schedule action is handled by panel job queue")
	default:
		s.recordScheduleMetric("failed")
		s.sendActionAck(serverID, "schedule", "failed", "unsupported schedule action", requestID, map[string]interface{}{
			"scheduleAction": action,
		})
		s.sendServerError(serverID, "unsupported schedule action")
	}
}
