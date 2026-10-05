package maintenance

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func helperServiceProperties(t *testing.T, unit string) map[string]string {
	t.Helper()
	properties := make(map[string]string)
	inService := false
	for line := range strings.Lines(unit) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inService = line == "[Service]"
			continue
		}
		if !inService || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid service setting: %s", line)
		}
		switch key {
		case "ExecStart", "StandardInput", "StandardOutput", "StandardError":
			continue
		}
		if _, exists := properties[key]; exists {
			t.Fatalf("duplicate helper property: %s", key)
		}
		properties[key] = value
	}
	return properties
}

func shippedHelperProperties(t *testing.T) map[string]string {
	t.Helper()
	unit, err := os.ReadFile("../../supplemental/systemd/beszel-maintenance@.service")
	if err != nil {
		t.Fatal(err)
	}
	return helperServiceProperties(t, string(unit))
}

func TestHelperUnitsRetainAPTSandboxCapabilityWithoutElevatingAgent(t *testing.T) {
	shipped := shippedHelperProperties(t)
	for key, expected := range map[string]string{
		"User": "root", "Group": "root", "AmbientCapabilities": "CAP_SETUID",
		"NoNewPrivileges": "yes", "PrivateTmp": "yes", "ProtectHome": "yes",
		"ProtectKernelTunables": "yes", "ProtectKernelModules": "yes", "ProtectControlGroups": "yes",
		"RestrictAddressFamilies": "AF_UNIX AF_INET AF_INET6", "RestrictNamespaces": "yes",
		"LockPersonality": "yes", "MemoryDenyWriteExecute": "yes",
		"TimeoutStartSec": "infinity", "KillMode": "process",
	} {
		if shipped[key] != expected {
			t.Errorf("helper %s=%q; expected %q", key, shipped[key], expected)
		}
	}
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	_, template, ok := strings.Cut(script, "cat >/etc/systemd/system/beszel-maintenance@.service <<EOF\n")
	if !ok {
		t.Fatal("installer helper template not found")
	}
	template, _, ok = strings.Cut(template, "\nEOF")
	if !ok || !reflect.DeepEqual(shipped, helperServiceProperties(t, template)) {
		t.Fatal("installer and shipped helper protections differ")
	}
	if strings.Count(script, "AmbientCapabilities=CAP_SETUID") != 1 {
		t.Fatal("UID capability must appear only in the privileged helper template")
	}
	agent, err := os.ReadFile("../../supplemental/debian/beszel-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "User=beszel") || strings.Contains(string(agent), "AmbientCapabilities=") {
		t.Fatal("unprivileged Agent received additional capabilities")
	}
}

// This probe sends only the APT configuration message, never a download URI.
// It changes credentials in the disposable probe process, not on the machine.
const aptSandboxProbe = `
import json, os, pwd, subprocess, time

def status(pid="self"):
    with open("/proc/" + str(pid) + "/status") as f:
        return dict(line.strip().split(":", 1) for line in f if ":" in line)

before = status()
apt = pwd.getpwnam("_apt")
result = {
    "uid_before": os.getuid(), "apt_uid": apt.pw_uid,
    "cap_eff_before": before["CapEff"].strip(),
    "no_new_privs": int(before["NoNewPrivs"]), "seccomp": int(before["Seccomp"]),
    "methods": {}, "uid_errno": 0,
}
env = dict(os.environ, LC_ALL="C", LANG="C")
for method in ("http", "https"):
    proc = subprocess.Popen(["/usr/lib/apt/methods/" + method],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, env=env)
    observed = None
    try:
        proc.stdin.write("601 Configuration\nConfig-Item: APT::Sandbox::User=_apt\n\n")
        proc.stdin.flush()
        deadline = time.monotonic() + 5
        while proc.poll() is None and time.monotonic() < deadline:
            try:
                current = status(proc.pid)
                if all(int(uid) == apt.pw_uid for uid in current["Uid"].split()):
                    observed = current
                    break
            except FileNotFoundError:
                break
            time.sleep(0.01)
    finally:
        proc.stdin.close()
        proc.stdin = None
        try:
            stdout, stderr = proc.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            stdout, stderr = proc.communicate()
    result["methods"][method] = {
        "exit_code": proc.returncode,
        "uid_denied": "Failed to set new user ids - setresuid" in stdout + stderr,
        "entered_sandbox": observed is not None,
        "usable_capabilities": None if observed is None else [
            int(observed[key], 16) for key in ("CapEff", "CapPrm", "CapAmb")],
        "no_new_privs": None if observed is None else int(observed["NoNewPrivs"]),
    }
try:
    os.setgroups([apt.pw_gid])
    os.setresgid(apt.pw_gid, apt.pw_gid, apt.pw_gid)
    os.setresuid(apt.pw_uid, apt.pw_uid, apt.pw_uid)
except OSError as error:
    result["uid_errno"] = error.errno
after = status()
result.update(uid_after=os.getuid(), cap_eff_after=after["CapEff"].strip(),
    cap_prm_after=after["CapPrm"].strip(), cap_amb_after=after["CapAmb"].strip(),
    no_new_privs_after=int(after["NoNewPrivs"]))
print(json.dumps(result))
`

