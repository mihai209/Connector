package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	fileHistoryDirName      = ".cpanel-history"
	maxFileHistorySnapshots = 20
	maxReadableHistoryBytes = 2 * maxEditableFileBytes
)

type cappedBuffer struct {
	max int
	buf bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max <= 0 {
		return len(p), nil
	}

	remaining := c.max - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		return len(p), nil
	}
	_, _ = c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return strings.TrimSpace(c.buf.String())
}

func isArchiveFileName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	switch {
	case strings.HasSuffix(lower, ".zip"),
		strings.HasSuffix(lower, ".tar"),
		strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar.bz2"),
		strings.HasSuffix(lower, ".tbz2"),
		strings.HasSuffix(lower, ".tar.xz"),
		strings.HasSuffix(lower, ".txz"),
		strings.HasSuffix(lower, ".gz"),
		strings.HasSuffix(lower, ".bz2"),
		strings.HasSuffix(lower, ".xz"):
		return true
	default:
		return false
	}
}

func buildArchiveExtractCommand(archivePath, targetDir string) (string, []string, error) {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "unzip", []string{"-o", archivePath, "-d", targetDir}, nil
	case strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"),
		strings.HasSuffix(lower, ".tar.bz2"),
		strings.HasSuffix(lower, ".tbz2"),
		strings.HasSuffix(lower, ".tar.xz"),
		strings.HasSuffix(lower, ".txz"):
		return "tar", []string{"-xf", archivePath, "-C", targetDir}, nil
	case strings.HasSuffix(lower, ".gz"):
		return "gunzip", []string{"-kf", archivePath}, nil
	case strings.HasSuffix(lower, ".bz2"):
		return "bzip2", []string{"-dk", archivePath}, nil
	case strings.HasSuffix(lower, ".xz"):
		return "xz", []string{"-dk", archivePath}, nil
	default:
		return "", nil, fmt.Errorf("unsupported archive format")
	}
}

func runExtractionCommand(command string, args []string) error {
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("%s is not installed on connector host", command)
	}

	cmd := exec.Command(command, args...)
	cmd.Stdout = io.Discard
	stderr := &cappedBuffer{max: 8192}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		errText := stderr.String()
		if errText == "" {
			errText = err.Error()
		}
		return fmt.Errorf("%s failed: %s", command, errText)
	}
	return nil
}

func (s *Service) handleExtractArchive(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	directory := asString(message["directory"])
	name := strings.TrimSpace(asString(message["name"]))
	targetDirectory := strings.TrimSpace(asString(message["targetDirectory"]))
	if serverID <= 0 || name == "" {
		return
	}

	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	if !isArchiveFileName(name) {
		sendErr("selected file is not a supported archive")
		return
	}
	if targetDirectory == "" {
		targetDirectory = directory
	}
	if strings.TrimSpace(targetDirectory) == "" {
		targetDirectory = "/"
	}

	currentDir, err := safeServerPath(s.volumesPath, serverID, directory)
	if err != nil {
		sendErr(err.Error())
		return
	}
	archivePath, err := safeJoin(currentDir, name)
	if err != nil {
		sendErr(err.Error())
		return
	}
	stats, err := os.Stat(archivePath)
	if err != nil {
		sendErr("archive file not found")
		return
	}
	if stats.IsDir() {
		sendErr("selected archive is a directory")
		return
	}

	targetDirAbs, err := safeServerPath(s.volumesPath, serverID, targetDirectory)
	if err != nil {
		sendErr(err.Error())
		return
	}
	if err := os.MkdirAll(targetDirAbs, 0o755); err != nil {
		sendErr(err.Error())
		return
	}

	command, args, err := buildArchiveExtractCommand(archivePath, targetDirAbs)
	if err != nil {
		sendErr(err.Error())
		return
	}

	operationID := fmt.Sprintf("extract_%d", time.Now().UnixNano())
	_ = s.sendJSON(map[string]interface{}{
		"type":            "extract_started",
		"serverId":        serverID,
		"operationId":     operationID,
		"archivePath":     filepath.ToSlash(filepath.Join(directory, name)),
		"directory":       directory,
		"targetDirectory": targetDirectory,
	})

	go func() {
		err := runExtractionCommand(command, args)
		if err == nil {
			_, _ = runCommand("chown", "-R", "1000:1000", targetDirAbs)
		}

		result := map[string]interface{}{
			"type":            "extract_complete",
			"serverId":        serverID,
			"operationId":     operationID,
			"archivePath":     filepath.ToSlash(filepath.Join(directory, name)),
			"directory":       directory,
			"targetDirectory": targetDirectory,
			"success":         err == nil,
		}
		if err != nil {
			result["error"] = err.Error()
		}
		_ = s.sendJSON(result)

		fileList, listErr := listDirectoryEntries(targetDirAbs)
		if listErr == nil {
			_ = s.sendJSON(map[string]interface{}{
				"type":      "file_list",
				"serverId":  serverID,
				"directory": targetDirectory,
				"files":     fileList,
			})
		}
	}()
}

