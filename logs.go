package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (s *Service) appendLogBuffer(serverID int, output string) {
	if output == "" {
		return
	}

	s.buffersMu.Lock()
	defer s.buffersMu.Unlock()

	buffer, ok := s.logBuffers[serverID]
	if !ok {
		buffer = &LogBuffer{}
		s.logBuffers[serverID] = buffer
	}

	parts := strings.Split(output, "\n")
	for i, line := range parts {
		entry := line
		if i != len(parts)-1 {
			entry += "\n"
		}
		if entry == "" {
			continue
		}
		buffer.Lines = append(buffer.Lines, entry)
		buffer.Bytes += len(entry)
	}

	for len(buffer.Lines) > logBufferMaxLines || buffer.Bytes > logBufferMaxBytes {
		if len(buffer.Lines) == 0 {
			break
		}
		removed := buffer.Lines[0]
		buffer.Lines = buffer.Lines[1:]
		buffer.Bytes -= len(removed)
	}
}

func (s *Service) getBufferedLogs(serverID int) string {
	s.buffersMu.Lock()
	defer s.buffersMu.Unlock()
	buffer := s.logBuffers[serverID]
	if buffer == nil || len(buffer.Lines) == 0 {
		return ""
	}
	return strings.Join(buffer.Lines, "")
}

func (s *Service) clearBufferedLogs(serverID int) {
	s.buffersMu.Lock()
	delete(s.logBuffers, serverID)
	s.buffersMu.Unlock()

	s.cacheMu.Lock()
	delete(s.lastNotRunningNotice, serverID)
	s.cacheMu.Unlock()

	s.consoleThrottleMu.Lock()
	delete(s.consoleThrottle, serverID)
	s.consoleThrottleMu.Unlock()
}

func (s *Service) shouldSendNotRunningNotice(serverID int) bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	last := s.lastNotRunningNotice[serverID]
	if !last.IsZero() && time.Since(last) < notRunningNoticeCooldown {
		return false
	}
	s.lastNotRunningNotice[serverID] = time.Now()
	return true
}

func (s *Service) getDiskUsageMB(serverID int) int {
	s.cacheMu.Lock()
	cached, ok := s.diskUsageCache[serverID]
	if ok && time.Since(cached.TS) < s.diskUsageCacheTTL {
		s.cacheMu.Unlock()
		return cached.UsedMB
	}
	s.cacheMu.Unlock()

	targetPath := fmt.Sprintf("%s/%d", strings.TrimRight(s.volumesPath, "/"), serverID)
	out, err := runCommand("du", "-sm", targetPath)
	usedMB := 0
	if err == nil {
		fields := strings.Fields(out)
		if len(fields) > 0 {
			if parsed, convErr := strconv.Atoi(strings.TrimSpace(fields[0])); convErr == nil && parsed >= 0 {
				usedMB = parsed
			}
		}
	}

	s.cacheMu.Lock()
	s.diskUsageCache[serverID] = DiskUsageCacheEntry{UsedMB: usedMB, TS: time.Now()}
	s.cacheMu.Unlock()

	return usedMB
}

func (s *Service) cleanupLogStream(serverID int) {
	s.clearAttachedStream(serverID)

	s.streamsMu.Lock()
	cancel, ok := s.activeLog[serverID]
	if ok {
		delete(s.activeLog, serverID)
	}
	s.streamsMu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Service) cleanupStatsStream(serverID int) {
	s.streamsMu.Lock()
	cancel, ok := s.activeStat[serverID]
	if ok {
		delete(s.activeStat, serverID)
	}
	s.streamsMu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Service) cleanupServerStreams(serverID int) {
	s.cleanupLogStream(serverID)
	s.cleanupStatsStream(serverID)
}

