package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

func sanitizeDownloadBasename(name string) string {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return ""
	}

	base := filepath.Base(raw)
	if base == "." || base == ".." {
		return ""
	}

	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '+' {
			return r
		}
		return '_'
	}, base)
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		return ""
	}
	if len(clean) > 128 {
		clean = clean[:128]
	}
	return clean
}

func (s *Service) sendDownloadFileResult(serverID int, requestID string, success bool, errMsg string, fileName string, filePath string, size int64) {
	payload := map[string]interface{}{
		"type":      "download_file_result",
		"serverId":  serverID,
		"requestId": requestID,
		"success":   success,
		"error":     strings.TrimSpace(errMsg),
		"fileName":  strings.TrimSpace(fileName),
		"path":      strings.TrimSpace(filePath),
		"size":      size,
	}
	_ = s.sendJSON(payload)
}

func (s *Service) handleDownloadFile(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	requestID := strings.TrimSpace(asString(message["requestId"]))
	directory := strings.TrimSpace(asString(message["directory"]))
	rawURL := strings.TrimSpace(asString(message["url"]))
	fileName := sanitizeDownloadBasename(asString(message["fileName"]))

	if serverID <= 0 {
		return
	}

	sendErr := func(format string, args ...interface{}) {
		s.sendDownloadFileResult(serverID, requestID, false, fmt.Sprintf(format, args...), "", "", 0)
	}

	if directory == "" {
		directory = "/"
	}
	if rawURL == "" {
		sendErr("missing download URL")
		return
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		sendErr("invalid download URL")
		return
	}
	scheme := strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if scheme != "http" && scheme != "https" {
		sendErr("only http/https URLs are allowed")
		return
	}
	if strings.TrimSpace(parsedURL.Host) == "" {
		sendErr("invalid download URL host")
		return
	}

	if fileName == "" {
		fileName = sanitizeDownloadBasename(pathpkg.Base(parsedURL.Path))
	}
	if fileName == "" {
		fileName = "download.bin"
	}

	targetDir, err := safeServerPath(s.volumesPath, serverID, directory)
	if err != nil {
		sendErr(err.Error())
		return
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		sendErr(err.Error())
		return
	}

	targetPath, err := safeJoin(targetDir, fileName)
	if err != nil {
		sendErr(err.Error())
		return
	}

	if info, statErr := os.Stat(targetPath); statErr == nil && info.IsDir() {
		sendErr("target path is a directory")
		return
	}

	tmpPath := targetPath + ".cpanel-downloading"
	_ = os.Remove(tmpPath)

	req, err := http.NewRequest(http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		sendErr("failed to build download request")
		return
	}
	req.Header.Set("User-Agent", "CPanel-Connector-Go/1.0")

	client := &http.Client{Timeout: remoteDownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		sendErr("download request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		sendErr("download failed with status %d", resp.StatusCode)
		return
	}
	if resp.ContentLength > maxRemoteDownloadBytes {
		sendErr("file too large (max %d MB)", maxRemoteDownloadBytes/(1024*1024))
		return
	}

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		sendErr(err.Error())
		return
	}

	limitedReader := io.LimitReader(resp.Body, maxRemoteDownloadBytes+1)
	written, copyErr := io.Copy(out, limitedReader)
	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(tmpPath)
		sendErr("failed while downloading file: %v", copyErr)
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		sendErr("failed to finalize downloaded file: %v", closeErr)
		return
	}
	if written > maxRemoteDownloadBytes {
		_ = os.Remove(tmpPath)
		sendErr("file too large (max %d MB)", maxRemoteDownloadBytes/(1024*1024))
		return
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		sendErr("failed to move downloaded file into place: %v", err)
		return
	}

	_, _ = runCommand("chown", "-R", "1000:1000", targetPath)

	relPath := filepath.ToSlash(filepath.Join(directory, fileName))
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}

	s.sendDownloadFileResult(serverID, requestID, true, "", fileName, relPath, written)
}
