package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	serverPath := filepath.Join(s.volumesPath, strconv.Itoa(serverID))
	if err := os.MkdirAll(serverPath, 0o755); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	if err := s.chownServerPath(serverPath); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not chown server path: %v\x1b[0m\n", err))
	}

	if err := s.runEggInstallation(serverID, serverPath, payload.Config); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	if err := s.applyEggConfigFiles(serverID, serverPath, payload.Config); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}
	if err := s.fixServerPermissions(serverPath); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not fix server permissions: %v\x1b[0m\n", err))
	}

	if err := s.pullImage(payload.Config.Image); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	if err := s.removeContainerIfExists(serverID); err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	containerID, err := s.createAndStartRuntimeContainer(serverID, serverPath, payload.Config)
	if err != nil {
		s.sendInstallFail(serverID, err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":        "install_success",
		"serverId":    serverID,
		"containerId": strings.TrimSpace(containerID),
	})

	time.Sleep(logAttachRetryDelay)
	s.ensureServerLogStream(serverID, false, true)
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

func (s *Service) chownServerPath(serverPath string) error {
	_, err := runCommand("chown", "-R", s.chownUser(), serverPath)
	return err
}

func (s *Service) createAndStartRuntimeContainer(serverID int, serverPath string, cfg ServerInstallConfig) (string, error) {
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	args := []string{"create", "--name", containerName}
	networkMode := strings.TrimSpace(s.cfg.Docker.Network.Mode)
	if networkMode == "" {
		networkMode = strings.TrimSpace(s.cfg.Docker.Network.Name)
	}

	hostname := normalizeBrandHostname(cfg.BrandName)
	args = append(args, "--hostname", hostname)
	domainname := strings.TrimSpace(s.cfg.Docker.Domainname)
	if domainname != "" {
		args = append(args, "--domainname", domainname)
	}
	for _, hostAlias := range buildContainerSelfHostAliases(hostname, domainname) {
		args = append(args, "--add-host", fmt.Sprintf("%s:127.0.0.1", hostAlias))
	}
	args = append(args, "-t", "-i")
	args = append(args, "-w", "/home/container")
	args = append(args, "-v", fmt.Sprintf("%s:/home/container", serverPath))
	for _, mount := range cfg.Mounts {
		source := strings.TrimSpace(mount.Source)
		target := strings.TrimSpace(mount.Target)
		if source == "" || target == "" {
			continue
		}
		if !strings.HasPrefix(target, "/") {
			target = "/" + strings.TrimLeft(target, "/")
		}
		mountArg := fmt.Sprintf("%s:%s", source, target)
		if mount.ReadOnly {
			mountArg += ":ro"
		}
		args = append(args, "-v", mountArg)
	}
	if networkMode != "" {
		args = append(args, "--network", networkMode)
	}
	for _, dns := range s.cfg.Docker.Network.DNS {
		if value := strings.TrimSpace(dns); value != "" {
			args = append(args, "--dns", value)
		}
	}
	if s.cfg.Docker.TmpfsSize > 0 {
		// Java/Netty/SQLite extract native libraries to /tmp and need execute permission.
		args = append(args, "--tmpfs", fmt.Sprintf("/tmp:rw,exec,nosuid,nodev,size=%dm", s.cfg.Docker.TmpfsSize))
	}

	pidsLimit := int64(cfg.PidsLimit)
	if pidsLimit <= 0 {
		pidsLimit = s.cfg.Docker.ContainerPidLimit
	}
	if pidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.FormatInt(pidsLimit, 10))
	}
	if s.cfg.Docker.Rootless.Enabled {
		uid := s.cfg.Docker.Rootless.ContainerUID
		gid := s.cfg.Docker.Rootless.ContainerGID
		if uid < 0 {
			uid = 0
		}
		if gid < 0 {
			gid = 0
		}
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, gid))
	}

	if cfg.Memory > 0 {
		memoryLimit := cfg.Memory + 128 // keep small overhead for JVM and similar runtimes
		args = append(args, "--memory", fmt.Sprintf("%dm", memoryLimit))

		switch {
		case cfg.SwapLimit < 0:
			args = append(args, "--memory-swap", "-1")
		case cfg.SwapLimit == 0:
			args = append(args, "--memory-swap", fmt.Sprintf("%dm", memoryLimit))
		default:
			args = append(args, "--memory-swap", fmt.Sprintf("%dm", memoryLimit+cfg.SwapLimit))
		}
	}
	if cfg.CPU > 0 {
		cpus := float64(cfg.CPU) / 100.0
		if cpus < 0.1 {
			cpus = 0.1
		}
		args = append(args, "--cpus", fmt.Sprintf("%.2f", cpus))
	}
	if cfg.IOWeight >= 10 && cfg.IOWeight <= 1000 {
		args = append(args, "--blkio-weight", strconv.Itoa(cfg.IOWeight))
	}
	if cfg.OOMKillDisable {
		args = append(args, "--oom-kill-disable")
	}
	if cfg.OOMScoreAdj >= -1000 && cfg.OOMScoreAdj <= 1000 && cfg.OOMScoreAdj != 0 {
		args = append(args, "--oom-score-adj", strconv.Itoa(cfg.OOMScoreAdj))
	}

	if !strings.EqualFold(networkMode, "host") {
		for _, port := range cfg.Ports {
			if port.Host <= 0 || port.Container <= 0 {
				continue
			}
			protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
			if protocol == "" {
				protocol = "tcp"
			}
			args = append(args, "-p", buildDockerPublishArg(port.IP, port.Host, port.Container, protocol))
		}
	}

	for key, value := range cfg.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, stringifyEnvValue(value)))
	}
	args = append(args, "-e", fmt.Sprintf("STARTUP=%s", cfg.Startup))

	args = append(args, cfg.Image)
	if strings.EqualFold(strings.TrimSpace(cfg.StartupMode), "command") && strings.TrimSpace(cfg.Startup) != "" {
		args = append(args, "/bin/bash", "-lc", cfg.Startup)
	}

	containerID, err := runCommand("docker", args...)
	if err != nil {
		if shouldRetryContainerCreateWithoutHostIP(err) {
			fallbackArgs, changed := buildContainerCreateArgsWithoutHostIP(args)
			if changed {
				s.sendConsoleOutput(serverID, "\x1b[1;33m[!] Docker publish bind failed on allocation IP. Retrying with wildcard host bind (0.0.0.0)...\x1b[0m\n")
				containerID, err = runCommand("docker", fallbackArgs...)
			}
		}
	}
	if err != nil {
		return "", err
	}

	if _, err := runCommand("docker", "start", containerName); err != nil {
		return "", err
	}
	return strings.TrimSpace(containerID), nil
}

