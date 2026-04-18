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

const runtimeConfigSnapshotFile = ".cpanel_runtime.json"

func (s *Service) handleInstallServer(message map[string]interface{}) {
	var payload ServerInstallMessage
	if err := s.marshalMessage(message, &payload); err != nil {
		s.sendInstallFail(0, fmt.Sprintf("invalid install payload: %v", err))
		return
	}
	if payload.ServerID <= 0 {
		return
	}

	serverID := payload.ServerID
	if err := s.beginInstall(serverID, payload.Reinstall); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Install dispatch rejected: %v\x1b[0m\n", err))
		s.sendInstallFail(serverID, err.Error())
		return
	}
	defer s.finishInstall(serverID)

	serverPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID))
	if err := os.MkdirAll(serverPath, 0o755); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	if err := s.chownServerPath(serverPath); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not chown server path: %v\x1b[0m\n", err))
	}

	if payload.Reinstall {
		s.sendConsoleOutput(serverID, "\x1b[1;34m[*] Preparing server reinstall...\x1b[0m\n")
		if err := s.ensureContainerStoppedForReinstall(serverID); err != nil {
			s.sendInstallFail(serverID, err.Error())
			return
		}
	}

	if payload.Config.SkipInstallationScript {
		s.sendConsoleOutput(serverID, "\x1b[1;34m[*] Skipping egg installation script (migration file import mode).\x1b[0m\n")
	} else {
		if err := s.runEggInstallation(serverID, serverPath, payload.Config); err != nil {
			s.sendInstallFail(serverID, err.Error())
			return
		}
	}

	if err := s.applyEggConfigFiles(serverID, serverPath, payload.Config); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}
	if err := s.fixServerPermissions(serverPath); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not fix server permissions: %v\x1b[0m\n", err))
	}
	if err := s.persistRuntimeConfigSnapshot(serverID, payload.Config); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not persist runtime config snapshot: %v\x1b[0m\n", err))
	}

	if err := s.pullImage(payload.Config.Image); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	if err := s.removeContainerIfExists(serverID); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	startAfterInstall := true
	if payload.Config.StartAfterInstall != nil {
		startAfterInstall = *payload.Config.StartAfterInstall
	}

	env := s.Environment(serverID)
	if err := env.Create(payload.Config, serverPath); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":        "install_success",
		"serverId":    serverID,
		"started":     startAfterInstall,
		"status":      map[bool]string{true: "running", false: "offline"}[startAfterInstall],
	})

	if startAfterInstall {
		time.Sleep(logAttachRetryDelay)
		_ = env.Start(context.Background())
		_ = env.Attach(context.Background())
	}
}

func (s *Service) sendInstallFail(serverID int, reason string) {
	if serverID <= 0 {
		serverID = -1
	}
	_ = s.sendJSON(map[string]interface{}{
		"type":     "install_fail",
		"serverId": serverID,
		"error":    reason,
	})
}

func (s *Service) pullImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("runtime image is missing")
	}
	_, err := runCommand("docker", "pull", image)
	return err
}

func (s *Service) removeContainerIfExists(serverID int) error {
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	_, _ = runCommand("docker", "stop", containerName)
	_, _ = runCommand("docker", "rm", "-f", containerName)
	return nil
}

func (s *Service) ensureContainerStoppedForReinstall(serverID int) error {
	env := s.Environment(serverID)
	running, _ := env.IsRunning(context.Background())
	if !running {
		return nil
	}

	s.sendConsoleOutput(serverID, "\x1b[1;34m[*] Waiting for server to stop before reinstall...\x1b[0m\n")
	if err := env.Stop(context.Background(), 30*time.Second); err != nil {
		return fmt.Errorf("reinstall: failed to stop running container: %w", err)
	}
	s.cleanupServerStreams(serverID)

	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		running, _ := env.IsRunning(context.Background())
		if !running {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("reinstall: timed out waiting for container to stop")
}

func (s *Service) runtimeConfigSnapshotPath(serverID int) string {
	return filepath.Join(s.volumesPath, strconv.Itoa(serverID), runtimeConfigSnapshotFile)
}

func (s *Service) persistRuntimeConfigSnapshot(serverID int, cfg ServerInstallConfig) error {
	if serverID <= 0 {
		return fmt.Errorf("invalid serverID")
	}
	path := s.runtimeConfigSnapshotPath(serverID)
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		return err
	}
	_, _ = runCommand("chown", s.chownUser(), path)
	return nil
}

