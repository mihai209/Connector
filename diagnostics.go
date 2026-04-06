package main

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const diagnosticsCacheTTL = 60 * time.Second

func (s *Service) getDiagnosticsSnapshot() ConnectorDiagnosticsSnapshot {
	now := time.Now().UTC()

	s.diagnosticsMu.RLock()
	cachedAt := s.diagnosticsCacheAt
	cached := s.diagnosticsCache
	s.diagnosticsMu.RUnlock()

	if !cachedAt.IsZero() && now.Sub(cachedAt) < diagnosticsCacheTTL && len(cached.Checks) > 0 {
		return cached
	}

	snapshot := s.buildDiagnosticsSnapshot(now)

	s.diagnosticsMu.Lock()
	s.diagnosticsCache = snapshot
	s.diagnosticsCacheAt = now
	s.diagnosticsMu.Unlock()

	return snapshot
}

func (s *Service) refreshDiagnosticsSnapshot() ConnectorDiagnosticsSnapshot {
	s.diagnosticsMu.Lock()
	s.diagnosticsCacheAt = time.Time{}
	s.diagnosticsCache = ConnectorDiagnosticsSnapshot{}
	s.diagnosticsMu.Unlock()
	return s.getDiagnosticsSnapshot()
}

func (s *Service) recordSFTPAuthResult(success bool, err error) {
	s.diagnosticsMu.Lock()
	defer s.diagnosticsMu.Unlock()

	s.lastSFTPAuthAt = time.Now().UTC()
	s.lastSFTPAuthSuccess = success
	if err != nil {
		s.lastSFTPAuthError = strings.TrimSpace(err.Error())
	} else {
		s.lastSFTPAuthError = ""
	}
}

func (s *Service) buildDiagnosticsSnapshot(now time.Time) ConnectorDiagnosticsSnapshot {
	checks := map[string]ConnectorDiagnosticCheck{
		"docker_access":          s.runDockerAccessCheck(now),
		"dns":                    s.runDNSCheck(now),
		"udp_bind":               s.runUDPBindCheck(now),
		"sftp_auth":              s.runSFTPAuthCheck(now),
		"websocket_payload_size": s.runWSPayloadCheck(now),
		"disk_perms":             s.runDiskPermsCheck(now),
		"image_pull":             s.runImagePullCheck(now),
		"archive_tools":          s.runArchiveToolsCheck(now),
		"java_runtime":           s.runRuntimeCheck(now, "java", []string{"-version"}),
		"node_runtime":           s.runRuntimeCheck(now, "node", []string{"--version"}),
	}

	return ConnectorDiagnosticsSnapshot{
		GeneratedAt: now.Format(time.RFC3339),
		Checks:      checks,
	}
}

func (s *Service) runDockerAccessCheck(now time.Time) ConnectorDiagnosticCheck {
	version, err := runCommand("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return diagnosticFail(now, "Docker unavailable", err.Error(), nil)
	}
	version = strings.TrimSpace(version)
	if version == "" {
		version = "unknown"
	}
	return diagnosticOK(now, "Docker access OK", "Docker server version "+version, map[string]interface{}{
		"version": version,
	})
}

func (s *Service) runDNSCheck(now time.Time) ConnectorDiagnosticCheck {
	panelURL := strings.TrimSpace(s.cfg.Panel.URL)
	if panelURL == "" {
		return diagnosticFail(now, "Panel URL missing", "panel.url is empty in connector config.", nil)
	}

	parsed, err := url.Parse(panelURL)
	if err != nil || strings.TrimSpace(parsed.Hostname()) == "" {
		return diagnosticFail(now, "Panel hostname invalid", "Could not parse panel.url for DNS lookup.", map[string]interface{}{
			"panelUrl": panelURL,
		})
	}

	host := parsed.Hostname()
	ips, lookupErr := net.LookupHost(host)
	if lookupErr != nil {
		return diagnosticFail(now, "DNS lookup failed", lookupErr.Error(), map[string]interface{}{
			"panelHost": host,
			"dns":       s.effectiveContainerDNSServers(),
		})
	}

	return diagnosticOK(now, "DNS resolution OK", "Resolved "+host+" successfully.", map[string]interface{}{
		"panelHost": host,
		"addresses": ips,
		"dns":       s.effectiveContainerDNSServers(),
	})
}

func (s *Service) runUDPBindCheck(now time.Time) ConnectorDiagnosticCheck {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return diagnosticFail(now, "UDP bind failed", err.Error(), nil)
	}
	defer conn.Close()

	return diagnosticOK(now, "UDP bind OK", "Connector can bind local UDP sockets.", map[string]interface{}{
		"address": conn.LocalAddr().String(),
	})
}

func (s *Service) runSFTPAuthCheck(now time.Time) ConnectorDiagnosticCheck {
	hostKeyPath := strings.TrimSpace(s.cfg.SFTP.HostKeyPath)
	if hostKeyPath == "" {
		return diagnosticFail(now, "SFTP host key missing", "sftp.hostKeyPath is not configured.", nil)
	}

	if _, err := os.Stat(hostKeyPath); err != nil {
		return diagnosticWarn(now, "SFTP host key not found yet", err.Error(), map[string]interface{}{
			"hostKeyPath": hostKeyPath,
		})
	}

	s.diagnosticsMu.RLock()
	lastAt := s.lastSFTPAuthAt
	lastSuccess := s.lastSFTPAuthSuccess
	lastError := s.lastSFTPAuthError
	s.diagnosticsMu.RUnlock()

	metadata := map[string]interface{}{
		"bind":        s.cfg.SFTP.Host + ":" + strings.TrimSpace(asString(s.cfg.SFTP.Port)),
		"hostKeyPath": hostKeyPath,
	}
	if !lastAt.IsZero() {
		metadata["lastAuthAt"] = lastAt.Format(time.RFC3339)
	}

	switch {
	case !lastAt.IsZero() && lastSuccess:
		return diagnosticOK(now, "SFTP auth OK", "Latest SFTP auth completed successfully.", metadata)
	case !lastAt.IsZero() && strings.TrimSpace(lastError) != "":
		return diagnosticWarn(now, "Recent SFTP auth failed", lastError, metadata)
	default:
		return diagnosticOK(now, "SFTP ready", "SFTP subsystem configured; no auth attempts recorded yet.", metadata)
	}
}

