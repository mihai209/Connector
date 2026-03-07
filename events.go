package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const dockerDebugLogTailMaxChars = 32 * 1024

func (s *Service) monitorDockerEvents() {
	for {
		s.runDockerEventsLoop()
		time.Sleep(3 * time.Second)
	}
}

func (s *Service) runDockerEventsLoop() {
	cmd := exec.Command(
		"docker", "events",
		"--filter", "type=container",
		"--filter", "event=start",
		"--filter", "event=stop",
		"--filter", "event=die",
		"--filter", "event=kill",
		"--format", "{{json .}}",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("docker events pipe error: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("docker events start error: %v", err)
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event struct {
			Action string `json:"Action"`
			Status string `json:"status"`
			Actor  struct {
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		containerName := event.Actor.Attributes["name"]
		if !strings.HasPrefix(containerName, "cpanel-") {
			continue
		}
		serverID, err := strconv.Atoi(strings.TrimPrefix(containerName, "cpanel-"))
		if err != nil {
			continue
		}

		action := strings.ToLower(strings.TrimSpace(event.Action))
		if action == "" {
			action = strings.ToLower(strings.TrimSpace(event.Status))
		}
		if action != "start" && action != "stop" && action != "die" && action != "kill" {
			continue
		}

		log.Printf("docker event: %s for server %d", action, serverID)
		status := "stopped"
		if action == "start" {
			status = "running"
		}

		debugPayload := map[string]interface{}{
			"type":     "server_debug_event",
			"serverId": serverID,
			"event":    action,
		}
		state := s.inspectContainerState(serverID)
		exitCode := -1
		oomKilled := false
		if len(state) > 0 {
			debugPayload["state"] = state
			if value, ok := state["exitCode"]; ok {
				switch typed := value.(type) {
				case float64:
					exitCode = int(typed)
				case int:
					exitCode = typed
				case string:
					if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
						exitCode = parsed
					}
				}
			}
			if value, ok := state["oomKilled"]; ok {
				if parsed, okBool := value.(bool); okBool {
					oomKilled = parsed
				}
			}
		}

		statusPayload := map[string]interface{}{
			"type":     "server_status_update",
			"serverId": serverID,
			"status":   status,
		}
		if exitCode >= 0 {
			statusPayload["exitCode"] = exitCode
		}
		if oomKilled {
			statusPayload["oomKilled"] = true
		}
		_ = s.sendJSON(statusPayload)
		logTail := ""
		if action == "stop" || action == "die" || action == "kill" {
			logTail = s.readDockerLogTail(serverID, 300, dockerDebugLogTailMaxChars)
			if logTail != "" {
				debugPayload["logTail"] = logTail
				debugPayload["logSource"] = "docker"
			}
			if bundlePath, err := s.writeCrashBundle(serverID, action, state, logTail); err == nil {
				debugPayload["bundlePath"] = bundlePath
				s.recordCrashBundleMetric()
			} else {
				bootWarn("failed to write crash bundle server=%d event=%s error=%v", serverID, action, err)
			}
		}
		_ = s.sendJSON(debugPayload)

		if action == "start" {
			go func(id int) {
				time.Sleep(logAttachRetryDelay)
				s.ensureServerLogStream(id, false, true, false)
			}(serverID)
		}

		if action == "stop" || action == "die" || action == "kill" {
			s.cleanupServerStreams(serverID)
			s.clearBufferedLogs(serverID)
			_ = s.sendJSON(map[string]interface{}{
				"type":     "server_stats",
				"serverId": serverID,
				"cpu":      "0.0",
				"memory":   "0",
				"disk":     "0",
			})
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("docker events scanner error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("docker events exited: %v", err)
	}
}

func (s *Service) dockerContainerRunning(serverID int) bool {
	out, err := runCommand("docker", "inspect", "-f", "{{.State.Running}}", fmt.Sprintf("cpanel-%d", serverID))
	return err == nil && strings.TrimSpace(out) == "true"
}

func (s *Service) inspectContainerState(serverID int) map[string]interface{} {
	out, err := runCommand("docker", "inspect", "-f", "{{json .State}}", fmt.Sprintf("cpanel-%d", serverID))
	if err != nil {
		return nil
	}

	var state map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &state); err != nil {
		return nil
	}

	normalized := map[string]interface{}{}
	if value, ok := state["Status"]; ok {
		normalized["status"] = value
	}
	if value, ok := state["ExitCode"]; ok {
		normalized["exitCode"] = value
	}
	if value, ok := state["OOMKilled"]; ok {
		normalized["oomKilled"] = value
	}
	if value, ok := state["Error"]; ok {
		normalized["error"] = value
	}
	if value, ok := state["FinishedAt"]; ok {
		normalized["finishedAt"] = value
	}
	if value, ok := state["StartedAt"]; ok {
		normalized["startedAt"] = value
	}

	return normalized
}

func (s *Service) readDockerLogTail(serverID int, tailLines int, maxChars int) string {
	if tailLines <= 0 {
		tailLines = 200
	}
	if maxChars <= 0 {
		maxChars = dockerDebugLogTailMaxChars
	}

	containerName := fmt.Sprintf("cpanel-%d", serverID)
	cmd := exec.Command("docker", "logs", "--tail", strconv.Itoa(tailLines), containerName)
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}

	text := strings.TrimSpace(string(outputBytes))
	if text == "" {
		return ""
	}

	if len(text) <= maxChars {
		return text
	}
	dropped := len(text) - maxChars
	return fmt.Sprintf("[... docker logs truncated %d chars ...]\n%s", dropped, text[len(text)-maxChars:])
}

func (s *Service) writeCrashBundle(serverID int, event string, state map[string]interface{}, logTail string) (string, error) {
	if serverID <= 0 {
		return "", fmt.Errorf("invalid server id")
	}

	if err := os.MkdirAll(s.crashPath, 0o755); err != nil {
		return "", err
	}

	timestamp := time.Now().UTC()
	fileName := fmt.Sprintf("server-%d-%s-%d.json", serverID, event, timestamp.Unix())
	absPath := filepath.Join(s.crashPath, fileName)

	payload := map[string]interface{}{
		"serverId":   serverID,
		"event":      event,
		"capturedAt": timestamp.Format(time.RFC3339),
		"state":      state,
		"logTail":    logTail,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(absPath, raw, 0o640); err != nil {
		return "", err
	}

	return absPath, nil
}
