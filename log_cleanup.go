package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
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
	serverRoot, err := safeServerPath(s.volumesPath, serverID, "/")
	if err != nil {
		sendErr(err.Error())
		return
	}

	stats, statErr := secureStat(serverRoot, logDir)
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

	if entries, readErr := secureReadDir(serverRoot, logDir); readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			entryName := entry.Name()
			entryPath, joinErr := safeJoin(logDir, entryName)
			if joinErr != nil {
				continue
			}
			info, infoErr := secureStat(serverRoot, entryPath)
			if infoErr != nil || info.IsDir() {
				continue
			}
			if info.Size() <= maxBytes {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entryName), ".gz") {
				continue
			}

			rotatedPath, joinErr := safeJoin(logDir, fmt.Sprintf("%s.%s", entryName, time.Now().UTC().Format("20060102-150405")))
			if joinErr != nil {
				continue
			}
			if err := secureRename(serverRoot, entryPath, rotatedPath); err != nil {
				continue
			}
			rotated++

			if compressOld {
				if gzPath, gzErr := compressFileToGzip(serverRoot, rotatedPath); gzErr == nil {
					_ = secureRemove(serverRoot, rotatedPath)
					_ = os.Chtimes(gzPath, info.ModTime(), info.ModTime())
				}
			}
		}
	}

	candidates := collectCleanupCandidates(serverRoot, logDir)
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
			if err := secureRemove(serverRoot, item.Path); err == nil {
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

func collectCleanupCandidates(serverRoot, logDir string) []cleanupCandidate {
	entries, err := secureReadDir(serverRoot, logDir)
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
		info, infoErr := secureStat(serverRoot, path)
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

func compressFileToGzip(serverRoot, srcPath string) (string, error) {
	src, err := secureOpen(serverRoot, srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dstPath := srcPath + ".gz"
	dst, err := secureOpenFile(serverRoot, dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}

	gz := gzip.NewWriter(dst)
	_, copyErr := io.Copy(gz, src)
	closeErr := gz.Close()
	fileCloseErr := dst.Close()

	if copyErr != nil {
		_ = secureRemove(serverRoot, dstPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = secureRemove(serverRoot, dstPath)
		return "", closeErr
	}
	if fileCloseErr != nil {
		_ = secureRemove(serverRoot, dstPath)
		return "", fileCloseErr
	}

	return dstPath, nil
}