func (s *Service) runWSPayloadCheck(now time.Time) ConnectorDiagnosticCheck {
	limitMb := s.getWSReadLimitMB()
	return diagnosticOK(now, "WebSocket payload limit configured", "Current connector read limit is set.", map[string]interface{}{
		"readLimitMb": limitMb,
		"minimumMb":   wsReadLimitMinMB,
		"maximumMb":   wsReadLimitMaxMB,
	})
}

func (s *Service) runDiskPermsCheck(now time.Time) ConnectorDiagnosticCheck {
	if strings.TrimSpace(s.volumesPath) == "" {
		return diagnosticFail(now, "Volumes path missing", "No volumes path configured for connector.", nil)
	}
	if err := os.MkdirAll(s.volumesPath, 0o755); err != nil {
		return diagnosticFail(now, "Volumes path unavailable", err.Error(), map[string]interface{}{
			"path": s.volumesPath,
		})
	}

	testDir, err := os.MkdirTemp(s.volumesPath, ".cpanel-diag-*")
	if err != nil {
		return diagnosticFail(now, "Write test failed", err.Error(), map[string]interface{}{
			"path": s.volumesPath,
		})
	}
	defer os.RemoveAll(testDir)

	testFile := filepath.Join(testDir, "write-test.txt")
	if err := os.WriteFile(testFile, []byte("ok"), 0o644); err != nil {
		return diagnosticFail(now, "File write failed", err.Error(), map[string]interface{}{
			"path": testFile,
		})
	}

	return diagnosticOK(now, "Disk permissions OK", "Connector can create and write inside the volumes directory.", map[string]interface{}{
		"path": s.volumesPath,
	})
}

func (s *Service) runImagePullCheck(now time.Time) ConnectorDiagnosticCheck {
	_, err := runCommand("docker", "pull", "--help")
	if err != nil {
		return diagnosticFail(now, "docker pull unavailable", err.Error(), nil)
	}
	return diagnosticOK(now, "Image pull command available", "docker pull is available on this connector. Registry/network auth is not tested by heartbeat.", nil)
}

func (s *Service) runArchiveToolsCheck(now time.Time) ConnectorDiagnosticCheck {
	required := []string{"zip", "unzip", "tar", "gunzip", "bzip2", "xz"}
	missing := make([]string, 0)
	paths := make(map[string]interface{}, len(required))

	for _, binary := range required {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			missing = append(missing, binary)
			continue
		}
		paths[binary] = resolved
	}

	if len(missing) > 0 {
		return diagnosticWarn(now, "Archive tools incomplete", "Some archive helpers are missing: "+strings.Join(missing, ", "), map[string]interface{}{
			"missing": missing,
			"paths":   paths,
		})
	}

	return diagnosticOK(now, "Archive tools OK", "zip/unzip/tar/gunzip/bzip2/xz are available.", map[string]interface{}{
		"paths": paths,
	})
}

func (s *Service) runRuntimeCheck(now time.Time, binary string, args []string) ConnectorDiagnosticCheck {
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return diagnosticWarn(now, strings.ToUpper(binary)+" runtime missing", err.Error(), nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	output, cmdErr := cmd.CombinedOutput()
	detail := strings.TrimSpace(string(output))
	if ctx.Err() == context.DeadlineExceeded {
		cmdErr = ctx.Err()
	}
	if detail == "" && cmdErr != nil {
		detail = cmdErr.Error()
	}
	if cmdErr != nil && detail == "" {
		detail = "Runtime command returned an error."
	}

	status := "ok"
	summary := strings.ToUpper(binary) + " runtime OK"
	if cmdErr != nil {
		status = "warn"
		summary = strings.ToUpper(binary) + " runtime found with warnings"
	}

	return ConnectorDiagnosticCheck{
		Status:    status,
		Summary:   summary,
		Detail:    detail,
		CheckedAt: now.Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"path": resolved,
		},
	}
}

func diagnosticOK(now time.Time, summary, detail string, metadata map[string]interface{}) ConnectorDiagnosticCheck {
	return ConnectorDiagnosticCheck{
		Status:    "ok",
		Summary:   summary,
		Detail:    detail,
		CheckedAt: now.Format(time.RFC3339),
		Metadata:  metadata,
	}
}

func diagnosticWarn(now time.Time, summary, detail string, metadata map[string]interface{}) ConnectorDiagnosticCheck {
	return ConnectorDiagnosticCheck{
		Status:    "warn",
		Summary:   summary,
		Detail:    detail,
		CheckedAt: now.Format(time.RFC3339),
		Metadata:  metadata,
	}
}

func diagnosticFail(now time.Time, summary, detail string, metadata map[string]interface{}) ConnectorDiagnosticCheck {
	return ConnectorDiagnosticCheck{
		Status:    "fail",
		Summary:   summary,
		Detail:    detail,
		CheckedAt: now.Format(time.RFC3339),
		Metadata:  metadata,
	}
}
