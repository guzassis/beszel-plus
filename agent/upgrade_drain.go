package agent

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

var upgradeDrainPath = "/run/beszel-agent/upgrade-in-progress"

type upgradeDrainState struct {
	PID int `json:"pid"`
}

func upgradeDrainActive() bool {
	return upgradeDrainActiveAt(upgradeDrainPath)
}

func upgradeDrainActiveAt(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state upgradeDrainState
	if json.Unmarshal(data, &state) != nil || state.PID <= 1 {
		_ = os.Remove(path)
		return false
	}
	err = syscall.Kill(state.PID, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		_ = os.Remove(path)
	}
	return false
}
