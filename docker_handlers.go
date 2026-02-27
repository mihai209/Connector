package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Service) handlePowerAction(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	action := strings.ToLower(strings.TrimSpace(asString(message["action"])))
	stopCommand := strings.TrimSpace(asString(message["stopCommand"]))
	if serverID <= 0 || action == "" {
		return
	}

	containerName := fmt.Sprintf("cpanel-%d", serverID)
	switch action {
	case "start":
		_, _ = runCommand("docker", "start", containerName)
		time.Sleep(logAttachRetryDelay)
		s.ensureServerLogStream(serverID, false, true)
	case "stop":
		if stopCommand != "" {
			if err := s.sendCommandToServerStdin(serverID, containerName, stopCommand); err != nil {
				bootWarn("graceful stop stdin failed server=%d error=%v", serverID, err)
			}
			time.Sleep(2 * time.Second)
		}
		_, _ = runCommand("docker", "stop", containerName)
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
	case "restart":
		_, _ = runCommand("docker", "restart", containerName)
		time.Sleep(logAttachRetryDelay)
		s.ensureServerLogStream(serverID, false, true)
	case "kill":
		_, _ = runCommand("docker", "kill", containerName)
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
	}
}

func (s *Service) handleServerCommand(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	command := strings.TrimSpace(asString(message["command"]))
	if serverID <= 0 || command == "" {
		return
	}
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	if err := s.sendCommandToServerStdin(serverID, containerName, command); err != nil {
		bootWarn("server command stdin failed server=%d command=%q error=%v", serverID, command, err)
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Failed to send command to server stdin: %v\x1b[0m\n", err))
	}
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
		accepted = strings.Contains(string(raw), "eula=true")
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
