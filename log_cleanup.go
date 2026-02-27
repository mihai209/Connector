package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type cleanupCandidate struct {
	Name  string
	Path  string
	Size  int64
	MTime time.Time
}

func (s *Service) handleLogCleanup(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	if serverID <= 0 {
		return
	}
	sendErr := func(text string) {
		s.sendServerError(serverID, text)
	}

	directory := strings.TrimSpace(asString(message["directory"]))
	if directory == "" {
		directory = "/logs"
	}

	maxFileSizeMB := asInt(message["maxFileSizeMB"])
	keepFiles := asInt(message["keepFiles"])
	maxAgeDays := asInt(message["maxAgeDays"])
	compressOld := asBool(message["compressOld"])

	if maxFileSizeMB <= 0 {
		maxFileSizeMB = 25
	}
	if keepFiles < 0 {
		keepFiles = 0
	}
	if maxAgeDays < 0 {
		maxAgeDays = 0
	}

	logDir, err := safeServerPath(s.volumesPath, serverID, directory)
	if err != nil {
		sendErr(err.Error())
		return
	}

	stats, statErr := os.Stat(logDir)
	if statErr != nil || !stats.IsDir() {
		_ = s.sendJSON(map[string]interface{}{
			"type":      "log_cleanup_result",
			"serverId":  serverID,
			"directory": directory,
			"rotated":   0,
			"deleted":   0,
			"kept":      0,
		})
		return
	}

	rotated, deleted := 0, 0
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024

	if entries, readErr := os.ReadDir(logDir); readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			entryName := entry.Name()
			entryPath, joinErr := safeJoin(logDir, entryName)
			if joinErr != nil {
				continue
			}
			info, infoErr := os.Stat(entryPath)
			if infoErr != nil || info.IsDir() {
				continue
			}
			if info.Size() <= maxBytes {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entryName), ".gz") {
				continue
			}

			rotatedPath := filepath.Join(logDir, fmt.Sprintf("%s.%s", entryName, time.Now().UTC().Format("20060102-150405")))
			if err := os.Rename(entryPath, rotatedPath); err != nil {
				continue
			}
			rotated++

			if compressOld {
				if gzPath, gzErr := compressFileToGzip(rotatedPath); gzErr == nil {
					_ = os.Remove(rotatedPath)
					_ = os.Chtimes(gzPath, info.ModTime(), info.ModTime())
				}
			}
		}
	}

	candidates := collectCleanupCandidates(logDir)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].MTime.After(candidates[j].MTime)
	})

	now := time.Now()
	cutoff := time.Time{}
	if maxAgeDays > 0 {
		cutoff = now.Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
	}

	kept := 0
	for idx, item := range candidates {
		removeForCount := keepFiles > 0 && idx >= keepFiles
		removeForAge := !cutoff.IsZero() && item.MTime.Before(cutoff)
		if removeForCount || removeForAge {
			if err := os.Remove(item.Path); err == nil {
				deleted++
			}
			continue
		}
		kept++
	}

	_ = s.sendJSON(map[string]interface{}{
		"type":      "log_cleanup_result",
		"serverId":  serverID,
		"directory": directory,
		"rotated":   rotated,
		"deleted":   deleted,
		"kept":      kept,
	})
}

func collectCleanupCandidates(logDir string) []cleanupCandidate {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}

	out := make([]cleanupCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path, joinErr := safeJoin(logDir, entry.Name())
		if joinErr != nil {
			continue
		}
		info, infoErr := os.Stat(path)
		if infoErr != nil || info.IsDir() {
			continue
		}
		out = append(out, cleanupCandidate{
			Name:  entry.Name(),
			Path:  path,
			Size:  info.Size(),
			MTime: info.ModTime(),
		})
	}
	return out
}

func compressFileToGzip(srcPath string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dstPath := srcPath + ".gz"
	dst, err := os.Create(dstPath)
	if err != nil {
		return "", err
	}

	gz := gzip.NewWriter(dst)
	_, copyErr := io.Copy(gz, src)
	closeErr := gz.Close()
	fileCloseErr := dst.Close()

	if copyErr != nil {
		_ = os.Remove(dstPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dstPath)
		return "", closeErr
	}
	if fileCloseErr != nil {
		_ = os.Remove(dstPath)
		return "", fileCloseErr
	}

	return dstPath, nil
}
