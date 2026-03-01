package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func parseMinecraftEULA(raw []byte) bool {
	content := strings.TrimPrefix(string(raw), "\uFEFF")
	accepted := false

	for _, line := range strings.Split(content, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, ";") {
			continue
		}

		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if key != "eula" {
			continue
		}

		value := strings.ToLower(strings.TrimSpace(parts[1]))
		switch value {
		case "true", "1", "yes", "on":
			accepted = true
		case "false", "0", "no", "off":
			accepted = false
		}
	}

	return accepted
}

func (s *Service) executePowerAction(serverID int, action, stopCommand string) error {
	if serverID <= 0 || action == "" {
		return fmt.Errorf("invalid power payload")
	}

	containerName := fmt.Sprintf("cpanel-%d", serverID)
	switch action {
	case "start":
		if _, err := runCommand("docker", "start", containerName); err != nil {
			return err
		}
		time.Sleep(logAttachRetryDelay)
		s.ensureServerLogStream(serverID, false, true)
		return nil
	case "stop":
		if stopCommand != "" {
			if err := s.sendCommandToServerStdin(serverID, containerName, stopCommand); err != nil {
				bootWarn("graceful stop stdin failed server=%d error=%v", serverID, err)
			}
			time.Sleep(2 * time.Second)
		}
		if _, err := runCommand("docker", "stop", containerName); err != nil {
			return err
		}
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
		return nil
	case "restart":
		if _, err := runCommand("docker", "restart", containerName); err != nil {
			return err
		}
		time.Sleep(logAttachRetryDelay)
		s.ensureServerLogStream(serverID, false, true)
		return nil
	case "kill":
		if _, err := runCommand("docker", "kill", containerName); err != nil {
			return err
		}
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
		return nil
	default:
		return fmt.Errorf("unsupported power action: %s", action)
	}
}

func (s *Service) executeServerCommand(serverID int, command string) error {
	if serverID <= 0 {
		return fmt.Errorf("invalid serverId")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	return s.sendCommandToServerStdin(serverID, containerName, command)
}

func (s *Service) handlePowerAction(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	action := strings.ToLower(strings.TrimSpace(asString(message["action"])))
	stopCommand := strings.TrimSpace(asString(message["stopCommand"]))
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if serverID <= 0 || action == "" {
		s.recordPowerMetric("failed")
		return
	}

	s.sendActionAck(serverID, "power", "accepted", fmt.Sprintf("Power action '%s' accepted.", action), requestID, map[string]interface{}{
		"action": action,
	})

	if err := s.executePowerAction(serverID, action, stopCommand); err != nil {
		s.recordPowerMetric("failed")
		s.sendActionAck(serverID, "power", "failed", err.Error(), requestID, map[string]interface{}{
			"action": action,
		})
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Power action failed (%s): %v\x1b[0m\n", action, err))
		return
	}

	s.recordPowerMetric("executed")
	s.sendActionAck(serverID, "power", "executed", fmt.Sprintf("Power action '%s' executed.", action), requestID, map[string]interface{}{
		"action": action,
	})
}

func (s *Service) handleServerCommand(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	command := strings.TrimSpace(asString(message["command"]))
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if serverID <= 0 || command == "" {
		s.recordCommandMetric("failed")
		return
	}

	s.sendActionAck(serverID, "command", "accepted", "Command accepted for execution.", requestID, map[string]interface{}{
		"command": command,
	})

	if err := s.executeServerCommand(serverID, command); err != nil {
		s.recordCommandMetric("failed")
		s.sendActionAck(serverID, "command", "failed", err.Error(), requestID, map[string]interface{}{
			"command": command,
		})
		bootWarn("server command stdin failed server=%d command=%q error=%v", serverID, command, err)
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Failed to send command to server stdin: %v\x1b[0m\n", err))
		return
	}

	s.recordCommandMetric("executed")
	s.sendActionAck(serverID, "command", "executed", "Command delivered to server stdin.", requestID, map[string]interface{}{
		"command": command,
	})
}

func (s *Service) handleServerLogs(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}
	s.ensureServerLogStream(serverID, true, false)
}

func (s *Service) handleCheckServerStatus(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}

	running, cpuPct, memMB := s.inspectContainerStats(serverID)
	status := "stopped"
	if running {
		status = "running"
	}
	_ = s.sendJSON(map[string]interface{}{
		"type":     "server_status_update",
		"serverId": serverID,
		"status":   status,
	})

	diskMB := 0
	if running {
		diskMB = s.getDiskUsageMB(serverID)
	}
	_ = s.sendJSON(map[string]interface{}{
		"type":     "server_stats",
		"serverId": serverID,
		"cpu":      fmt.Sprintf("%.1f", cpuPct),
		"memory":   strconv.Itoa(memMB),
		"disk":     strconv.Itoa(diskMB),
	})
}

func (s *Service) handleCheckEULA(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}
	eulaPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID), "eula.txt")
	accepted := false
	if raw, err := os.ReadFile(eulaPath); err == nil {
		accepted = parseMinecraftEULA(raw)
	} else if !os.IsNotExist(err) {
		bootWarn("failed reading eula file server=%d path=%s error=%v", serverID, eulaPath, err)
	}
	_ = s.sendJSON(map[string]interface{}{
		"type":     "eula_status",
		"serverId": serverID,
		"accepted": accepted,
	})
}

func (s *Service) handleAcceptEULA(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}
	eulaPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID), "eula.txt")
	_ = os.MkdirAll(filepath.Dir(eulaPath), 0o755)
	_ = os.WriteFile(eulaPath, []byte("eula=true\n"), 0o644)
	_ = s.sendJSON(map[string]interface{}{
		"type":     "eula_status",
		"serverId": serverID,
		"accepted": true,
	})
}

func (s *Service) handleDeleteServer(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	_, _ = runCommand("docker", "rm", "-f", containerName)

	s.cleanupServerStreams(serverID)
	s.clearBufferedLogs(serverID)
	s.cacheMu.Lock()
	delete(s.diskUsageCache, serverID)
	s.cacheMu.Unlock()

	serverPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID))
	if err := os.RemoveAll(serverPath); err != nil {
		_ = s.sendJSON(map[string]interface{}{
			"type":     "delete_fail",
			"serverId": serverID,
			"error":    err.Error(),
		})
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":     "delete_success",
		"serverId": serverID,
	})
}
