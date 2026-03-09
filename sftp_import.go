package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type sftpImportStats struct {
	Files       int
	Directories int
	Bytes       int64
	lastEmitAt  time.Time
	emit        func(files, directories int, bytes int64)
}

func normalizeSHA256Fingerprint(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "SHA256:")
	return strings.TrimSpace(value)
}

func buildSFTPHostKeyCallback(message map[string]interface{}) (ssh.HostKeyCallback, error) {
	if message == nil {
		return nil, errors.New("missing SFTP options")
	}

	if pinned := normalizeSHA256Fingerprint(asString(message["hostKeyFingerprint"])); pinned != "" {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			actual := normalizeSHA256Fingerprint(ssh.FingerprintSHA256(key))
			if subtle.ConstantTimeCompare([]byte(actual), []byte(pinned)) != 1 {
				return fmt.Errorf("host key fingerprint mismatch for %s", hostname)
			}
			return nil
		}, nil
	}

	candidates := make([]string, 0, 3)
	if homeDir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(homeDir) != "" {
		candidates = append(candidates, filepath.Join(homeDir, ".ssh", "known_hosts"))
	}
	candidates = append(candidates, "/etc/ssh/ssh_known_hosts")

	seen := make(map[string]struct{}, len(candidates))
	existingFiles := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		path := strings.TrimSpace(candidate)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			existingFiles = append(existingFiles, path)
		}
	}
	if len(existingFiles) == 0 {
		return nil, errors.New("missing SFTP host key trust data: provide hostKeyFingerprint or known_hosts file")
	}

	callback, err := knownhosts.New(existingFiles...)
	if err != nil {
		return nil, fmt.Errorf("invalid known_hosts configuration: %w", err)
	}
	return callback, nil
}

func (s *sftpImportStats) maybeEmit(force bool) {
	if s == nil || s.emit == nil {
		return
	}
	now := time.Now()
	if !force {
		// Emit progress every 2 seconds or each 25 files.
		if s.Files%25 != 0 && now.Sub(s.lastEmitAt) < 2*time.Second {
			return
		}
	}
	s.lastEmitAt = now
	s.emit(s.Files, s.Directories, s.Bytes)
}

func (s *Service) handleImportSFTPFiles(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	host := strings.TrimSpace(asString(message["host"]))
	port := asInt(message["port"])
	username := strings.TrimSpace(asString(message["username"]))
	password := asString(message["password"])
	remotePath := strings.TrimSpace(asString(message["remotePath"]))
	cleanTarget := asBool(message["cleanTarget"])

	if serverID <= 0 || host == "" || username == "" || password == "" {
		return
	}
	if port <= 0 || port > 65535 {
		port = 2022
	}
	if remotePath == "" {
		remotePath = "/"
	}
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}

	targetRoot, err := safeServerPath(s.volumesPath, serverID, "/")
	if err != nil {
		s.sendSFTPImportResult(serverID, false, 0, 0, 0, err.Error())
		return
	}

	s.sendConsoleOutput(serverID, fmt.Sprintf("[*] Starting migration file import from %s:%d%s\n", host, port, remotePath))

	if cleanTarget {
		if err := clearDirectoryContents(targetRoot); err != nil {
			s.sendSFTPImportResult(serverID, false, 0, 0, 0, fmt.Sprintf("failed to clean destination: %v", err))
			return
		}
	}

	hostKeyCallback, err := buildSFTPHostKeyCallback(message)
	if err != nil {
		s.sendSFTPImportResult(serverID, false, 0, 0, 0, err.Error())
		return
	}

	sshConfig := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         20 * time.Second,
	}

	sshConn, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), sshConfig)
	if err != nil {
		s.sendSFTPImportResult(serverID, false, 0, 0, 0, fmt.Sprintf("failed to connect source SFTP: %v", err))
		return
	}
	defer sshConn.Close()

	sftpClient, err := sftp.NewClient(sshConn)
	if err != nil {
		s.sendSFTPImportResult(serverID, false, 0, 0, 0, fmt.Sprintf("failed to initialize source SFTP client: %v", err))
		return
	}
	defer sftpClient.Close()

	stats := &sftpImportStats{
		emit: func(files, directories int, bytes int64) {
			s.sendSFTPImportProgress(serverID, files, directories, bytes)
		},
	}
	stats.maybeEmit(true)
	if err := importSFTPPathRecursive(sftpClient, remotePath, targetRoot, stats); err != nil {
		s.sendSFTPImportResult(serverID, false, stats.Files, stats.Directories, stats.Bytes, err.Error())
		return
	}
	stats.maybeEmit(true)

	if err := s.fixServerPermissions(targetRoot); err != nil {
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;33m[!] Could not fix server permissions: %v\x1b[0m\n", err))
	}
	s.sendSFTPImportResult(serverID, true, stats.Files, stats.Directories, stats.Bytes, "")
}