func (s *Service) handleReadFile(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	filePath := asString(message["filePath"])
	if serverID <= 0 || strings.TrimSpace(filePath) == "" {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	absPath, err := safeServerPath(s.volumesPath, serverID, filePath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	stat, err := os.Stat(absPath)
	if err != nil {
		sendErr(err.Error())
		return
	}
	if stat.IsDir() {
		sendErr("cannot read a directory")
		return
	}
	if stat.Size() > maxEditableFileBytes {
		sendErr("file is too large to edit (max 5 MB)")
		return
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":     "file_content",
		"serverId": serverID,
		"filePath": filePath,
		"content":  string(raw),
	})
}

func (s *Service) handleWriteFile(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	filePath := asString(message["filePath"])
	content := asString(message["content"])
	contentBase64 := asString(message["contentBase64"])
	encoding := strings.ToLower(strings.TrimSpace(asString(message["encoding"])))
	if serverID <= 0 || strings.TrimSpace(filePath) == "" {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	absPath, err := safeServerPath(s.volumesPath, serverID, filePath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		sendErr(err.Error())
		return
	}

	var payload []byte
	if encoding == "base64" || (contentBase64 != "" && content == "") {
		decoded, decodeErr := base64.StdEncoding.DecodeString(contentBase64)
		if decodeErr != nil {
			sendErr("invalid base64 file payload")
			return
		}
		payload = decoded
	} else {
		payload = []byte(content)
	}

	s.createFileHistorySnapshot(serverID, filePath, absPath)
	if err := os.WriteFile(absPath, payload, 0o644); err != nil {
		sendErr(err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":     "write_success",
		"serverId": serverID,
		"filePath": filePath,
	})
}

func (s *Service) handleListFileVersions(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	filePath := asString(message["filePath"])
	if serverID <= 0 || strings.TrimSpace(filePath) == "" {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	historyDir, baseName, err := s.resolveFileHistoryLocation(serverID, filePath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	entries, err := os.ReadDir(historyDir)
	if err != nil {
		if os.IsNotExist(err) {
			_ = s.sendJSON(map[string]interface{}{
				"type":     "file_versions",
				"serverId": serverID,
				"filePath": filePath,
				"versions": []map[string]interface{}{},
			})
			return
		}
		sendErr(err.Error())
		return
	}

	prefix := baseName + "."
	type versionInfo struct {
		Name  string
		Size  int64
		MTime time.Time
	}
	versions := make([]versionInfo, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".bak") {
			continue
		}
		stats, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		versions = append(versions, versionInfo{
			Name:  name,
			Size:  stats.Size(),
			MTime: stats.ModTime(),
		})
	}

	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].MTime.After(versions[j].MTime)
	})

	items := make([]map[string]interface{}, 0, len(versions))
	for _, version := range versions {
		items = append(items, map[string]interface{}{
			"id":    version.Name,
			"name":  version.Name,
			"size":  version.Size,
			"mtime": version.MTime.UTC().Format(time.RFC3339),
		})
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":     "file_versions",
		"serverId": serverID,
		"filePath": filePath,
		"versions": items,
	})
}

