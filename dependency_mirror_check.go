package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const dependencyMirrorMaxHosts = 16

var defaultDependencyMirrorHosts = []string{
	"mohistmc.github.io",
	"api.github.com",
	"api.modrinth.com",
	"api.spigotmc.org",
	"maven.minecraftforge.net",
	"repo.spongepowered.org",
	"libraries.minecraft.net",
}

type dependencyMirrorContainerState struct {
	Name             string   `json:"name"`
	Exists           bool     `json:"exists"`
	Running          bool     `json:"running"`
	Status           string   `json:"status"`
	ExitCode         int      `json:"exitCode"`
	OOMKilled        bool     `json:"oomKilled"`
	NetworkMode      string   `json:"networkMode"`
	Networks         []string `json:"networks"`
	DNSConfigured    []string `json:"dnsConfigured"`
	ResolvNameserver []string `json:"resolvNameservers"`
	ResolvReadError  string   `json:"resolvReadError,omitempty"`
	InspectError     string   `json:"inspectError,omitempty"`
}

type dependencyMirrorHostResult struct {
	Host        string   `json:"host"`
	DNSResolved bool     `json:"dnsResolved"`
	ResolvedIPs []string `json:"resolvedIps,omitempty"`
	Reachable   bool     `json:"reachable"`
	Transport   string   `json:"transport,omitempty"`
	LatencyMs   int64    `json:"latencyMs"`
	Error       string   `json:"error,omitempty"`
}

type dependencyMirrorSummary struct {
	TotalMirrors       int    `json:"totalMirrors"`
	DNSResolvedMirrors int    `json:"dnsResolvedMirrors"`
	ReachableMirrors   int    `json:"reachableMirrors"`
	LikelyRootCause    string `json:"likelyRootCause"`
}

func normalizeDependencyMirrorHostForCheck(raw string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		parsedHost := strings.TrimPrefix(strings.TrimPrefix(value, "http://"), "https://")
		slashIdx := strings.Index(parsedHost, "/")
		if slashIdx >= 0 {
			parsedHost = parsedHost[:slashIdx]
		}
		value = parsedHost
	}
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")
	if host, _, err := net.SplitHostPort(value); err == nil && strings.TrimSpace(host) != "" {
		value = strings.TrimSpace(host)
	}
	if value == "" || len(value) > 255 {
		return ""
	}
	if strings.ContainsAny(value, " \t\r\n/\\") {
		return ""
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '-' {
			continue
		}
		return ""
	}
	if !strings.Contains(value, ".") {
		return ""
	}
	return value
}

func dedupeDependencyMirrorHosts(candidates []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(candidates))
	for _, item := range candidates {
		host := normalizeDependencyMirrorHostForCheck(item)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
		if len(out) >= dependencyMirrorMaxHosts {
			break
		}
	}
	return out
}

func parseNameserversFromResolvConf(content string) []string {
	lines := strings.Split(content, "\n")
	seen := make(map[string]struct{})
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.Fields(trimmed)
		if len(parts) < 2 || strings.ToLower(parts[0]) != "nameserver" {
			continue
		}
		server := strings.TrimSpace(parts[1])
		if server == "" {
			continue
		}
		if _, exists := seen[server]; exists {
			continue
		}
		seen[server] = struct{}{}
		out = append(out, server)
	}
	return out
}