func (s *Service) cleanupAllStreams() {
	s.attachMu.Lock()
	attached := make([]*AttachedStream, 0, len(s.attachStdin))
	for _, stream := range s.attachStdin {
		attached = append(attached, stream)
	}
	s.attachStdin = make(map[int]*AttachedStream)
	s.attachMu.Unlock()

	for _, stream := range attached {
		if stream != nil && stream.Stdin != nil {
			_ = stream.Stdin.Close()
		}
	}

	s.streamsMu.Lock()
	logCancels := make([]context.CancelFunc, 0, len(s.activeLog))
	for _, cancel := range s.activeLog {
		logCancels = append(logCancels, cancel)
	}
	statCancels := make([]context.CancelFunc, 0, len(s.activeStat))
	for _, cancel := range s.activeStat {
		statCancels = append(statCancels, cancel)
	}
	s.activeLog = make(map[int]context.CancelFunc)
	s.activeStat = make(map[int]context.CancelFunc)
	s.pendingAttach = make(map[int]bool)
	s.streamsMu.Unlock()

	for _, cancel := range logCancels {
		cancel()
	}
	for _, cancel := range statCancels {
		cancel()
	}
}

func (s *Service) startStatsStream(serverID int) {
	s.streamsMu.Lock()
	if _, exists := s.activeStat[serverID]; exists {
		s.streamsMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.activeStat[serverID] = cancel
	s.streamsMu.Unlock()

	go func() {
		ticker := time.NewTicker(serverStatsInterval)
		defer ticker.Stop()
		defer s.cleanupStatsStream(serverID)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				running, cpuPct, memMB := s.inspectContainerStats(serverID)
				if !running {
					return
				}
				diskMB := s.getDiskUsageMB(serverID)
				_ = s.sendJSON(map[string]interface{}{
					"type":     "server_stats",
					"serverId": serverID,
					"cpu":      fmt.Sprintf("%.1f", cpuPct),
					"memory":   strconv.Itoa(memMB),
					"disk":     strconv.Itoa(diskMB),
				})
			}
		}
	}()
}

func (s *Service) inspectContainerStats(serverID int) (bool, float64, int) {
	containerName := fmt.Sprintf("cpanel-%d", serverID)
	out, err := runCommand("docker", "inspect", "-f", "{{.State.Running}}", containerName)
	if err != nil || strings.TrimSpace(out) != "true" {
		return false, 0, 0
	}

	statsOut, err := runCommand("docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}|{{.MemUsage}}", containerName)
	if err != nil {
		return true, 0, 0
	}
	parts := strings.Split(strings.TrimSpace(statsOut), "|")
	cpuPct := 0.0
	memMB := 0
	if len(parts) > 0 {
		cpuRaw := strings.TrimSpace(strings.TrimSuffix(parts[0], "%"))
		if parsed, parseErr := strconv.ParseFloat(cpuRaw, 64); parseErr == nil {
			cpuPct = parsed
		}
	}
	if len(parts) > 1 {
		memParts := strings.Split(parts[1], "/")
		if len(memParts) > 0 {
			if parsed, parseErr := parseHumanSizeToMB(memParts[0]); parseErr == nil {
				memMB = parsed
			}
		}
	}
	return true, cpuPct, memMB
}

