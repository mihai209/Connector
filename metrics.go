package main

import (
	"fmt"
	"net/http"
	"time"
)

func (s *Service) withMetrics(update func(*ConnectorMetrics)) {
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	update(&s.metrics)
}

func (s *Service) metricsSnapshot() ConnectorMetrics {
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	return s.metrics
}

func (s *Service) setWSConnected(connected bool) {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.WSConnected = connected
		if !connected {
			m.WSReconnects++
		}
	})
}

func (s *Service) recordCommandMetric(outcome string) {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.CommandReceivedTotal++
		if outcome == "executed" {
			m.CommandExecutedTotal++
		} else if outcome == "failed" {
			m.CommandFailedTotal++
		}
	})
}

func (s *Service) recordPowerMetric(outcome string) {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.PowerReceivedTotal++
		if outcome == "executed" {
			m.PowerExecutedTotal++
		} else if outcome == "failed" {
			m.PowerFailedTotal++
		}
	})
}

func (s *Service) recordScheduleMetric(outcome string) {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.ScheduleReceivedTotal++
		if outcome == "executed" {
			m.ScheduleExecutedTotal++
		} else if outcome == "failed" {
			m.ScheduleFailedTotal++
		}
	})
}

func (s *Service) recordResourceApplyMetric(outcome string) {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.ResourceApplyReceivedTotal++
		if outcome == "executed" {
			m.ResourceApplyExecutedTotal++
		} else if outcome == "failed" {
			m.ResourceApplyFailedTotal++
		}
	})
}

func (s *Service) recordCrashBundleMetric() {
	s.withMetrics(func(m *ConnectorMetrics) {
		m.CrashBundlesWrittenTotal++
	})
}

func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m := s.metricsSnapshot()
	now := time.Now().UTC()
	uptimeSeconds := now.Sub(m.StartTime).Seconds()
	if uptimeSeconds < 0 {
		uptimeSeconds = 0
	}

	wsConnected := 0
	if m.WSConnected {
		wsConnected = 1
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP connector_uptime_seconds Connector uptime in seconds.\n")
	fmt.Fprintf(w, "# TYPE connector_uptime_seconds gauge\n")
	fmt.Fprintf(w, "connector_uptime_seconds %.0f\n", uptimeSeconds)

	fmt.Fprintf(w, "# HELP connector_ws_connected WebSocket connection state to panel (1=connected).\n")
	fmt.Fprintf(w, "# TYPE connector_ws_connected gauge\n")
	fmt.Fprintf(w, "connector_ws_connected %d\n", wsConnected)

	fmt.Fprintf(w, "# HELP connector_ws_reconnects_total WebSocket reconnect attempts.\n")
	fmt.Fprintf(w, "# TYPE connector_ws_reconnects_total counter\n")
	fmt.Fprintf(w, "connector_ws_reconnects_total %d\n", m.WSReconnects)

	fmt.Fprintf(w, "# HELP connector_command_received_total Total server commands received.\n")
	fmt.Fprintf(w, "# TYPE connector_command_received_total counter\n")
	fmt.Fprintf(w, "connector_command_received_total %d\n", m.CommandReceivedTotal)
	fmt.Fprintf(w, "# HELP connector_command_executed_total Total server commands executed.\n")
	fmt.Fprintf(w, "# TYPE connector_command_executed_total counter\n")
	fmt.Fprintf(w, "connector_command_executed_total %d\n", m.CommandExecutedTotal)
	fmt.Fprintf(w, "# HELP connector_command_failed_total Total server commands failed.\n")
	fmt.Fprintf(w, "# TYPE connector_command_failed_total counter\n")
	fmt.Fprintf(w, "connector_command_failed_total %d\n", m.CommandFailedTotal)

	fmt.Fprintf(w, "# HELP connector_power_received_total Total power actions received.\n")
	fmt.Fprintf(w, "# TYPE connector_power_received_total counter\n")
	fmt.Fprintf(w, "connector_power_received_total %d\n", m.PowerReceivedTotal)
	fmt.Fprintf(w, "# HELP connector_power_executed_total Total power actions executed.\n")
	fmt.Fprintf(w, "# TYPE connector_power_executed_total counter\n")
	fmt.Fprintf(w, "connector_power_executed_total %d\n", m.PowerExecutedTotal)
	fmt.Fprintf(w, "# HELP connector_power_failed_total Total power actions failed.\n")
	fmt.Fprintf(w, "# TYPE connector_power_failed_total counter\n")
	fmt.Fprintf(w, "connector_power_failed_total %d\n", m.PowerFailedTotal)

	fmt.Fprintf(w, "# HELP connector_schedule_received_total Total schedule actions received.\n")
	fmt.Fprintf(w, "# TYPE connector_schedule_received_total counter\n")
	fmt.Fprintf(w, "connector_schedule_received_total %d\n", m.ScheduleReceivedTotal)
	fmt.Fprintf(w, "# HELP connector_schedule_executed_total Total schedule actions executed.\n")
	fmt.Fprintf(w, "# TYPE connector_schedule_executed_total counter\n")
	fmt.Fprintf(w, "connector_schedule_executed_total %d\n", m.ScheduleExecutedTotal)
	fmt.Fprintf(w, "# HELP connector_schedule_failed_total Total schedule actions failed.\n")
	fmt.Fprintf(w, "# TYPE connector_schedule_failed_total counter\n")
	fmt.Fprintf(w, "connector_schedule_failed_total %d\n", m.ScheduleFailedTotal)

	fmt.Fprintf(w, "# HELP connector_resource_apply_received_total Total live resource updates received.\n")
	fmt.Fprintf(w, "# TYPE connector_resource_apply_received_total counter\n")
	fmt.Fprintf(w, "connector_resource_apply_received_total %d\n", m.ResourceApplyReceivedTotal)
	fmt.Fprintf(w, "# HELP connector_resource_apply_executed_total Total live resource updates executed.\n")
	fmt.Fprintf(w, "# TYPE connector_resource_apply_executed_total counter\n")
	fmt.Fprintf(w, "connector_resource_apply_executed_total %d\n", m.ResourceApplyExecutedTotal)
	fmt.Fprintf(w, "# HELP connector_resource_apply_failed_total Total live resource updates failed.\n")
	fmt.Fprintf(w, "# TYPE connector_resource_apply_failed_total counter\n")
	fmt.Fprintf(w, "connector_resource_apply_failed_total %d\n", m.ResourceApplyFailedTotal)

	fmt.Fprintf(w, "# HELP connector_crash_bundles_written_total Total crash bundles written.\n")
	fmt.Fprintf(w, "# TYPE connector_crash_bundles_written_total counter\n")
	fmt.Fprintf(w, "connector_crash_bundles_written_total %d\n", m.CrashBundlesWrittenTotal)
}