func TestSystemdMaintenanceSandboxAllowsAPTPrivilegeDrop(t *testing.T) {
	if os.Getenv("BESZEL_TEST_SYSTEMD") != "1" {
		t.Skip("set BESZEL_TEST_SYSTEMD=1 on a native systemd host with passwordless sudo")
	}
	properties := shippedHelperProperties(t)
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, fixed := range []bool{false, true} {
		name := "legacy_without_uid_capability"
		if fixed {
			name = "fixed_with_uid_capability"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"-n", "systemd-run", "--quiet", "--wait", "--pipe", "--collect"}
			for _, key := range keys {
				if key == "AmbientCapabilities" && !fixed {
					continue
				}
				args = append(args, "-p", key+"="+properties[key])
			}
			args = append(args, "/usr/bin/python3", "-c", aptSandboxProbe)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			output, err := exec.CommandContext(ctx, "sudo", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("native systemd probe failed: %v: %s", err, output)
			}
			var probe struct {
				UIDBefore       int    `json:"uid_before"`
				UIDAfter        int    `json:"uid_after"`
				AptUID          int    `json:"apt_uid"`
				UIDErrno        int    `json:"uid_errno"`
				CapEffBefore    string `json:"cap_eff_before"`
				CapEffAfter     string `json:"cap_eff_after"`
				CapPrmAfter     string `json:"cap_prm_after"`
				CapAmbAfter     string `json:"cap_amb_after"`
				NoNewPrivs      int    `json:"no_new_privs"`
				NoNewPrivsAfter int    `json:"no_new_privs_after"`
				Seccomp         int
				Methods         map[string]struct {
					ExitCode           int      `json:"exit_code"`
					UIDDenied          bool     `json:"uid_denied"`
					EnteredSandbox     bool     `json:"entered_sandbox"`
					UsableCapabilities []uint64 `json:"usable_capabilities"`
					NoNewPrivs         int      `json:"no_new_privs"`
				}
			}
			if err := json.Unmarshal(output, &probe); err != nil {
				t.Fatalf("invalid probe result: %v: %s", err, output)
			}
			mask, err := strconv.ParseUint(probe.CapEffBefore, 16, 64)
			if err != nil || (mask&(1<<7) != 0) != fixed {
				t.Fatalf("unexpected CAP_SETUID before drop: %s", output)
			}
			if probe.UIDBefore != 0 || probe.NoNewPrivs != 1 || probe.Seccomp != 2 || probe.AptUID <= 0 {
				t.Fatalf("probe did not use the hardened root context: %s", output)
			}
			for _, method := range []string{"http", "https"} {
				result, exists := probe.Methods[method]
				if !exists || (!fixed && (result.ExitCode != 112 || !result.UIDDenied || result.EnteredSandbox)) {
					t.Fatalf("unexpected %s method result: %s", method, output)
				}
				// Methods may return 100 on the deliberate protocol EOF. The live
				// credentials after configuration prove that sandbox setup worked.
				if fixed && ((!result.EnteredSandbox || result.UIDDenied || result.NoNewPrivs != 1) ||
					(result.ExitCode != 0 && result.ExitCode != 100) ||
					!reflect.DeepEqual(result.UsableCapabilities, []uint64{0, 0, 0})) {
					t.Fatalf("%s did not enter an unprivileged _apt sandbox: %s", method, output)
				}
			}
			if !fixed {
				if probe.UIDErrno != 1 || probe.UIDAfter != 0 {
					t.Fatalf("legacy context did not reproduce EPERM: %s", output)
				}
			} else if probe.UIDErrno != 0 || probe.UIDAfter != probe.AptUID || probe.NoNewPrivsAfter != 1 {
				t.Fatalf("fixed context could not enter _apt sandbox: %s", output)
			} else {
				for _, capability := range []string{probe.CapEffAfter, probe.CapPrmAfter, probe.CapAmbAfter} {
					mask, err := strconv.ParseUint(capability, 16, 64)
					if err != nil || mask != 0 {
						t.Fatalf("_apt retained usable capabilities: %s", output)
					}
				}
			}
			t.Logf("verified sandbox: %s", output)
		})
	}
}
