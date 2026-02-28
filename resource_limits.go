package main

import (
	"fmt"
	"strconv"
	"strings"
)

func (s *Service) handleApplyResourceLimits(message map[string]interface{}) {
	serverID := asInt(message["serverId"])
	requestID := strings.TrimSpace(asString(message["requestId"]))
	if serverID <= 0 {
		s.recordResourceApplyMetric("failed")
		return
	}

	s.sendActionAck(serverID, "resource_limits", "accepted", "Live resource update accepted.", requestID, nil)

	containerName := fmt.Sprintf("cpanel-%d", serverID)
	args := []string{"update"}
	applied := map[string]interface{}{}

	memoryMB := asInt(message["memory"])
	if memoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", memoryMB))
		applied["memory"] = memoryMB
		if rawSwap, exists := message["swapLimit"]; exists {
			swapLimit := asInt(rawSwap)
			if swapLimit >= 0 {
				totalSwap := memoryMB + swapLimit
				args = append(args, "--memory-swap", fmt.Sprintf("%dm", totalSwap))
				applied["swapLimit"] = swapLimit
			} else {
				args = append(args, "--memory-swap", "-1")
				applied["swapLimit"] = -1
			}
		}
	}

	cpuPercent := asInt(message["cpu"])
	if cpuPercent > 0 {
		cpus := float64(cpuPercent) / 100.0
		args = append(args, "--cpus", fmt.Sprintf("%.2f", cpus))
		applied["cpu"] = cpuPercent
	}

	ioWeight := asInt(message["ioWeight"])
	if ioWeight > 0 {
		if ioWeight < 10 {
			ioWeight = 10
		}
		if ioWeight > 1000 {
			ioWeight = 1000
		}
		args = append(args, "--blkio-weight", strconv.Itoa(ioWeight))
		applied["ioWeight"] = ioWeight
	}

	if rawPids, exists := message["pidsLimit"]; exists {
		pidsLimit := asInt(rawPids)
		if pidsLimit >= 0 {
			args = append(args, "--pids-limit", strconv.Itoa(pidsLimit))
			applied["pidsLimit"] = pidsLimit
		}
	}

	if _, exists := message["oomKillDisable"]; exists {
		oomKillDisable := asBool(message["oomKillDisable"])
		args = append(args, "--oom-kill-disable", strconv.FormatBool(oomKillDisable))
		applied["oomKillDisable"] = oomKillDisable
	}

	if _, exists := message["oomScoreAdj"]; exists {
		oomScoreAdj := asInt(message["oomScoreAdj"])
		args = append(args, "--oom-score-adj", strconv.Itoa(oomScoreAdj))
		applied["oomScoreAdj"] = oomScoreAdj
	}

	if len(args) == 1 {
		s.recordResourceApplyMetric("failed")
		errMsg := "no live resource parameters provided"
		s.sendActionAck(serverID, "resource_limits", "failed", errMsg, requestID, nil)
		s.sendServerError(serverID, errMsg)
		return
	}

	args = append(args, containerName)
	output, err := runCommand("docker", args...)
	if err != nil {
		s.recordResourceApplyMetric("failed")
		s.sendActionAck(serverID, "resource_limits", "failed", err.Error(), requestID, nil)
		s.sendServerError(serverID, fmt.Sprintf("live limits apply failed: %v", err))
		_ = s.sendJSON(map[string]interface{}{
			"type":      "resource_limits_result",
			"serverId":  serverID,
			"success":   false,
			"error":     err.Error(),
			"requestId": requestID,
		})
		return
	}

	s.recordResourceApplyMetric("executed")
	s.sendActionAck(serverID, "resource_limits", "executed", "Live resource limits applied.", requestID, map[string]interface{}{
		"applied": applied,
	})
	_ = s.sendJSON(map[string]interface{}{
		"type":      "resource_limits_result",
		"serverId":  serverID,
		"success":   true,
		"applied":   applied,
		"output":    output,
		"requestId": requestID,
	})
}
