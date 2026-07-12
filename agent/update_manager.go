package agent

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henrygd/beszel/agent/utils"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

const (
	defaultUpdateInterval = 6 * time.Hour
	defaultUpdateTimeout  = 30 * time.Second
	defaultUpdateMaxPkgs  = 50
)

type updateOptions struct {
	interval    time.Duration
	timeout     time.Duration
	maxPackages int
}

type updateManager struct {
	mu          sync.RWMutex
	status      *updateentity.Status
	lastAttempt time.Time
	options     updateOptions
	systemd     *systemdManager
}

func newUpdateManager(systemd *systemdManager) *updateManager {
	enabled, _ := utils.GetEnv("UPDATE_MONITORING")
	if !strings.EqualFold(strings.TrimSpace(enabled), "true") {
		return nil
	}
	opts := updateOptions{interval: defaultUpdateInterval, timeout: defaultUpdateTimeout, maxPackages: defaultUpdateMaxPkgs}
	if raw, ok := utils.GetEnv("UPDATE_CHECK_INTERVAL"); ok {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= time.Minute {
			opts.interval = parsed
		} else {
			slog.Warn("Invalid UPDATE_CHECK_INTERVAL", "value", raw)
		}
	}
	if raw, ok := utils.GetEnv("UPDATE_CHECK_TIMEOUT"); ok {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= time.Second && parsed <= 5*time.Minute {
			opts.timeout = parsed
		} else {
			slog.Warn("Invalid UPDATE_CHECK_TIMEOUT", "value", raw)
		}
	}
	if raw, ok := utils.GetEnv("UPDATE_MAX_PACKAGE_LIST"); ok {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 && parsed <= 500 {
			opts.maxPackages = parsed
		} else {
			slog.Warn("Invalid UPDATE_MAX_PACKAGE_LIST", "value", raw)
		}
	}
	m := &updateManager{options: opts, systemd: systemd}
	go m.run()
	return m
}

func (m *updateManager) run() {
	m.refresh()
	ticker := time.NewTicker(m.options.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.refresh()
	}
}

func (m *updateManager) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), m.options.timeout)
	defer cancel()
	status, err := collectUpdateStatus(ctx, m.options, m.systemd)
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAttempt = now
	if status != nil {
		status.CollectedAt = now
		status.CacheAgeSeconds = 0
		if status.PendingSecurityUpdates != nil && *status.PendingSecurityUpdates > 0 {
			status.SecurityUpdatesSince = &now
			if m.status != nil && m.status.PendingSecurityUpdates != nil && *m.status.PendingSecurityUpdates > 0 && m.status.SecurityUpdatesSince != nil {
				status.SecurityUpdatesSince = m.status.SecurityUpdatesSince
			}
		}
		if status.RebootRequired {
			status.RebootRequiredSince = &now
			if m.status != nil && m.status.RebootRequired && m.status.RebootRequiredSince != nil {
				status.RebootRequiredSince = m.status.RebootRequiredSince
			}
		}
		if err != nil {
			status.LastError = sanitizeUpdateText(err.Error(), 512)
		}
		status.OverallState = updateentity.DeriveOverallState(status)
		m.status = status
		return
	}
	// A failed refresh never discards the last usable snapshot.
	if m.status != nil && err != nil {
		m.status.LastError = sanitizeUpdateText(err.Error(), 512)
	}
}

func (m *updateManager) snapshot() *updateentity.Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.status == nil {
		return &updateentity.Status{InstallationState: updateentity.InstallationUnknown, ConfigurationState: updateentity.ConfigurationUnknown, TimerState: updateentity.TimerUnknown, ServiceState: updateentity.ServiceUnknown, LastResult: updateentity.ResultUnknown, OverallState: updateentity.OverallMonitoringIncomplete, CollectedAt: m.lastAttempt}
	}
	copy := *m.status
	copy.RecentlyUpdatedPackages = append([]string(nil), m.status.RecentlyUpdatedPackages...)
	copy.RebootRequiredBy = append([]string(nil), m.status.RebootRequiredBy...)
	copy.DataSources = append([]string(nil), m.status.DataSources...)
	copy.Timers = append([]updateentity.UnitStatus(nil), m.status.Timers...)
	if !copy.CollectedAt.IsZero() {
		age := time.Since(copy.CollectedAt)
		if age > 0 {
			copy.CacheAgeSeconds = uint32(min(age/time.Second, time.Duration(^uint32(0))))
		}
	}
	return &copy
}

func sanitizeUpdateText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) > max {
		value = value[:max]
	}
	return value
}

func readPackageLines(data []byte, max int) []string {
	if max <= 0 {
		return nil
	}
	seen := make(map[string]struct{}, max)
	result := make([]string, 0, max)
	for line := range strings.Lines(string(data)) {
		name := strings.TrimSpace(line)
		if fields := strings.Fields(name); len(fields) > 0 {
			name = fields[0]
		}
		name = sanitizeUpdateText(name, 160)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
		if len(result) == max {
			break
		}
	}
	return result
}
