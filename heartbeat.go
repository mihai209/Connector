package main

import (
	"math"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

func (s *Service) startHeartbeat(done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.sendHeartbeat()
		}
	}
}

func (s *Service) sendHeartbeat() {
	cpuPct := 0.0
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		cpuPct = pct[0]
	}

	vm, _ := mem.VirtualMemory()
	dk, _ := disk.Usage("/")

	toGB := func(value uint64) int {
		return int(math.Round(float64(value) / (1024 * 1024 * 1024)))
	}

	payload := map[string]interface{}{
		"type": "heartbeat",
		"usage": map[string]interface{}{
			"cpu": math.Round(cpuPct),
			"memory": map[string]interface{}{
				"total": toGB(vm.Total),
				"used":  toGB(vm.Used),
				"free":  toGB(vm.Available),
			},
			"disk": map[string]interface{}{
				"total": toGB(dk.Total),
				"used":  toGB(dk.Used),
				"free":  toGB(dk.Free),
			},
		},
	}
	_ = s.sendJSON(payload)
}
