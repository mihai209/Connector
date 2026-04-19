package main

import (
	"context"
	"encoding/json"
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
	return s.executePowerActionWithConfig(serverID, action, stopCommand, ServerInstallConfig{})
}

func (s *Service) executePowerActionWithConfig(serverID int, action, stopCommand string, runtimeCfg ServerInstallConfig) error {
	if serverID <= 0 || action == "" {
		return fmt.Errorf("invalid power payload")
	}

	env := s.Environment(serverID)
	switch action {
	case "start":
		exists, _ := env.Exists()
		if !exists {
			s.sendConsoleOutput(serverID, "\x1b[1;33m[!] Runtime environment is missing. Rebuilding it from the last saved install config...\x1b[0m\n")
			if recoverErr := s.recreateMissingRuntimeContainer(serverID, runtimeCfg); recoverErr != nil {
				return fmt.Errorf("auto-recreate failed: %v", recoverErr)
			}
		}
		if err := env.Start(context.Background()); err != nil {
			return err
		}
		_ = env.Attach(context.Background())
		return nil
	case "stop":
		if stopCommand != "" {
			_ = env.SendCommand(stopCommand)
			time.Sleep(2 * time.Second)
		}
		if err := env.Stop(context.Background(), 30*time.Second); err != nil {
			return err
		}
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
		return nil
	case "restart":
		_ = env.Stop(context.Background(), 30*time.Second)
		return env.Start(context.Background())
	case "kill":
		if err := env.Terminate(context.Background()); err != nil {
			return err
		}
		s.cleanupServerStreams(serverID)
		s.clearBufferedLogs(serverID)
		return nil
	default:
		return fmt.Errorf("unsupported power action: %s", action)
	}
}

func shouldRecoverMissingContainerOnStart(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such object")
}

func (s *Service) recreateMissingRuntimeContainer(serverID int, fallbackCfg ServerInstallConfig) error {
	serverPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID))
	if _, statErr := os.Stat(serverPath); statErr != nil {
		return fmt.Errorf("server path is missing: %w", statErr)
	}

	cfg, err := s.loadRuntimeConfigSnapshot(serverID)
	if err != nil {
		if strings.TrimSpace(fallbackCfg.Image) == "" {
			return fmt.Errorf("runtime config snapshot missing: %w", err)
		}
		cfg = fallbackCfg
		if persistErr := s.persistRuntimeConfigSnapshot(serverID, cfg); persistErr != nil {
			s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not persist runtime config snapshot during auto-recreate: %v\x1b[0m\n", persistErr))
		}
	}
	if strings.TrimSpace(cfg.Image) == "" {
		return fmt.Errorf("runtime config snapshot has no image")
	}

	if err := s.pullImage(cfg.Image); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	if err := s.removeContainerIfExists(serverID); err != nil {
		return fmt.Errorf("cleanup stale container failed: %w", err)
	}
	env := s.Environment(serverID)
	if err := env.Create(cfg, serverPath); err != nil {
		return err
	}
	return nil
}

func (s *Service) executeServerCommand(serverID int, command string) error {
	if serverID <= 0 {
		return fmt.Errorf("invalid serverId")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}

	allowed, errStr := s.allowServerCommand(serverID)
	if allowed {
		return s.Environment(serverID).SendCommand(command)
	}

	// If throttled, try to queue it instead of failing
	if s.PushToCommandQueue(serverID, command) {
		// Log internal audit/diagnostic if needed, but return success to panel
		bootInfo("command queued for server %d due to rate limit: %s", serverID, command)
		return nil
	}

	return fmt.Errorf("%s (queue full)", errStr)
}

func (s *Service) handlePowerAction(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	action := strings.ToLower(strings.TrimSpace(asString(message["action"])))
	stopCommand := strings.TrimSpace(asString(message["stopCommand"]))
	requestID := strings.TrimSpace(asString(message["requestId"]))
	var runtimeCfg ServerInstallConfig
	if rawCfg, ok := message["runtimeConfig"]; ok && rawCfg != nil {
		payload, marshalErr := json.Marshal(rawCfg)
		if marshalErr != nil {
			bootWarn("server power runtime config encode failed server=%d error=%v", serverID, marshalErr)
		} else if err := json.Unmarshal(payload, &runtimeCfg); err != nil {
			bootWarn("server power runtime config parse failed server=%d error=%v", serverID, err)
		}
	}
	if serverID <= 0 || action == "" {
		s.recordPowerMetric("failed")
		return
	}

	if err := s.beginPowerAction(serverID, action); err != nil {
		s.recordPowerMetric("failed")
		s.sendActionAck(serverID, "power", "failed", err.Error(), requestID, map[string]interface{}{
			"action": action,
		})
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Power action rejected (%s): %v\x1b[0m\n", action, err))
		return
	}
	defer s.finishPowerAction(serverID)

	s.sendActionAck(serverID, "power", "accepted", fmt.Sprintf("Power action '%s' accepted.", action), requestID, map[string]interface{}{
		"action": action,
	})

	if err := s.executePowerActionWithConfig(serverID, action, stopCommand, runtimeCfg); err != nil {
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
	s.ensureServerLogStream(serverID, true, false, true)
}

func (s *Service) handleCheckServerStatus(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}

	running, cpuPct, memMB, netRxBytes, netTxBytes, uptimeSeconds := s.inspectContainerStats(serverID)
	status := "stopped"
	if running {
		status = "running"
	}
	statusPayload := map[string]interface{}{
		"type":     "server_status_update",
		"serverId": serverID,
		"status":   status,
	}
	if !running {
		state := s.inspectContainerState(serverID)
		if value, ok := state["exitCode"]; ok {
			switch typed := value.(type) {
			case float64:
				statusPayload["exitCode"] = int(typed)
			case int:
				statusPayload["exitCode"] = typed
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
					statusPayload["exitCode"] = parsed
				}
			}
		}
		if value, ok := state["oomKilled"]; ok {
			if parsed, okBool := value.(bool); okBool && parsed {
				statusPayload["oomKilled"] = true
			}
		}
		if value, ok := state["finishedAt"]; ok {
			statusPayload["finishedAt"] = value
		}
		if value, ok := state["startedAt"]; ok {
			statusPayload["startedAt"] = value
		}
	}
	_ = s.sendJSON(statusPayload)

	diskMB := 0
	if running {
		diskMB = s.getDiskUsageMB(serverID)
	}
	_ = s.sendJSON(map[string]interface{}{
		"type":           "server_stats",
		"serverId":       serverID,
		"cpu":            fmt.Sprintf("%.1f", cpuPct),
		"memory":         strconv.Itoa(memMB),
		"disk":           strconv.Itoa(diskMB),
		"network_rx":     strconv.FormatInt(netRxBytes, 10),
		"network_tx":     strconv.FormatInt(netTxBytes, 10),
		"uptime_seconds": uptimeSeconds,
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
