package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

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

		_ = s.sendJSON(map[string]interface{}{
			"type":     "server_status_update",
			"serverId": serverID,
			"status":   status,
		})

		if action == "start" {
			go func(id int) {
				time.Sleep(logAttachRetryDelay)
				s.ensureServerLogStream(id, false, true)
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
