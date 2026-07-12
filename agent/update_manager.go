package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henrygd/beszel/agent/utils"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

const (
	defaultUpdateInterval   = 6 * time.Hour
	defaultUpdateTimeout    = 30 * time.Second
	defaultUpdateMaxPkgs    = 50
	defaultUpdateAPTTimeout = 2 * time.Minute
)

type updateOptions struct {
	interval    time.Duration
	timeout     time.Duration
	maxPackages int
	aptTimeout  time.Duration
}

type updateManager struct {
	mu          sync.RWMutex
	status      *updateentity.Status
	lastAttempt time.Time
	options     updateOptions
	systemd     *systemdManager
	statePath   string
	persisted   updatePersistentState
}

func newUpdateManager(systemd *systemdManager, dataDirs ...string) *updateManager {
	enabled, _ := utils.GetEnv("UPDATE_MONITORING")
	if !strings.EqualFold(strings.TrimSpace(enabled), "true") {
		return nil
	}
	opts := updateOptions{interval: defaultUpdateInterval, timeout: defaultUpdateTimeout, aptTimeout: defaultUpdateAPTTimeout, maxPackages: defaultUpdateMaxPkgs}
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
	if raw, ok := utils.GetEnv("UPDATE_APT_TIMEOUT"); ok {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 10*time.Second && parsed <= time.Hour {
			opts.aptTimeout = parsed
		} else {
			slog.Warn("Invalid UPDATE_APT_TIMEOUT", "value", raw)
		}
	}
	m := &updateManager{options: opts, systemd: systemd}
	dataDir := ""
	if len(dataDirs) > 0 {
		dataDir = dataDirs[0]
	}
	if dataDir != "" {
		m.statePath = filepath.Join(dataDir, "update-monitor-state.json")
		m.loadState()
	}
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
	status, err := collectUpdateStatus(context.Background(), m.options, m.systemd)
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAttempt = now
	if status != nil {
		status.CollectedAt = now
		status.CacheAgeSeconds = 0
		if status.PendingSecurityUpdates != nil && *status.PendingSecurityUpdates > 0 {
			status.SecurityUpdatesSince = &now
			if m.status != nil && m.status.SecurityUpdatesSince != nil {
				status.SecurityUpdatesSince = m.status.SecurityUpdatesSince
			} else if m.persisted.SecurityUpdatesSince != nil {
				status.SecurityUpdatesSince = m.persisted.SecurityUpdatesSince
			}
		}
		if status.RebootRequired {
			status.RebootRequiredSince = &now
			if m.status != nil && m.status.RebootRequiredSince != nil {
				status.RebootRequiredSince = m.status.RebootRequiredSince
			} else if m.persisted.RebootRequiredSince != nil {
				status.RebootRequiredSince = m.persisted.RebootRequiredSince
			}
		}
		if err != nil {
			status.LastError = sanitizeUpdateText(err.Error(), 512)
		}
		status.OverallState = updateentity.DeriveOverallState(status)
		if status.LastUpgradeAt != nil && m.persisted.LastManualUpgradeAt != nil {
			delta := status.LastUpgradeAt.Sub(*m.persisted.LastManualUpgradeAt)
			if delta < 0 {
				delta = -delta
			}
			if delta < 10*time.Minute {
				status.LastUpgradeSource = "manual"
			}
		}
		m.status = status
		m.saveState()
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
		if age > m.options.interval*2 {
			copy.DataStale = true
			copy.OverallState = updateentity.OverallMonitoringIncomplete
		}
	}
	return &copy
}

type updatePersistentState struct {
	SecurityUpdatesSince *time.Time `json:"security_updates_since,omitempty"`
	RebootRequiredSince  *time.Time `json:"reboot_required_since,omitempty"`
	LastManualUpgradeAt  *time.Time `json:"last_manual_upgrade_at,omitempty"`
}

func (m *updateManager) loadState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var state updatePersistentState
	if json.Unmarshal(data, &state) == nil {
		m.persisted = state
	}
}
func (m *updateManager) saveState() {
	if m.statePath == "" {
		return
	}
	state := m.persisted
	if m.status != nil {
		state.SecurityUpdatesSince = m.status.SecurityUpdatesSince
		state.RebootRequiredSince = m.status.RebootRequiredSince
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	temp := m.statePath + ".tmp"
	if os.WriteFile(temp, data, 0o600) == nil {
		_ = os.Rename(temp, m.statePath)
		m.persisted = state
	}
}

func (m *updateManager) markManualUpgrade(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at = at.UTC()
	m.persisted.LastManualUpgradeAt = &at
	m.saveState()
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
