package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"
)

func normalizeDownloadHost(input string) string {
	host := strings.TrimSpace(strings.ToLower(input))
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	return host
}

func resolveAllowlistedDownloadHost(hostname, port string, allowlisted []string) (string, error) {
	host := normalizeDownloadHost(hostname)
	if host == "" {
		return "", errors.New("download URL host is blocked")
	}

	// Match explicit host:port entries first.
	if port != "" {
		withPort := net.JoinHostPort(host, strings.TrimSpace(port))
		for _, entry := range allowlisted {
			if strings.EqualFold(entry, withPort) {
				return withPort, nil
			}
		}
	}

	// Then match exact host entries.
	for _, entry := range allowlisted {
		if strings.Contains(entry, ":") {
			continue
		}
		if strings.EqualFold(entry, host) {
			return entry, nil
		}
	}

	return "", errors.New("download URL host is not allowlisted")
}

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

func isBlockedDownloadIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return !ip.IsGlobalUnicast()
}

func validateRemoteDownloadURL(ctx context.Context, parsedURL *url.URL, allowlistedHosts []string) error {
	if parsedURL == nil {
		return errors.New("invalid download URL")
	}
	if parsedURL.User != nil {
		return errors.New("download URL must not include credentials")
	}

	scheme := strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if scheme != "http" && scheme != "https" {
		return errors.New("only http/https URLs are allowed")
	}

	host := normalizeDownloadHost(parsedURL.Hostname())
	if host == "" || strings.EqualFold(host, "localhost") {
		return errors.New("download URL host is blocked")
	}
	if _, err := resolveAllowlistedDownloadHost(host, strings.TrimSpace(parsedURL.Port()), allowlistedHosts); err != nil {
		return err
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedDownloadIP(ip) {
			return errors.New("download URL host resolves to blocked IP range")
		}
		return nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	records, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(records) == 0 {
		return errors.New("download URL host cannot be resolved")
	}
	for _, record := range records {
		if isBlockedDownloadIP(record.IP) {
			return errors.New("download URL host resolves to blocked IP range")
		}
	}
	return nil
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
	if err := validateRemoteDownloadURL(context.Background(), parsedURL, s.cfg.Transfers.AllowedDownloadHosts); err != nil {
		sendErr(err.Error())
		return
	}
	safeHost, err := resolveAllowlistedDownloadHost(parsedURL.Hostname(), parsedURL.Port(), s.cfg.Transfers.AllowedDownloadHosts)
	if err != nil {
		sendErr(err.Error())
		return
	}
	safeURL := &url.URL{
		Scheme:   strings.ToLower(strings.TrimSpace(parsedURL.Scheme)),
		Host:     safeHost,
		Path:     parsedURL.Path,
		RawPath:  parsedURL.RawPath,
		RawQuery: parsedURL.RawQuery,
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
	serverRoot, err := safeServerPath(s.volumesPath, serverID, "/")
	if err != nil {
		sendErr(err.Error())
		return
	}
	if err := secureMkdirAll(serverRoot, targetDir, 0o755); err != nil {
		sendErr(err.Error())
		return
	}

	targetPath, err := safeJoin(targetDir, fileName)
	if err != nil {
		sendErr(err.Error())
		return
	}

	if info, statErr := secureStat(serverRoot, targetPath); statErr == nil && info.IsDir() {
		sendErr("target path is a directory")
		return
	}

	tmpPath := targetPath + ".cpanel-downloading"
	_ = secureRemove(serverRoot, tmpPath)

	req, err := http.NewRequest(http.MethodGet, safeURL.String(), nil)
	if err != nil {
		sendErr("failed to build download request")
		return
	}
	req.Header.Set("User-Agent", "CPanel-Connector-Go/1.0")

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				return nil, splitErr
			}
			if err := validateRemoteDownloadURL(ctx, &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, s.cfg.Transfers.AllowedDownloadHosts); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{
		Timeout:   remoteDownloadTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return validateRemoteDownloadURL(req.Context(), req.URL, s.cfg.Transfers.AllowedDownloadHosts)
		},
	}
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

	out, err := secureOpenFile(serverRoot, tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		sendErr(err.Error())
		return
	}

	bodyReader := io.LimitReader(resp.Body, maxRemoteDownloadBytes+1)
	if s.downloadLimitBytesPerSec > 0 {
		bodyReader = newRateLimitedReader(bodyReader, s.downloadLimitBytesPerSec)
	}
	written, copyErr := io.Copy(out, bodyReader)
	closeErr := out.Close()

	if copyErr != nil {
		_ = secureRemove(serverRoot, tmpPath)
		sendErr("failed while downloading file: %v", copyErr)
		return
	}
	if closeErr != nil {
		_ = secureRemove(serverRoot, tmpPath)
		sendErr("failed to finalize downloaded file: %v", closeErr)
		return
	}
	if written > maxRemoteDownloadBytes {
		_ = secureRemove(serverRoot, tmpPath)
		sendErr("file too large (max %d MB)", maxRemoteDownloadBytes/(1024*1024))
		return
	}

	if err := secureRename(serverRoot, tmpPath, targetPath); err != nil {
		_ = secureRemove(serverRoot, tmpPath)
		sendErr("failed to move downloaded file into place: %v", err)
		return
	}

	_, _ = runCommand("chown", "-R", s.chownUser(), targetPath)

	relPath := filepath.ToSlash(filepath.Join(directory, fileName))
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}

	s.sendDownloadFileResult(serverID, requestID, true, "", fileName, relPath, written)
}

type rateLimitedReader struct {
	reader           io.Reader
	limitBytesPerSec int64
	start            time.Time
	bytes            int64
}

func newRateLimitedReader(reader io.Reader, limitBytesPerSec int64) io.Reader {
	if limitBytesPerSec <= 0 {
		return reader
	}
	return &rateLimitedReader{
		reader:           reader,
		limitBytesPerSec: limitBytesPerSec,
		start:            time.Now(),
	}
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n <= 0 || r.limitBytesPerSec <= 0 {
		return n, err
	}

	r.bytes += int64(n)
	elapsed := time.Since(r.start)
	if elapsed < 0 {
		elapsed = 0
	}
	expected := time.Duration(float64(r.bytes) / float64(r.limitBytesPerSec) * float64(time.Second))
	if expected > elapsed {
		time.Sleep(expected - elapsed)
	}
	return n, err
}