func inspectDependencyMirrorContainer(serverID int) dependencyMirrorContainerState {
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	state := dependencyMirrorContainerState{
		Name: containerName,
	}

	rawInspect, err := runCommand("docker", "inspect", containerName)
	if err != nil {
		state.Exists = false
		state.InspectError = err.Error()
		return state
	}

	var inspectRows []struct {
		State struct {
			Running   bool   `json:"Running"`
			Status    string `json:"Status"`
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
		} `json:"State"`
		HostConfig struct {
			NetworkMode string   `json:"NetworkMode"`
			Dns         []string `json:"Dns"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]interface{} `json:"Networks"`
		} `json:"NetworkSettings"`
	}

	if err := json.Unmarshal([]byte(rawInspect), &inspectRows); err != nil {
		state.Exists = false
		state.InspectError = fmt.Sprintf("parse docker inspect failed: %v", err)
		return state
	}
	if len(inspectRows) == 0 {
		state.Exists = false
		state.InspectError = "container inspect returned no rows"
		return state
	}

	row := inspectRows[0]
	state.Exists = true
	state.Running = row.State.Running
	state.Status = strings.TrimSpace(row.State.Status)
	state.ExitCode = row.State.ExitCode
	state.OOMKilled = row.State.OOMKilled
	state.NetworkMode = strings.TrimSpace(row.HostConfig.NetworkMode)
	state.DNSConfigured = append([]string{}, row.HostConfig.Dns...)
	for name := range row.NetworkSettings.Networks {
		name = strings.TrimSpace(name)
		if name != "" {
			state.Networks = append(state.Networks, name)
		}
	}
	sort.Strings(state.Networks)

	if !state.Running {
		return state
	}

	resolvRaw, resolvErr := runCommand("docker", "exec", "--user", "0", containerName, "sh", "-lc", "cat /etc/resolv.conf")
	if resolvErr != nil {
		state.ResolvReadError = resolvErr.Error()
		return state
	}
	state.ResolvNameserver = parseNameserversFromResolvConf(resolvRaw)
	return state
}

func probeDependencyMirrorHost(host string) dependencyMirrorHostResult {
	startedAt := time.Now()
	result := dependencyMirrorHostResult{
		Host: strings.TrimSpace(host),
	}
	if result.Host == "" {
		result.Error = "empty host"
		result.LatencyMs = time.Since(startedAt).Milliseconds()
		return result
	}

	ips, dnsErr := net.LookupHost(result.Host)
	if dnsErr != nil {
		result.Error = fmt.Sprintf("dns lookup failed: %v", dnsErr)
		result.LatencyMs = time.Since(startedAt).Milliseconds()
		return result
	}

	result.DNSResolved = true
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		result.ResolvedIPs = append(result.ResolvedIPs, ip)
		if len(result.ResolvedIPs) >= 4 {
			break
		}
	}

	conn443, err443 := net.DialTimeout("tcp", net.JoinHostPort(result.Host, "443"), 4*time.Second)
	if err443 == nil {
		result.Reachable = true
		result.Transport = "tcp/443"
		_ = conn443.Close()
		result.LatencyMs = time.Since(startedAt).Milliseconds()
		return result
	}

	conn80, err80 := net.DialTimeout("tcp", net.JoinHostPort(result.Host, "80"), 4*time.Second)
	if err80 == nil {
		result.Reachable = true
		result.Transport = "tcp/80"
		_ = conn80.Close()
		result.LatencyMs = time.Since(startedAt).Milliseconds()
		return result
	}

	result.Error = fmt.Sprintf("connect failed: 443=%v; 80=%v", err443, err80)
	result.LatencyMs = time.Since(startedAt).Milliseconds()
	return result
}

func probeDependencyMirrorHosts(hosts []string) []dependencyMirrorHostResult {
	results := make([]dependencyMirrorHostResult, len(hosts))
	var wg sync.WaitGroup
	for idx, host := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			results[i] = probeDependencyMirrorHost(h)
		}(idx, host)
	}
	wg.Wait()
	return results
}

func buildDependencyMirrorSummary(container dependencyMirrorContainerState, mirrors []dependencyMirrorHostResult) dependencyMirrorSummary {
	summary := dependencyMirrorSummary{
		TotalMirrors: len(mirrors),
	}
	for _, mirror := range mirrors {
		if mirror.DNSResolved {
			summary.DNSResolvedMirrors++
		}
		if mirror.Reachable {
			summary.ReachableMirrors++
		}
	}

	switch {
	case !container.Exists:
		summary.LikelyRootCause = "Container is missing; install/runtime environment is not present."
	case container.Running && len(container.Networks) == 0:
		summary.LikelyRootCause = "Container has no Docker network attached."
	case summary.TotalMirrors > 0 && summary.ReachableMirrors == 0:
		summary.LikelyRootCause = "Connector host cannot reach dependency mirrors (DNS and/or outbound network blocked)."
	case container.Running && len(container.ResolvNameserver) == 0:
		summary.LikelyRootCause = "Container resolv.conf has no nameserver entries."
	case container.Running && container.ResolvReadError != "":
		summary.LikelyRootCause = "Unable to read /etc/resolv.conf from running container."
	case summary.TotalMirrors > 0 && summary.ReachableMirrors < summary.TotalMirrors:
		summary.LikelyRootCause = "Some mirrors are reachable, some are failing (partial network or upstream outage)."
	case container.Running:
		summary.LikelyRootCause = "Connector host can reach mirrors; issue is likely inside container DNS/runtime path."
	default:
		summary.LikelyRootCause = "Mirror connectivity looks healthy from connector host."
	}

	return summary
}

func (s *Service) handleDependencyMirrorCheck(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if serverID <= 0 {
		return
	}

	s.sendActionAck(serverID, "dependency_mirror_check", "accepted", "Dependency mirror check accepted.", requestID, nil)

	inputHosts := asStringSlice(message["hosts"])
	if len(inputHosts) == 0 {
		inputHosts = append([]string{}, defaultDependencyMirrorHosts...)
	}
	hosts := dedupeDependencyMirrorHosts(inputHosts)
	if len(hosts) == 0 {
		hosts = dedupeDependencyMirrorHosts(defaultDependencyMirrorHosts)
	}
	if len(hosts) > dependencyMirrorMaxHosts {
		hosts = hosts[:dependencyMirrorMaxHosts]
	}

	container := inspectDependencyMirrorContainer(serverID)
	mirrors := probeDependencyMirrorHosts(hosts)
	summary := buildDependencyMirrorSummary(container, mirrors)

	s.sendActionAck(
		serverID,
		"dependency_mirror_check",
		"executed",
		fmt.Sprintf("Dependency mirror check completed (%d/%d reachable).", summary.ReachableMirrors, summary.TotalMirrors),
		requestID,
		map[string]interface{}{
			"reachableMirrors": summary.ReachableMirrors,
			"totalMirrors":     summary.TotalMirrors,
		},
	)

	_ = s.sendJSON(map[string]interface{}{
		"type":      "dependency_mirror_check_result",
		"serverId":  serverID,
		"requestId": requestID,
		"success":   true,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"container": container,
		"mirrors":   mirrors,
		"summary":   summary,
	})
}