func buildContainerSelfHostAliases(hostname, domainname string) []string {
	seen := make(map[string]struct{})
	aliases := make([]string, 0, 2)
	add := func(raw string) {
		value := strings.ToLower(strings.TrimSpace(raw))
		value = strings.Trim(value, ".")
		if value == "" || strings.ContainsAny(value, " \t\r\n") {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		aliases = append(aliases, value)
	}

	add(hostname)
	if strings.TrimSpace(hostname) != "" && strings.TrimSpace(domainname) != "" {
		add(strings.Trim(strings.TrimSpace(hostname), ".") + "." + strings.Trim(strings.TrimSpace(domainname), "."))
	}
	return aliases
}

func stringifyEnvValue(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		raw, _ := json.Marshal(v)
		if len(raw) == 0 {
			return ""
		}
		if string(raw) == "null" {
			return ""
		}
		return string(raw)
	}
}

func buildDockerPublishArg(hostIP string, hostPort, containerPort int, protocol string) string {
	host := strings.TrimSpace(hostIP)
	if host == "" {
		return fmt.Sprintf("%d:%d/%s", hostPort, containerPort, protocol)
	}

	// Docker expects IPv6 bind addresses wrapped in [].
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}

	return fmt.Sprintf("%s:%d:%d/%s", host, hostPort, containerPort, protocol)
}

func shouldRetryContainerCreateWithoutHostIP(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "cannot assign requested address")
}

func buildContainerCreateArgsWithoutHostIP(args []string) ([]string, bool) {
	if len(args) == 0 {
		return args, false
	}
	rewritten := make([]string, 0, len(args))
	changed := false
	for i := 0; i < len(args); i++ {
		current := args[i]
		if current == "-p" && (i+1) < len(args) {
			mapped, didStrip := stripHostIPFromDockerPublishArg(args[i+1])
			rewritten = append(rewritten, "-p", mapped)
			if didStrip {
				changed = true
			}
			i++
			continue
		}
		rewritten = append(rewritten, current)
	}
	return rewritten, changed
}

func stripHostIPFromDockerPublishArg(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return value, false
	}

	// IPv6 publish format: [addr]:hostPort:containerPort/proto
	if strings.HasPrefix(value, "[") {
		closing := strings.Index(value, "]:")
		if closing == -1 {
			return value, false
		}
		tail := strings.TrimSpace(value[closing+2:])
		if strings.Count(tail, ":") == 1 && strings.Contains(tail, "/") {
			return tail, true
		}
		return value, false
	}

	// IPv4/hostname publish format: hostIP:hostPort:containerPort/proto
	if strings.Count(value, ":") >= 2 {
		first := strings.Index(value, ":")
		if first > 0 {
			tail := strings.TrimSpace(value[first+1:])
			if strings.Count(tail, ":") == 1 && strings.Contains(tail, "/") {
				return tail, true
			}
		}
	}

	return value, false
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
	if networkMode != "" {
		args = append(args, "--network", networkMode)
	}
	for _, dns := range s.cfg.Docker.Network.DNS {
		if value := strings.TrimSpace(dns); value != "" {
			args = append(args, "--dns", value)
		}
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

	output, err := runCommand("docker", args...)
	s.sendConsoleOutput(serverID, output+"\n")
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
