package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Service) repairExistingServerPermissions() {
	if strings.TrimSpace(s.volumesPath) == "" {
		return
	}

	if err := os.MkdirAll(s.volumesPath, 0o755); err != nil {
		bootWarn("permissions sweep skipped: cannot ensure volumes path=%s error=%v", s.volumesPath, err)
		return
	}

	entries, err := os.ReadDir(s.volumesPath)
	if err != nil {
		bootWarn("permissions sweep skipped: cannot read volumes path=%s error=%v", s.volumesPath, err)
		return
	}

	total := 0
	fixed := 0
	failed := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" {
			continue
		}
		serverID, parseErr := strconv.Atoi(name)
		if parseErr != nil || serverID <= 0 {
			continue
		}

		total++
		serverPath := filepath.Join(s.volumesPath, name)
		if err := s.fixServerPermissions(serverPath); err != nil {
			failed++
			bootWarn("permissions sweep failed server_id=%d path=%s error=%v", serverID, serverPath, err)
			continue
		}
		fixed++
	}

	bootInfo("permissions sweep completed total=%d fixed=%d failed=%d", total, fixed, failed)
}
