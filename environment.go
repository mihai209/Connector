package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Environment topics for the event bus
const (
	EventEnvironmentStateChange = "environment:state_change"
	EventEnvironmentLog         = "environment:log"
	EventEnvironmentResources   = "environment:resources"
)

// Environment states
const (
	StateOffline  = "offline"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
)

// ProcessEnvironment defines the interface for different server runtime environments.
type ProcessEnvironment interface {
	// Type returns the name of the environment type (e.g., "docker").
	Type() string

	// Exists determines if the environment instance (e.g., container) is created.
	Exists() (bool, error)

	// IsRunning determines if the process is currently active.
	IsRunning(ctx context.Context) (bool, error)

	// Create initializes the environment for the server.
	Create(cfg ServerInstallConfig, serverPath string) error

	// Start begins the server process.
	Start(ctx context.Context) error

	// Stop gracefully terminates the server process.
	Stop(ctx context.Context, timeout time.Duration) error

	// Terminate forcibly stops the server process.
	Terminate(ctx context.Context) error

	// Destroy removes the environment instance.
	Destroy() error

	// SendCommand sends input to the process's stdin.
	SendCommand(command string) error

	// Attach begins streaming logs from the process.
	Attach(ctx context.Context) error

	// State returns the current cached state of the environment.
	State() string

	// SetState updates the cached state of the environment.
	SetState(state string)
}

// DockerEnvironment is an implementation of ProcessEnvironment using Docker containers.
type DockerEnvironment struct {
	svc           *Service
	serverID      int
	containerName string
	state         string
}

func NewDockerEnvironment(svc *Service, serverID int) *DockerEnvironment {
	return &DockerEnvironment{
		svc:           svc,
		serverID:      serverID,
		containerName: fmt.Sprintf("cpanel-%d", serverID),
		state:         StateOffline,
	}
}

func (e *DockerEnvironment) Type() string {
	return "docker"
}

func (e *DockerEnvironment) Exists() (bool, error) {
	_, err := runCommand("docker", "inspect", e.containerName)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (e *DockerEnvironment) IsRunning(ctx context.Context) (bool, error) {
	out, err := runCommand("docker", "inspect", "-f", "{{.State.Running}}", e.containerName)
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

func (e *DockerEnvironment) Create(cfg ServerInstallConfig, serverPath string) error {
	args := []string{"create", "--name", e.containerName}
	networkMode := strings.TrimSpace(e.svc.cfg.Docker.Network.Mode)
	if networkMode == "" {
		networkMode = strings.TrimSpace(e.svc.cfg.Docker.Network.Name)
	}

	hostname := normalizeBrandHostname(cfg.BrandName)
	args = append(args, "--hostname", hostname)
	domainname := strings.TrimSpace(e.svc.cfg.Docker.Domainname)
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
	for _, dns := range e.svc.effectiveContainerDNSServers() {
		args = append(args, "--dns", dns)
	}
	if e.svc.cfg.Docker.TmpfsSize > 0 {
		args = append(args, "--tmpfs", fmt.Sprintf("/tmp:rw,exec,nosuid,nodev,size=%dm", e.svc.cfg.Docker.TmpfsSize))
	}

	pidsLimit := int64(cfg.PidsLimit)
	if pidsLimit <= 0 {
		pidsLimit = e.svc.cfg.Docker.ContainerPidLimit
	}
	if pidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.FormatInt(pidsLimit, 10))
	}
	args = append(args, "--user", e.svc.chownUser())

	if cfg.Memory > 0 {
		memoryLimit := cfg.Memory + 128
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

	_, err := runCommand("docker", args...)
	if err != nil {
		if shouldRetryContainerCreateWithoutHostIP(err) {
			fallbackArgs, changed := buildContainerCreateArgsWithoutHostIP(args)
			if changed {
				e.svc.sendConsoleOutput(e.serverID, "\x1b[1;33m[!] Docker publish bind failed on allocation IP. Retrying with wildcard host bind (0.0.0.0)....\x1b[0m\n")
				_, err = runCommand("docker", fallbackArgs...)
			}
		}
	}
	return err
}

func (e *DockerEnvironment) Start(ctx context.Context) error {
	e.SetState(StateStarting)
	_, err := runCommand("docker", "start", e.containerName)
	if err != nil {
		e.SetState(StateOffline)
		return err
	}
	e.SetState(StateRunning)
	e.svc.repairServerContainerDNS(e.serverID)
	return nil
}

func (e *DockerEnvironment) Stop(ctx context.Context, timeout time.Duration) error {
	e.SetState(StateStopping)
	_, err := runCommand("docker", "stop", "-t", fmt.Sprintf("%d", int(timeout.Seconds())), e.containerName)
	if err != nil {
		return err
	}
	e.SetState(StateOffline)
	return nil
}

func (e *DockerEnvironment) Terminate(ctx context.Context) error {
	e.SetState(StateStopping)
	_, err := runCommand("docker", "kill", e.containerName)
	if err != nil {
		return err
	}
	e.SetState(StateOffline)
	return nil
}

func (e *DockerEnvironment) Destroy() error {
	_, err := runCommand("docker", "rm", "-f", e.containerName)
	return err
}

func (e *DockerEnvironment) SendCommand(command string) error {
	return e.svc.sendCommandToServerStdin(e.serverID, e.containerName, command)
}

func (e *DockerEnvironment) Attach(ctx context.Context) error {
	e.svc.ensureServerLogStream(e.serverID, false, true, false)
	return nil
}

func (e *DockerEnvironment) State() string {
	return e.state
}

func (e *DockerEnvironment) SetState(state string) {
	if e.state != state {
		e.state = state
		e.svc.events.Publish(fmt.Sprintf("%s:%d", EventEnvironmentStateChange, e.serverID), state)
	}
}

// Docker Specific Helper Functions

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