func importSFTPPathRecursive(client *sftp.Client, remotePath, localPath string, stats *sftpImportStats) error {
	info, err := client.Stat(remotePath)
	if err != nil {
		return fmt.Errorf("cannot stat remote path %s: %w", remotePath, err)
	}

	if info.IsDir() {
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return err
		}
		stats.Directories++
		stats.maybeEmit(false)

		entries, err := client.ReadDir(remotePath)
		if err != nil {
			return fmt.Errorf("cannot read remote directory %s: %w", remotePath, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "." || name == ".." {
				continue
			}
			if strings.Contains(name, "/") || strings.Contains(name, "\\") {
				continue
			}
			if entry.Mode()&os.ModeSymlink != 0 {
				continue
			}

			nextRemote := path.Join(remotePath, name)
			nextLocal := filepath.Join(localPath, name)
			if err := importSFTPPathRecursive(client, nextRemote, nextLocal, stats); err != nil {
				return err
			}
		}
		return nil
	}

	return copySFTPFile(client, remotePath, localPath, info, stats)
}

func copySFTPFile(client *sftp.Client, remotePath, localPath string, info os.FileInfo, stats *sftpImportStats) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}

	remoteFile, err := client.Open(remotePath)
	if err != nil {
		return fmt.Errorf("cannot open remote file %s: %w", remotePath, err)
	}
	defer remoteFile.Close()

	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
	}

	localFile, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("cannot open local file %s: %w", localPath, err)
	}

	written, copyErr := io.Copy(localFile, remoteFile)
	closeErr := localFile.Close()
	if copyErr != nil {
		return fmt.Errorf("cannot copy file %s: %w", remotePath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("cannot finalize local file %s: %w", localPath, closeErr)
	}

	stats.Files++
	stats.Bytes += written
	stats.maybeEmit(false)
	return nil
}

func clearDirectoryContents(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(root, 0o755)
		}
		return err
	}
	for _, entry := range entries {
		target, joinErr := safeJoin(root, entry.Name())
		if joinErr != nil {
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendSFTPImportResult(serverID int, success bool, files, directories int, bytes int64, errorMessage string) {
	payload := map[string]interface{}{
		"type":        "sftp_import_result",
		"serverId":    serverID,
		"success":     success,
		"files":       files,
		"directories": directories,
		"bytes":       bytes,
	}
	if !success {
		payload["error"] = errorMessage
		s.sendConsoleOutput(serverID, fmt.Sprintf("[!] Migration file import failed: %s\n", errorMessage))
	} else {
		s.sendConsoleOutput(serverID, fmt.Sprintf("[✓] Migration file import completed: %d files, %d directories, %d bytes.\n", files, directories, bytes))
	}
	_ = s.sendJSON(payload)
}

func (s *Service) sendSFTPImportProgress(serverID int, files, directories int, bytes int64) {
	_ = s.sendJSON(map[string]interface{}{
		"type":        "sftp_import_progress",
		"serverId":    serverID,
		"files":       files,
		"directories": directories,
		"bytes":       bytes,
	})
}