func (s *Service) handleReadFileVersion(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	filePath := asString(message["filePath"])
	versionID := strings.TrimSpace(asString(message["versionId"]))
	if serverID <= 0 || strings.TrimSpace(filePath) == "" || versionID == "" {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	historyDir, baseName, err := s.resolveFileHistoryLocation(serverID, filePath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	if strings.Contains(versionID, "/") || strings.Contains(versionID, "\\") {
		sendErr("invalid version identifier")
		return
	}
	if !strings.HasPrefix(versionID, baseName+".") || !strings.HasSuffix(versionID, ".bak") {
		sendErr("invalid version identifier")
		return
	}

	versionPath, err := safeJoin(historyDir, versionID)
	if err != nil {
		sendErr(err.Error())
		return
	}

	stats, err := os.Stat(versionPath)
	if err != nil {
		sendErr("version not found")
		return
	}
	if stats.IsDir() {
		sendErr("invalid version entry")
		return
	}
	if stats.Size() > maxReadableHistoryBytes {
		sendErr("version snapshot is too large to read")
		return
	}

	raw, err := os.ReadFile(versionPath)
	if err != nil {
		sendErr(err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":      "file_version_content",
		"serverId":  serverID,
		"filePath":  filePath,
		"versionId": versionID,
		"content":   string(raw),
	})
}

func (s *Service) handleFilesAction(action string, message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	directory := asString(message["directory"])
	name := strings.TrimSpace(asString(message["name"]))
	newName := strings.TrimSpace(asString(message["newName"]))
	permissions := strings.TrimSpace(asString(message["permissions"]))
	files := asStringSlice(message["files"])

	if serverID <= 0 {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	currentDir, err := safeServerPath(s.volumesPath, serverID, directory)
	if err != nil {
		sendErr(err.Error())
		return
	}

	switch action {
	case "rename_file":
		srcPath, err := safeJoin(currentDir, name)
		if err != nil {
			sendErr(err.Error())
			return
		}
		dstPath, err := safeJoin(currentDir, newName)
		if err != nil {
			sendErr(err.Error())
			return
		}
		if _, err := os.Stat(srcPath); err != nil {
			sendErr("source file or folder not found")
			return
		}
		if _, err := os.Stat(dstPath); err == nil {
			sendErr("a file or folder with that name already exists")
			return
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			sendErr(err.Error())
			return
		}
	case "delete_files":
		for _, fileName := range files {
			targetPath, err := safeJoin(currentDir, fileName)
			if err != nil {
				continue
			}
			_ = os.RemoveAll(targetPath)
		}
	case "set_permissions":
		if !regexpPerm.MatchString(permissions) {
			sendErr("invalid permissions format")
			return
		}
		targetPath, err := safeJoin(currentDir, name)
		if err != nil {
			sendErr(err.Error())
			return
		}
		mode, _ := strconv.ParseUint(permissions, 8, 32)
		if err := os.Chmod(targetPath, os.FileMode(mode)); err != nil {
			sendErr(err.Error())
			return
		}
	case "create_folder":
		targetPath, err := safeJoin(currentDir, name)
		if err != nil {
			sendErr(err.Error())
			return
		}
		if _, err := os.Stat(targetPath); err == nil {
			sendErr("folder or file already exists")
			return
		}
		if err := os.MkdirAll(targetPath, 0o755); err != nil {
			sendErr(err.Error())
			return
		}
		_, _ = runCommand("chown", "-R", "1000:1000", targetPath)
	case "create_file":
		targetPath, err := safeJoin(currentDir, name)
		if err != nil {
			sendErr(err.Error())
			return
		}
		if _, err := os.Stat(targetPath); err == nil {
			sendErr("folder or file already exists")
			return
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			sendErr(err.Error())
			return
		}
		f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			sendErr(err.Error())
			return
		}
		_ = f.Close()
		_, _ = runCommand("chown", "-R", "1000:1000", targetPath)
	case "list_files":
		if _, err := os.Stat(currentDir); err != nil {
			sendErr("directory not found")
			return
		}
	}

	fileList, err := listDirectoryEntries(currentDir)
	if err != nil {
		sendErr(err.Error())
		return
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":      "file_list",
		"serverId":  serverID,
		"directory": directory,
		"files":     fileList,
	})
}

func listDirectoryEntries(dir string) ([]FileListEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	items := make([]FileListEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == fileHistoryDirName {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		stats, err := os.Stat(fullPath)
		if err != nil {
			continue
		}
		item := FileListEntry{
			Name:        entry.Name(),
			IsDirectory: entry.IsDir(),
			Permissions: formatPermissions(stats.Mode().Perm()),
			Size:        0,
			MTime:       stats.ModTime(),
		}
		if !entry.IsDir() {
			item.Size = stats.Size()
		}
		items = append(items, item)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDirectory != items[j].IsDirectory {
			return items[i].IsDirectory
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return items, nil
}

func formatPermissions(mode os.FileMode) string {
	return fmt.Sprintf("%03o", mode&0o777)
}

var regexpPerm = regexp.MustCompile(`^[0-7]{3,4}$`)

func (s *Service) createFileHistorySnapshot(serverID int, filePath, absPath string) {
	stats, err := os.Stat(absPath)
	if err != nil || stats.IsDir() || stats.Size() > maxReadableHistoryBytes {
		return
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		return
	}

	historyDir, baseName, err := s.resolveFileHistoryLocation(serverID, filePath)
	if err != nil {
		return
	}
	if err := os.MkdirAll(historyDir, 0o750); err != nil {
		return
	}

	snapshotName := fmt.Sprintf("%s.%s.bak", baseName, time.Now().UTC().Format("20060102T150405.000"))
	targetPath, err := safeJoin(historyDir, snapshotName)
	if err != nil {
		return
	}
	if err := os.WriteFile(targetPath, raw, 0o640); err != nil {
		return
	}

	s.pruneFileHistorySnapshots(historyDir, baseName)
}

func (s *Service) resolveFileHistoryLocation(serverID int, filePath string) (string, string, error) {
	cleanPath := strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/")
	if cleanPath == "" {
		return "", "", fmt.Errorf("file path is required")
	}
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	cleanPath = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(cleanPath)), "/")
	if cleanPath == "" {
		return "", "", fmt.Errorf("invalid file path")
	}

	serverRoot := filepath.Clean(filepath.Join(s.volumesPath, strconv.Itoa(serverID)))
	historyRoot := filepath.Join(serverRoot, fileHistoryDirName)
	relativeDir := filepath.FromSlash(filepath.Dir(cleanPath))
	if relativeDir == "." {
		relativeDir = ""
	}
	baseName := filepath.Base(cleanPath)
	historyDir := filepath.Join(historyRoot, relativeDir)
	return historyDir, baseName, nil
}

func (s *Service) pruneFileHistorySnapshots(historyDir, baseName string) {
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return
	}

	prefix := baseName + "."
	type historyFile struct {
		Name  string
		MTime time.Time
	}
	files := make([]historyFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".bak") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		files = append(files, historyFile{Name: name, MTime: info.ModTime()})
	}

	sort.SliceStable(files, func(i, j int) bool {
		return files[i].MTime.After(files[j].MTime)
	})

	for idx := maxFileHistorySnapshots; idx < len(files); idx++ {
		targetPath, joinErr := safeJoin(historyDir, files[idx].Name)
		if joinErr != nil {
			continue
		}
		_ = os.Remove(targetPath)
	}
}
