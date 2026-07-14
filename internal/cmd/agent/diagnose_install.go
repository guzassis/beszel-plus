package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/henrygd/beszel/internal/aptstatus"
	"github.com/spf13/pflag"
)

type installDiagnostic struct {
	Version       int              `json:"version"`
	APTRequired   bool             `json:"apt_required"`
	CanProceed    bool             `json:"can_proceed"`
	WaitedSeconds int64            `json:"waited_seconds"`
	APT           aptstatus.Status `json:"apt"`
	Error         string           `json:"error,omitempty"`
}

func runDiagnoseInstall(args []string, stdout, stderr io.Writer, root string, interval time.Duration) int {
	flags := pflag.NewFlagSet("diagnose-install", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	aptRequired := flags.Bool("apt-required", false, "Whether the installer must modify packages")
	waitSeconds := flags.Int("wait-for-apt", 0, "Seconds to wait for APT/dpkg locks")
	jsonOutput := flags.Bool("json", false, "Print a machine-readable report")
	if err := flags.Parse(args); err != nil || *waitSeconds < 0 || *waitSeconds > 3600 || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "invalid diagnose-install arguments")
		return 64
	}

	report := installDiagnostic{Version: 1, APTRequired: *aptRequired, CanProceed: true}
	status, err := aptstatus.Inspect(root)
	if err != nil {
		report.Error = err.Error()
		report.CanProceed = !*aptRequired
	} else {
		report.APT = status
		if status.Busy && *aptRequired {
			printAPTHolders(stderr, status, "APT/dpkg is active; waiting for the package transaction to finish")
			status, waited, waitErr := aptstatus.Wait(context.Background(), root, time.Duration(*waitSeconds)*time.Second, interval, nil)
			report.APT = status
			report.WaitedSeconds = int64(waited.Round(time.Second) / time.Second)
			if waitErr != nil {
				report.Error = waitErr.Error()
			}
			report.CanProceed = waitErr == nil && !status.Busy
		} else if status.Busy {
			printAPTHolders(stderr, status, "APT/dpkg is active, but this install does not require package changes; continuing")
		}
	}

	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(report)
	} else {
		fmt.Fprintf(stdout, "APT required: %t\nAPT busy: %t\nInstall can proceed: %t\n", report.APTRequired, report.APT.Busy, report.CanProceed)
		if report.Error != "" {
			fmt.Fprintf(stdout, "Diagnostic error: %s\n", report.Error)
		}
	}
	if !report.CanProceed {
		return 75
	}
	return 0
}

func printAPTHolders(output io.Writer, status aptstatus.Status, message string) {
	fmt.Fprintln(output, message+".")
	for _, holder := range status.Holders {
		command := holder.Command
		if command == "" {
			command = "unknown"
		}
		unit := holder.Unit
		if unit == "" {
			unit = "unknown"
		}
		fmt.Fprintf(output, "  PID %d, command %s, unit %s, locks %s\n", holder.PID, command, unit, strings.Join(holder.Locks, ", "))
	}
}
