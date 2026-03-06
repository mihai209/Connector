package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func loadConfig(configPath string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}

	cfg.Panel.URL = strings.TrimSpace(cfg.Panel.URL)
	cfg.Panel.AllowedURLs = normalizeAllowedOrigins(cfg.Panel.AllowedURLs)
	cfg.Panel.AllowedOrigins = normalizeAllowedOrigins(cfg.Panel.AllowedOrigins)
	cfg.API.Host = strings.TrimSpace(cfg.API.Host)
	cfg.API.AllowedOrigins = normalizeAllowedOrigins(cfg.API.AllowedOrigins)
	cfg.API.AllowedOriginsLegacy = normalizeAllowedOrigins(cfg.API.AllowedOriginsLegacy)
	cfg.API.TrustedProxies = normalizeTrustedProxies(cfg.API.TrustedProxies)
	cfg.Connector.Token = strings.TrimSpace(cfg.Connector.Token)
	cfg.SFTP.Host = strings.TrimSpace(cfg.SFTP.Host)
	cfg.SFTP.Directory = strings.TrimSpace(cfg.SFTP.Directory)
	cfg.SFTP.HostKeyPath = strings.TrimSpace(cfg.SFTP.HostKeyPath)
	cfg.API.SSL.CertFile = strings.TrimSpace(cfg.API.SSL.CertFile)
	cfg.API.SSL.KeyFile = strings.TrimSpace(cfg.API.SSL.KeyFile)
	cfg.Docker.Domainname = strings.TrimSpace(cfg.Docker.Domainname)
	cfg.Docker.Network.Name = strings.TrimSpace(cfg.Docker.Network.Name)
	cfg.Docker.Network.Driver = strings.TrimSpace(cfg.Docker.Network.Driver)
	cfg.Docker.Network.Mode = strings.TrimSpace(cfg.Docker.Network.Mode)
	cfg.Docker.Network.Interface = strings.TrimSpace(cfg.Docker.Network.Interface)
	cfg.Docker.Network.Interfaces.V4.Subnet = strings.TrimSpace(cfg.Docker.Network.Interfaces.V4.Subnet)
	cfg.Docker.Network.Interfaces.V4.Gateway = strings.TrimSpace(cfg.Docker.Network.Interfaces.V4.Gateway)
	cfg.Docker.Network.Interfaces.V6.Subnet = strings.TrimSpace(cfg.Docker.Network.Interfaces.V6.Subnet)
	cfg.Docker.Network.Interfaces.V6.Gateway = strings.TrimSpace(cfg.Docker.Network.Interfaces.V6.Gateway)

	if cfg.Panel.URL == "" || cfg.Connector.ID <= 0 || cfg.Connector.Token == "" {
		return cfg, errors.New("missing required config fields: panel.url, connector.id, connector.token")
	}

	mergedAllowedOrigins := normalizeAllowedOrigins(
		cfg.Panel.AllowedURLs,
		cfg.Panel.AllowedOrigins,
		cfg.API.AllowedOrigins,
		cfg.API.AllowedOriginsLegacy,
	)
	if len(mergedAllowedOrigins) == 0 {
		panelOrigin, err := extractURLOrigin(cfg.Panel.URL)
		if err != nil {
			return cfg, fmt.Errorf("panel.url origin parse failed: %w", err)
		}
		mergedAllowedOrigins = append(mergedAllowedOrigins, panelOrigin)
	}
	cfg.Panel.AllowedURLs = mergedAllowedOrigins

	if cfg.API.Host == "" {
		cfg.API.Host = "0.0.0.0"
	}
	if cfg.SFTP.Host == "" {
		cfg.SFTP.Host = defaultSFTPBindHost
	}
	if cfg.SFTP.Port <= 0 {
		cfg.SFTP.Port = defaultSFTPPort
	}
	if cfg.SFTP.HostKeyPath == "" {
		cfg.SFTP.HostKeyPath = defaultSFTPHostKeyPath
	}
	if cfg.SFTP.Directory == "" {
		cfg.SFTP.Directory = defaultVolumesPath
	}
	if cfg.Docker.Network.Name == "" {
		cfg.Docker.Network.Name = defaultDockerNetworkName
	}
	if cfg.Docker.Network.Driver == "" {
		cfg.Docker.Network.Driver = defaultDockerNetworkDriver
	}
	if cfg.Docker.Network.Mode == "" {
		cfg.Docker.Network.Mode = cfg.Docker.Network.Name
	}
	if cfg.Docker.Network.Interface == "" {
		cfg.Docker.Network.Interface = defaultDockerNetworkInterface
	}
	if cfg.Docker.Network.Interfaces.V4.Subnet == "" {
		cfg.Docker.Network.Interfaces.V4.Subnet = defaultDockerNetworkV4Subnet
	}
	if cfg.Docker.Network.Interfaces.V4.Gateway == "" {
		cfg.Docker.Network.Interfaces.V4.Gateway = defaultDockerNetworkV4Gateway
	}
	if cfg.Docker.Network.EnableIPv6 {
		if cfg.Docker.Network.Interfaces.V6.Subnet == "" {
			cfg.Docker.Network.Interfaces.V6.Subnet = defaultDockerNetworkV6Subnet
		}
		if cfg.Docker.Network.Interfaces.V6.Gateway == "" {
			cfg.Docker.Network.Interfaces.V6.Gateway = defaultDockerNetworkV6Gateway
		}
	}

	normalizedDNS := make([]string, 0, len(cfg.Docker.Network.DNS))
	seenDNS := make(map[string]struct{})
	for _, dns := range cfg.Docker.Network.DNS {
		value := strings.TrimSpace(dns)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		// "auto"/"host"/"inherit" explicitly means "do not force --dns",
		// allowing Docker daemon/host resolver settings to be used.
		if lower == "auto" || lower == "host" || lower == "inherit" {
			normalizedDNS = normalizedDNS[:0]
			seenDNS = map[string]struct{}{}
			break
		}
		if _, exists := seenDNS[value]; exists {
			continue
		}
		seenDNS[value] = struct{}{}
		normalizedDNS = append(normalizedDNS, value)
	}
	// If empty, runtime containers inherit Docker daemon DNS instead of forcing public resolvers.
	cfg.Docker.Network.DNS = normalizedDNS

	if cfg.Docker.Network.EnableICC == nil {
		defaultICC := true
		cfg.Docker.Network.EnableICC = &defaultICC
	}
	if cfg.Docker.Network.NetworkMTU <= 0 {
		cfg.Docker.Network.NetworkMTU = defaultDockerNetworkMTU
	}
	if cfg.Docker.TmpfsSize == 0 {
		cfg.Docker.TmpfsSize = defaultContainerTmpfsSizeMB
	}
	if cfg.Docker.ContainerPidLimit <= 0 {
		cfg.Docker.ContainerPidLimit = defaultContainerPidLimit
	}
	if cfg.System.DiskCheckTtlSeconds <= 0 {
		cfg.System.DiskCheckTtlSeconds = defaultDiskUsageCacheTTLSeconds
	}
	if cfg.System.WSReadLimitMB <= 0 {
		cfg.System.WSReadLimitMB = defaultWSReadLimitMB
	}
	if envLimit := strings.TrimSpace(os.Getenv("CONNECTOR_WS_READ_LIMIT_MB")); envLimit != "" {
		if parsed, err := strconv.ParseInt(envLimit, 10, 64); err == nil && parsed > 0 {
			cfg.System.WSReadLimitMB = parsed
		}
	}
	if cfg.System.WSReadLimitMB < 8 {
		cfg.System.WSReadLimitMB = 8
	}
	if cfg.System.WSReadLimitMB > 1024 {
		cfg.System.WSReadLimitMB = 1024
	}
	if cfg.Transfers.DownloadLimit < 0 {
		cfg.Transfers.DownloadLimit = 0
	}
	if cfg.Throttles.Enabled == nil {
		enabled := true
		cfg.Throttles.Enabled = &enabled
	}
	if cfg.Throttles.Lines == 0 {
		cfg.Throttles.Lines = defaultConsoleThrottleLines
	}
	if cfg.Throttles.LineResetInterval == 0 {
		cfg.Throttles.LineResetInterval = defaultConsoleThrottleIntervalMs
	}

	// Backward-compat flag: internal -> is_internal.
	if cfg.Docker.Network.Internal {
		cfg.Docker.Network.IsInternal = true
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func normalizeAllowedOrigins(groups ...[]string) []string {
	normalized := make([]string, 0)
	seen := make(map[string]struct{})

	for _, group := range groups {
		for _, raw := range group {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}

			if value == "*" {
				if _, ok := seen["*"]; !ok {
					seen["*"] = struct{}{}
					normalized = append(normalized, "*")
				}
				continue
			}

			origin, err := extractURLOrigin(value)
			if err != nil {
				continue
			}
			if _, ok := seen[origin]; ok {
				continue
			}
			seen[origin] = struct{}{}
			normalized = append(normalized, origin)
		}
	}

	return normalized
}

func normalizeTrustedProxies(values []string) []string {
	normalized := make([]string, 0)
	seen := make(map[string]struct{})
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func extractURLOrigin(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("empty URL")
	}

	withScheme := trimmed
	if !strings.Contains(withScheme, "://") {
		withScheme = "https://" + withScheme
	}

	parsed, err := url.Parse(withScheme)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", errors.New("missing host")
	}

	protocol := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if protocol != "http" && protocol != "https" {
		return "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	return protocol + "://" + strings.ToLower(parsed.Host), nil
}