func (s *Service) ensureServerLogStream(serverID int, replayBufferedLogs bool, silentIfStopped bool, includeHistoricalTail bool) {
	hadBufferedLogs := false

	s.streamsMu.Lock()
	if _, active := s.activeLog[serverID]; active {
		s.streamsMu.Unlock()
		return
	}
	if s.pendingAttach[serverID] {
		s.streamsMu.Unlock()
		return
	}
	s.pendingAttach[serverID] = true
	s.streamsMu.Unlock()

	defer func() {
		s.streamsMu.Lock()
		delete(s.pendingAttach, serverID)
		s.streamsMu.Unlock()
	}()

	if replayBufferedLogs {
		if buffered := s.getBufferedLogs(serverID); buffered != "" {
			hadBufferedLogs = true
			_ = s.sendJSON(map[string]interface{}{
				"type":     "console_output",
				"serverId": serverID,
				"output":   buffered,
			})
		}
	}

	runningOut, err := runCommand("docker", "inspect", "-f", "{{.State.Running}}", fmt.Sprintf("cpanel-%d", serverID))
	if err != nil || strings.TrimSpace(runningOut) != "true" {
		if !silentIfStopped && s.shouldSendNotRunningNotice(serverID) {
			s.sendConsoleOutput(serverID, "\x1b[1;33m[!] Server is not running. Start the server to see live output.\x1b[0m\n")
		}
		return
	}

	s.streamsMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	s.activeLog[serverID] = cancel
	s.streamsMu.Unlock()

	s.startStatsStream(serverID)

	go func() {
		defer s.cleanupLogStream(serverID)
		containerName := fmt.Sprintf("cpanel-%d", serverID)

		if includeHistoricalTail && !hadBufferedLogs {
			if err := s.streamDockerOutput(ctx, serverID, "logs", "--tail", "500", containerName); err != nil && ctx.Err() == nil {
				bootWarn("failed to stream historical logs server=%d error=%v", serverID, err)
			}
		}

		attachErr := s.streamDockerAttach(ctx, serverID, containerName)
		if attachErr != nil && ctx.Err() == nil {
			s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;31m[!] Failed to start live console stream: %v\x1b[0m\n", attachErr))
		}
		s.cleanupStatsStream(serverID)
	}()
}

func (s *Service) restoreRunningConsoleStreams() {
	out, err := runCommand("docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		bootWarn("failed to list running containers for console restore error=%v", err)
		return
	}

	restored := 0
	for _, line := range strings.Split(out, "\n") {
		containerName := strings.TrimSpace(line)
		if !strings.HasPrefix(containerName, "cpanel-") {
			continue
		}

		serverIDRaw := strings.TrimSpace(strings.TrimPrefix(containerName, "cpanel-"))
		serverID, parseErr := strconv.Atoi(serverIDRaw)
		if parseErr != nil || serverID <= 0 {
			continue
		}

		s.ensureServerLogStream(serverID, false, true, true)
		restored++
	}

	bootInfo("restored console streams for running servers count=%d", restored)
}

func (s *Service) streamDockerAttach(ctx context.Context, serverID int, containerName string) error {
	// Prefer output-only attach to avoid TTY stdin issues on some Docker setups.
	err := s.streamDockerOutput(ctx, serverID, "attach", "--no-stdin", "--sig-proxy=false", containerName)
	if err == nil || ctx.Err() != nil {
		return err
	}

	// Older Docker builds might not support --no-stdin; fallback to full attach.
	if strings.Contains(strings.ToLower(err.Error()), "unknown flag") {
		err = s.streamDockerAttachWithStdin(ctx, serverID, containerName)
		if err == nil || ctx.Err() != nil {
			return err
		}
	}

	// Final safety net: keep live output available even if attach cannot acquire a TTY stdin.
	if isDockerTTYInputError(err) {
		bootWarn("docker attach tty stdin failed server=%d; falling back to docker logs follow", serverID)
		return s.streamDockerOutput(ctx, serverID, "logs", "-f", "--tail", "0", containerName)
	}

	return err
}

func (s *Service) streamDockerAttachWithStdin(ctx context.Context, serverID int, containerName string) error {
	cmd := exec.CommandContext(ctx, "docker", "attach", "--sig-proxy=false", containerName)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	s.setAttachedStream(serverID, &AttachedStream{Stdin: stdin})
	defer s.clearAttachedStream(serverID)

	var wg sync.WaitGroup
	forward := func(reader io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			s.sendConsoleOutput(serverID, scanner.Text()+"\n")
		}
	}

	wg.Add(2)
	go forward(stdout)
	go forward(stderr)
	wg.Wait()

	err = cmd.Wait()
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

func isDockerTTYInputError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "input device is not a tty") || strings.Contains(msg, "the input device is not a tty")
}

func (s *Service) setAttachedStream(serverID int, stream *AttachedStream) {
	s.attachMu.Lock()
	if old := s.attachStdin[serverID]; old != nil && old.Stdin != nil {
		_ = old.Stdin.Close()
	}
	s.attachStdin[serverID] = stream
	s.attachMu.Unlock()
}

func (s *Service) clearAttachedStream(serverID int) {
	s.attachMu.Lock()
	stream := s.attachStdin[serverID]
	delete(s.attachStdin, serverID)
	s.attachMu.Unlock()

	if stream != nil && stream.Stdin != nil {
		_ = stream.Stdin.Close()
	}
}

func (s *Service) streamDockerOutput(ctx context.Context, serverID int, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	forward := func(reader io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			s.sendConsoleOutput(serverID, scanner.Text()+"\n")
		}
	}

	wg.Add(2)
	go forward(stdout)
	go forward(stderr)
	wg.Wait()

	err = cmd.Wait()
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}
