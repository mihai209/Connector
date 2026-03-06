package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

const (
	serverContainerNamePrefix = "cpanel-"
	dockerEmbeddedDNS         = "127.0.0.11"
)

func (s *Service) repairRunningContainersDNS() {
	namesRaw, err := runCommand("docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		bootWarn("dns repair sweep skipped: cannot list running containers error=%v", err)
		return
	}

	servers := s.resolveDNSServersForRepair()
	if len(servers) == 0 {
		bootWarn("dns repair sweep skipped: no usable DNS servers detected")
		return
	}

	total := 0
	fixed := 0
	failed := 0

	for _, line := range strings.Split(namesRaw, "\n") {
		containerName := strings.TrimSpace(line)
		if containerName == "" || !strings.HasPrefix(containerName, serverContainerNamePrefix) {
			continue
		}
		total++
		if err := s.repairContainerDNS(containerName, servers); err != nil {
			failed++
			bootWarn("dns repair failed container=%s error=%v", containerName, err)
			continue
		}
		fixed++
	}

	if total > 0 {
		bootInfo("dns repair sweep completed total=%d fixed=%d failed=%d", total, fixed, failed)
	}
}

func (s *Service) repairServerContainerDNS(serverID int) {
	if serverID <= 0 {
		return
	}
	servers := s.resolveDNSServersForRepair()
	if len(servers) == 0 {
		return
	}
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	if err := s.repairContainerDNS(containerName, servers); err != nil {
		bootWarn("dns repair failed server=%d container=%s error=%v", serverID, containerName, err)
	}
}

func (s *Service) repairContainerDNS(containerName string, servers []string) error {
	containerName = strings.TrimSpace(containerName)
	if containerName == "" {
		return fmt.Errorf("empty container name")
	}
	if len(servers) == 0 {
		return fmt.Errorf("no dns servers provided")
	}

	var content strings.Builder
	content.WriteString("# Managed by CPanel connector DNS repair.\n")
	for _, server := range servers {
		content.WriteString("nameserver ")
		content.WriteString(server)
		content.WriteString("\n")
	}
	content.WriteString("options ndots:0 timeout:2 attempts:2\n")

	_, err := runCommandWithInput(content.String(), "docker", "exec", "-i", "--user", "0", containerName, "sh", "-lc", "cat > /etc/resolv.conf")
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) resolveDNSServersForRepair() []string {
	servers := []string{dockerEmbeddedDNS}
	seen := map[string]struct{}{dockerEmbeddedDNS: {}}

	for _, value := range normalizeDNSServers(s.cfg.Docker.Network.DNS) {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		servers = append(servers, value)
	}

	// If no explicit DNS was configured, append host resolvers (if usable).
	if len(servers) == 1 {
		for _, value := range normalizeDNSServers(readHostDNSServers()) {
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			servers = append(servers, value)
		}
	}

	return servers
}

// effectiveContainerDNSServers returns resolvers to pass at docker create/run time.
// Keep Docker embedded DNS first so name resolution works even when public resolvers
// are blocked in provider environments.
func (s *Service) effectiveContainerDNSServers() []string {
	servers := []string{dockerEmbeddedDNS}
	seen := map[string]struct{}{dockerEmbeddedDNS: {}}
	for _, value := range normalizeDNSServers(s.cfg.Docker.Network.DNS) {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		servers = append(servers, value)
	}
	return servers
}

func readHostDNSServers() []string {
	candidates := []string{
		"/run/systemd/resolve/resolv.conf",
		"/etc/resolv.conf",
	}

	collected := make([]string, 0, 4)
	for _, path := range candidates {
		parsed, err := parseNameserversFromResolv(path)
		if err != nil || len(parsed) == 0 {
			continue
		}
		collected = append(collected, parsed...)
	}
	return collected
}

func parseNameserversFromResolv(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := make([]string, 0, 4)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.ToLower(fields[0]) != "nameserver" {
			continue
		}
		out = append(out, fields[1])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeDNSServers(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		parsed := net.ParseIP(value)
		if parsed == nil || parsed.IsUnspecified() || parsed.IsLoopback() {
			continue
		}
		canonical := parsed.String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return normalized
}