func (s *Service) loadRuntimeConfigSnapshot(serverID int) (ServerInstallConfig, error) {
	var cfg ServerInstallConfig
	if serverID <= 0 {
		return cfg, fmt.Errorf("invalid serverID")
	}
	raw, err := os.ReadFile(s.runtimeConfigSnapshotPath(serverID))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (s *Service) chownServerPath(serverPath string) error {
	_, err := runCommand("chown", "-R", s.chownUser(), serverPath)
	return err
}

func (s *Service) Environment(serverID int) ProcessEnvironment {
	s.environmentsMu.Lock()
	defer s.environmentsMu.Unlock()

	if env, ok := s.environments[serverID]; ok {
		return env
	}

	env := NewDockerEnvironment(s, serverID)
	s.environments[serverID] = env
	return env
}

func (s *Service) runEggInstallation(serverID int, serverPath string, cfg ServerInstallConfig) error {
	installation := resolveInstallationPayload(cfg)
	script := strings.TrimSpace(asString(installation["script"]))
	if script == "" {
		return nil
	}

	installerImage := strings.TrimSpace(asString(installation["container"]))
	if installerImage == "" {
		installerImage = cfg.Image
	}

	s.sendConsoleOutput(serverID, "\x1b[1;34m[*] Running egg installation script...\x1b[0m\n")
	if err := s.pullImage(installerImage); err != nil {
		return err
	}

	scriptPath := filepath.Join(serverPath, ".cpanel_install.sh")
	scriptBody := strings.ReplaceAll(script, "\r\n", "\n")
	scriptBody = normalizeInstallationScript(scriptBody)
	if !strings.HasSuffix(scriptBody, "\n") {
		scriptBody += "\n"
	}
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		return err
	}
	_, _ = runCommand("chown", s.chownUser(), scriptPath)

	installerName := fmt.Sprintf("cpanel-installer-%d-%d", serverID, time.Now().Unix())
	args := []string{"run", "--rm", "--name", installerName, "-v", fmt.Sprintf("%s:/mnt/server", serverPath), "-w", "/mnt/server"}
	networkMode := strings.TrimSpace(s.cfg.Docker.Network.Mode)
	if networkMode == "" {
		networkMode = strings.TrimSpace(s.cfg.Docker.Network.Name)
	}
	// Force installers to use bridge network if the system-wide network is internal.
	// This ensures dependencies can be downloaded during the installation phase.
	if s.cfg.Docker.Network.IsInternal {
		bootInfo("forcing 'bridge' network for installer session due to internal network configuration")
		networkMode = "bridge"
	}
	if networkMode != "" {
		args = append(args, "--network", networkMode)
	}
	for _, dns := range s.effectiveContainerDNSServers() {
		args = append(args, "--dns", dns)
	}

	for key, value := range cfg.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, stringifyEnvValue(value)))
	}
	args = append(args, "-e", fmt.Sprintf("STARTUP=%s", cfg.Startup))

	entrypoint := resolveEntrypointPayload(installation["entrypoint"])
	if len(entrypoint) > 0 {
		args = append(args, "--entrypoint", entrypoint[0])
	}

	args = append(args, installerImage)
	if len(entrypoint) > 1 {
		args = append(args, entrypoint[1:]...)
	}
	args = append(args, "/mnt/server/.cpanel_install.sh")

	err := streamCommandOutput("docker", args, func(line string) {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			return
		}
		s.sendConsoleOutput(serverID, trimmed+"\n")
	})
	_ = os.Remove(scriptPath)
	if err != nil {
		return fmt.Errorf("egg installation failed: %w", err)
	}

	s.sendConsoleOutput(serverID, "\x1b[1;32m[✓] Egg installation completed.\x1b[0m\n")
	return nil
}

func normalizeInstallationScript(script string) string {
	normalized := strings.ReplaceAll(script, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "mkdir ") && !strings.Contains(trimmed, " -p") && !strings.Contains(trimmed, "--parents") {
			lines[idx] = strings.Replace(line, "mkdir ", "mkdir -p ", 1)
			trimmed = strings.TrimSpace(lines[idx])
		}

		if strings.Contains(trimmed, "db.getSiblingDB('admin').createUser(") && !strings.Contains(trimmed, "|| true") {
			lines[idx] = strings.TrimRight(line, " \t") + " || true"
			trimmed = strings.TrimSpace(lines[idx])
		}
		if strings.Contains(trimmed, "db.getSiblingDB('admin').shutdownServer()") && !strings.Contains(trimmed, "|| true") {
			lines[idx] = strings.TrimRight(line, " \t") + " || true"
		}
	}
	return strings.Join(lines, "\n")
}

func resolveInstallationPayload(cfg ServerInstallConfig) map[string]interface{} {
	if len(cfg.EggScripts) > 0 {
		if installation := asMap(cfg.EggScripts["installation"]); installation != nil {
			return installation
		}
	}
	if len(cfg.Installation) > 0 {
		return cfg.Installation
	}
	return nil
}

func resolveEntrypointPayload(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	if str := strings.TrimSpace(asString(raw)); str != "" && !strings.HasPrefix(str, "[") {
		return strings.Fields(str)
	}

	items := asSlice(raw)
	if len(items) == 0 {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(asString(item))
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
