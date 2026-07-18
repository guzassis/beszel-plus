#!/bin/sh

PRODUCT_NAME="Beszel Plus"
PRODUCT_VERSION="0.2.7"
REPOSITORY="guzassis/beszel-plus"

is_alpine() {
  [ -f /etc/alpine-release ]
}

is_openwrt() {
  grep -qi "OpenWrt" /etc/os-release
}

is_freebsd() {
  [ "$(uname -s)" = "FreeBSD" ]
}

is_opnsense() {
  [ -f /usr/local/sbin/opnsense-version ] || [ -f /usr/local/etc/opnsense-version ] || [ -f /etc/opnsense-release ]
}

is_glibc() {
  # Prefer glibc-enabled agent (NVML via purego) on linux/amd64 glibc systems.
  # Check common dynamic loader paths first (fast + reliable).
  for p in \
    /lib64/ld-linux-x86-64.so.2 \
    /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 \
    /lib/ld-linux-x86-64.so.2; do
    [ -e "$p" ] && return 0
  done

  # Fallback to ldd output if available.
  if command -v ldd >/dev/null 2>&1; then
    ldd --version 2>&1 | grep -qiE 'gnu libc|glibc' && return 0
  fi

  return 1
}


# If SELinux is enabled, set the context of the binary
set_selinux_context() {
  # Check if SELinux is enabled and in enforcing or permissive mode
  if command -v getenforce >/dev/null 2>&1; then
    SELINUX_MODE=$(getenforce)
    if [ "$SELINUX_MODE" != "Disabled" ]; then
      echo "SELinux is enabled (${SELINUX_MODE} mode). Setting appropriate context..."

      # First try to set persistent context if semanage is available
      if command -v semanage >/dev/null 2>&1; then
        echo "Attempting to set persistent SELinux context..."
        if semanage fcontext -a -t bin_t "$BIN_PATH" >/dev/null 2>&1; then
          restorecon -v "$BIN_PATH" >/dev/null 2>&1
        else
          echo "Warning: Failed to set persistent context, falling back to temporary context."
        fi
      fi

      # Fall back to chcon if semanage failed or isn't available
      if command -v chcon >/dev/null 2>&1; then
        # Set context for both the directory and binary
        chcon -t bin_t "$BIN_PATH" || echo "Warning: Failed to set SELinux context for binary."
        chcon -R -t bin_t "$AGENT_DIR" || echo "Warning: Failed to set SELinux context for directory."
      else
        if [ "$SELINUX_MODE" = "Enforcing" ]; then
          echo "Warning: SELinux is in enforcing mode but chcon command not found. The service may fail to start."
          echo "Consider installing the policycoreutils package or temporarily setting SELinux to permissive mode."
        else
          echo "Warning: SELinux is in permissive mode but chcon command not found."
        fi
      fi
    fi
  fi
}

# Clean up SELinux contexts if they were set
cleanup_selinux_context() {
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ]; then
    echo "Cleaning up SELinux contexts..."
    # Remove persistent context if semanage is available
    if command -v semanage >/dev/null 2>&1; then
      semanage fcontext -d "$BIN_PATH" 2>/dev/null || true
    fi
  fi
}

# Ensure the proxy URL ends with a /
ensure_trailing_slash() {
  if [ -n "$1" ]; then
    case "$1" in
    */) echo "$1" ;;
    *) echo "$1/" ;;
    esac
  else
    echo "$1"
  fi
}

# Generate FreeBSD rc service content
generate_freebsd_rc_service() {
  cat <<'EOF'
#!/bin/sh

# PROVIDE: beszel_agent
# REQUIRE: DAEMON NETWORKING
# BEFORE: LOGIN
# KEYWORD: shutdown

# Add the following lines to /etc/rc.conf to configure Beszel Plus Agent:
#
# beszel_agent_enable (bool):   Set to YES to enable Beszel Plus Agent
#                               Default: YES
# beszel_agent_env_file (str):  Beszel Plus Agent env configuration file
#                               Default: /usr/local/etc/beszel-agent/env
# beszel_agent_user (str):      Beszel Plus Agent daemon user
#                               Default: beszel
# beszel_agent_bin (str):       Path to the beszel-agent binary
#                               Default: /usr/local/sbin/beszel-agent
# beszel_agent_flags (str):     Extra flags passed to beszel-agent command invocation
#                               Default:

. /etc/rc.subr

name="beszel_agent"
rcvar=beszel_agent_enable

load_rc_config $name
: ${beszel_agent_enable:="YES"}
: ${beszel_agent_user:="beszel"}
: ${beszel_agent_flags:=""}
: ${beszel_agent_env_file:="/usr/local/etc/beszel-agent/env"}
: ${beszel_agent_bin:="/usr/local/sbin/beszel-agent"}

logfile="/var/log/${name}.log"
pidfile="/var/run/${name}.pid"

procname="/usr/sbin/daemon"
start_precmd="${name}_prestart"
start_cmd="${name}_start"
stop_cmd="${name}_stop"

extra_commands="upgrade"
upgrade_cmd="beszel_agent_upgrade"

beszel_agent_prestart()
{
    if [ ! -f "${beszel_agent_env_file}" ]; then
        echo WARNING: missing "${beszel_agent_env_file}" env file. Start aborted.
        exit 1
    fi
}

beszel_agent_start()
{
    echo "Starting ${name}"
    /usr/sbin/daemon -fc \
            -P "${pidfile}" \
            -o "${logfile}" \
            -u "${beszel_agent_user}" \
            "${beszel_agent_bin}" ${beszel_agent_flags}
}

beszel_agent_stop()
{
    pid="$(check_pidfile "${pidfile}" "${procname}")"
    if [ -n "${pid}" ]; then
        echo "Stopping ${name} (pid=${pid})"
        kill -- "-${pid}"
        wait_for_pids "${pid}"
    else
        echo "${name} isn't running"
    fi
}

beszel_agent_upgrade()
{
    echo "Upgrading ${name}"
    if command -v sudo >/dev/null; then
        sudo -u "${beszel_agent_user}" -- "${beszel_agent_bin}" update
    else
        su -m "${beszel_agent_user}" -c "${beszel_agent_bin} update"
    fi
}

run_rc_command "$1"
EOF
}

# Detect system architecture
detect_architecture() {
  arch=$(uname -m)

  if [ "$arch" = "mips" ]; then
    detect_mips_endianness
    return $?
  fi

  case "$arch" in
    x86_64)
      arch="amd64"
      ;;
    armv6l|armv7l)
      arch="arm"
      ;;
    aarch64)
      arch="arm64"
      ;;
  esac

  echo "$arch"
}

# Detect MIPS endianness using ELF header
detect_mips_endianness() {
  bins="/bin/sh /bin/ls /usr/bin/env"
  
  for bin_to_check in $bins; do
    if [ -f "$bin_to_check" ]; then
      # The 6th byte in ELF header: 01 = little, 02 = big
      endian=$(hexdump -n 1 -s 5 -e '1/1 "%02x"' "$bin_to_check" 2>/dev/null)
      if [ "$endian" = "01" ]; then
        echo "mipsle"
        return
      elif [ "$endian" = "02" ]; then
        echo "mips" 
        return
      fi
    fi
  done
  
  # Final fallback
  echo "mips"
}

# Default values
PORT=45876
PORT_PROVIDED=false
UNINSTALL=false
GITHUB_URL="https://github.com"
GITHUB_PROXY_URL=""
KEY=""
KEY_PROVIDED=false
TOKEN=""
TOKEN_PROVIDED=false
HUB_URL=""
HUB_URL_PROVIDED=false
AUTO_UPDATE_FLAG="" # empty string means prompt, "true" means auto-enable, "false" means skip
OS_UPDATE_MANAGEMENT_FLAG=""
POWER_MANAGEMENT_FLAG=""
OS_UPDATE_POLICY="security"
VERSION="latest"
ENROLLMENT_FAILURE=false
ENROLLMENT_FAILURE_REASON=""
POLICY_PENDING=false
WAIT_FOR_MAINTENANCE=5
WAIT_FOR_APT=60
VERBOSE=false
DIAGNOSE=false

# Check for help flag
case "$1" in
-h | --help)
  printf "Beszel Plus Agent installation script\n\n"
  printf "Usage: ./install-agent.sh [options]\n\n"
  printf "Options: \n"
  printf "  -k                    : SSH key (required, or interactive if not provided)\n"
  printf "  -p                    : Port (default: %s)\n" "$PORT"
  printf "  -t                    : Token (optional for backwards compatibility)\n"
  printf "  -url                  : Hub URL (optional for backwards compatibility)\n"
  printf "  -v, --version         : Version to install (default: latest)\n"
  printf "  -u                    : Uninstall Beszel Plus Agent\n"
	printf "  --agent-auto-update=true|false : Control automatic updates of the Agent binary\n"
	printf "  --auto-update [VALUE] : Deprecated alias for --agent-auto-update\n"
	printf "  --os-update-management=true|false : Install OS update management components\n"
	printf "  --power-management=true|false : Enable WOL diagnostics and secure shutdown\n"
	printf "  --os-update-policy=monitor|security|official-all : Initial policy for new installations\n"
	printf "  --wait-for-maintenance=SECONDS : Wait up to 5 seconds by default (maximum 300)\n"
	printf "  --wait-for-apt=SECONDS : Wait for required APT access (default: 60, maximum: 3600)\n"
	printf "  --verbose             : Show detailed maintenance preflight diagnostics\n"
	printf "  --diagnose            : Print install readiness without changing the system\n"
  printf "                          VALUE can be true (enable) or false (disable). If not specified, will prompt.\n"
  printf "  --mirror [URL]        : Use GitHub proxy to resolve network timeout issues in mainland China\n"
  printf "                          URL: optional custom proxy URL (default: https://gh.beszel.dev)\n"
  printf "  -h, --help            : Display this help message\n"
  exit 0
  ;;
esac

systemd_escape_environment() {
  # systemd double-quoted Environment= values require backslash and quote escaping.
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

run_with_portable_timeout() {
  timeout_seconds="$1"
  shift
  "$@" </dev/null &
  timeout_pid=$!
  (
    sleep "$timeout_seconds"
    kill -TERM "$timeout_pid" 2>/dev/null || true
  ) &
  watchdog_pid=$!
  wait "$timeout_pid"
  timeout_status=$?
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  return "$timeout_status"
}

detect_existing_helper_version() {
  helper_path="$1"
  OLD_HELPER_VERSION="legacy"
  OLD_HELPER_VERSION_OUTPUT=""
  if command -v timeout >/dev/null 2>&1; then
    OLD_HELPER_VERSION_OUTPUT=$(timeout 3s "$helper_path" --version </dev/null 2>/dev/null || true)
  else
    OLD_HELPER_VERSION_OUTPUT=$(run_with_portable_timeout 3 "$helper_path" --version 2>/dev/null || true)
  fi
  DETECTED_HELPER_VERSION=$(printf '%s\n' "$OLD_HELPER_VERSION_OUTPUT" | sed -n -e 's/^Beszel Plus Maintenance Helper v//p' -e 's/^beszel-maintenance-helper v//p' | head -n 1)
  if [ -n "$DETECTED_HELPER_VERSION" ]; then
    OLD_HELPER_VERSION="$DETECTED_HELPER_VERSION"
  else
    echo "Existing helper does not support version reporting; treating as legacy."
  fi
}

# Check if running as root and re-execute with sudo if needed
if [ "$(id -u)" != "0" ]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -- "$0" "$@"
  else
    echo "This script must be run as root. Please either:"
    echo "1. Run this script as root (su root)"
    echo "2. Install sudo and run with sudo"
    exit 1
  fi
fi

# Parse arguments
while [ $# -gt 0 ]; do
  case "$1" in
  -k)
    [ "$#" -ge 2 ] || { echo "Missing value for -k" >&2; exit 64; }
    shift
    KEY="$1"
    KEY_PROVIDED=true
    ;;
  -p)
    [ "$#" -ge 2 ] || { echo "Missing value for -p" >&2; exit 64; }
    shift
    PORT="$1"
    PORT_PROVIDED=true
    ;;
  -t)
    [ "$#" -ge 2 ] || { echo "Missing value for -t" >&2; exit 64; }
    shift
    TOKEN="$1"
    TOKEN_PROVIDED=true
    ;;
  -url)
    [ "$#" -ge 2 ] || { echo "Missing value for -url" >&2; exit 64; }
    shift
    HUB_URL="$1"
    HUB_URL_PROVIDED=true
    ;;
  -v | --version)
    [ "$#" -ge 2 ] || { echo "Missing value for $1" >&2; exit 64; }
    shift
    VERSION="$1"
    ;;
  -u)
    UNINSTALL=true
    ;;
  --mirror* | --china-mirrors*)
    # Check if there's a value after the = sign
    if echo "$1" | grep -q "="; then
      # Extract the value after =
      CUSTOM_PROXY=$(echo "$1" | cut -d'=' -f2)
      if [ -n "$CUSTOM_PROXY" ]; then
        GITHUB_PROXY_URL="$CUSTOM_PROXY"
        GITHUB_URL="$(ensure_trailing_slash "$CUSTOM_PROXY")https://github.com"
      else
        GITHUB_PROXY_URL="https://gh.beszel.dev"
        GITHUB_URL="$GITHUB_PROXY_URL"
      fi
    elif [ "$2" != "" ] && ! echo "$2" | grep -q '^-'; then
      # use custom proxy URL provided as next argument
      GITHUB_PROXY_URL="$2"
      GITHUB_URL="$(ensure_trailing_slash "$2")https://github.com"
      shift
    else
      # No value specified, use default
      GITHUB_PROXY_URL="https://gh.beszel.dev"
      GITHUB_URL="$GITHUB_PROXY_URL"
    fi
    ;;
	--agent-auto-update*)
		if echo "$1" | grep -q "="; then AUTO_UPDATE_VALUE=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --agent-auto-update value" >&2; exit 64; }; AUTO_UPDATE_VALUE="$2"; shift; fi
		if [ "$AUTO_UPDATE_VALUE" = "true" ] || [ "$AUTO_UPDATE_VALUE" = "false" ]; then AUTO_UPDATE_FLAG="$AUTO_UPDATE_VALUE"; else echo "Invalid --agent-auto-update value" >&2; exit 1; fi
		;;
	--os-update-management*)
		if echo "$1" | grep -q "="; then OS_UPDATE_VALUE=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --os-update-management value" >&2; exit 64; }; OS_UPDATE_VALUE="$2"; shift; fi
		if [ "$OS_UPDATE_VALUE" = "true" ] || [ "$OS_UPDATE_VALUE" = "false" ]; then OS_UPDATE_MANAGEMENT_FLAG="$OS_UPDATE_VALUE"; else echo "Invalid --os-update-management value" >&2; exit 1; fi
		;;
	--os-update-policy*)
		if echo "$1" | grep -q "="; then OS_UPDATE_POLICY=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --os-update-policy value" >&2; exit 64; }; OS_UPDATE_POLICY="$2"; shift; fi
		case "$OS_UPDATE_POLICY" in monitor|security|official-all) ;; *) echo "Invalid --os-update-policy value" >&2; exit 1 ;; esac
		;;
	--power-management*)
		if echo "$1" | grep -q "="; then POWER_VALUE=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --power-management value" >&2; exit 64; }; POWER_VALUE="$2"; shift; fi
		if [ "$POWER_VALUE" = "true" ] || [ "$POWER_VALUE" = "false" ]; then POWER_MANAGEMENT_FLAG="$POWER_VALUE"; else echo "Invalid --power-management value" >&2; exit 1; fi
		;;
	--wait-for-maintenance*)
		if echo "$1" | grep -q "="; then WAIT_FOR_MAINTENANCE=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --wait-for-maintenance value" >&2; exit 64; }; WAIT_FOR_MAINTENANCE="$2"; shift; fi
		case "$WAIT_FOR_MAINTENANCE" in ''|*[!0-9]*) echo "Invalid --wait-for-maintenance value" >&2; exit 1 ;; esac
		if [ "$WAIT_FOR_MAINTENANCE" -gt 300 ]; then echo "--wait-for-maintenance cannot exceed 300 seconds" >&2; exit 1; fi
		;;
	--wait-for-apt*)
		if echo "$1" | grep -q "="; then WAIT_FOR_APT=$(echo "$1" | cut -d'=' -f2); else [ "$#" -ge 2 ] || { echo "Missing --wait-for-apt value" >&2; exit 64; }; WAIT_FOR_APT="$2"; shift; fi
		case "$WAIT_FOR_APT" in ''|*[!0-9]*) echo "Invalid --wait-for-apt value" >&2; exit 1 ;; esac
		if [ "$WAIT_FOR_APT" -gt 3600 ]; then echo "--wait-for-apt cannot exceed 3600 seconds" >&2; exit 1; fi
		;;
	--verbose)
		VERBOSE=true
		;;
	--diagnose)
		DIAGNOSE=true
		;;
  --auto-update*)
		echo "Warning: --auto-update is deprecated; use --agent-auto-update." >&2
    # Check if there's a value after the = sign
    if echo "$1" | grep -q "="; then
      # Extract the value after =
      AUTO_UPDATE_VALUE=$(echo "$1" | cut -d'=' -f2)
      if [ "$AUTO_UPDATE_VALUE" = "true" ]; then
        AUTO_UPDATE_FLAG="true"
      elif [ "$AUTO_UPDATE_VALUE" = "false" ]; then
        AUTO_UPDATE_FLAG="false"
      else
        echo "Invalid value for --auto-update flag: $AUTO_UPDATE_VALUE. Using default (prompt)."
      fi
    elif [ "$2" = "true" ] || [ "$2" = "false" ]; then
      # Value provided as next argument
      AUTO_UPDATE_FLAG="$2"
      shift
    else
      # No value specified, use true
      AUTO_UPDATE_FLAG="true"
    fi
    ;;
  *)
    echo "Invalid option: $1" >&2
    exit 1
    ;;
  esac
  shift
done

if [ "$(uname -s)" != "Linux" ] || ! grep -Eq '^ID=("?)(debian|ubuntu|raspbian)\1$' /etc/os-release 2>/dev/null; then
  echo "$PRODUCT_NAME v$PRODUCT_VERSION supports only Debian, Ubuntu, or Raspbian Linux in this release." >&2
  exit 1
fi
SUPPORTED_ARCH=$(detect_architecture)
case "$SUPPORTED_ARCH" in amd64|arm64) ;; *) echo "$PRODUCT_NAME v$PRODUCT_VERSION supports only amd64 and arm64 (detected: $SUPPORTED_ARCH)." >&2; exit 1 ;; esac

# Set paths based on operating system
if is_freebsd; then
  AGENT_DIR="/usr/local/etc/beszel-agent"
  BIN_DIR="/usr/local/sbin"
  BIN_PATH="/usr/local/sbin/beszel-agent"
else
  AGENT_DIR="/opt/beszel-agent"
  BIN_DIR="/opt/beszel-agent"
  BIN_PATH="/opt/beszel-agent/beszel-agent"
fi
MAINTENANCE_HELPER_PATH="/usr/local/libexec/beszel/maintenance-helper"
AGENT_ENV_PATH="/etc/beszel-agent/agent.env"
MAINTENANCE_POLICY_PATH="/var/lib/beszel-maintenance/policy.json"
MAINTENANCE_STATUS_PATH="/var/lib/beszel-maintenance/status.json"
DRAIN_PATH="/run/beszel-agent/upgrade-in-progress"
INSTALL_LOCK_PATH="/run/lock/beszel-plus-agent-install.lock"
ROLLBACK_REPORT_PATH="/var/lib/beszel-maintenance/last-install-rollback.json"
PHASE="PHASE_INITIAL"
TRANSACTION_ACTIVE=false
TRANSACTION_DIR=""
TEMP_DIR=""
QUIESCE_ACTIVE=false
AGENT_PREVIOUS_STATE="not-found"
SOCKET_PREVIOUS_STATE="not-found"
LOCK_KIND=""
LOCK_DIR=""
AGENT_REPLACED=false
HELPER_REPLACED=false
HELPER_REMOVED=false
AGENT_SERVICE_CHANGED=false
SOCKET_CHANGED=false
TEMPLATE_CHANGED=false
MONITORING_DROPIN_CHANGED=false
CONNECTION_DROPIN_CHANGED=false
AGENT_ENV_CHANGED=false
UPDATE_SERVICE_CHANGED=false
UPDATE_TIMER_CHANGED=false
ROLLBACK_FAILED_COMPONENTS=""
ROLLBACK_RESTORED_COMPONENTS=""
APT_TIMERS_QUIESCED=false
APT_DAILY_TIMER_WAS_ACTIVE=false
APT_UPGRADE_TIMER_WAS_ACTIVE=false

cleanup_temp_dir() {
  if [ -n "${TEMP_DIR:-}" ]; then
    rm -rf "$TEMP_DIR"
    TEMP_DIR=""
  fi
}

json_string_value() {
  json_file="$1"
  json_key="$2"
  [ -r "$json_file" ] || return 1
  sed -n "s/.*\"${json_key}\":\"\([^\"]*\)\".*/\1/p" "$json_file" | head -n 1
}

service_state() {
  state_unit="$1"
  [ "$(systemctl show "$state_unit" -p LoadState --value 2>/dev/null)" != "not-found" ] || { echo "not-found"; return; }
  systemctl show "$state_unit" -p ActiveState --value 2>/dev/null || echo "unknown"
}

restore_quiesced_services() {
  [ "$QUIESCE_ACTIVE" = "true" ] || return 0
  restore_services_ok=true
  if [ "$SOCKET_PREVIOUS_STATE" = "active" ]; then
    systemctl start beszel-maintenance.socket >/dev/null 2>&1 || restore_services_ok=false
  fi
  if [ "$AGENT_PREVIOUS_STATE" = "active" ]; then
    systemctl start beszel-agent.service >/dev/null 2>&1 || restore_services_ok=false
  fi
  QUIESCE_ACTIVE=false
  [ "$restore_services_ok" = "true" ]
}

pause_apt_timers() {
  if systemctl is-active --quiet apt-daily.timer 2>/dev/null; then APT_DAILY_TIMER_WAS_ACTIVE=true; fi
  if systemctl is-active --quiet apt-daily-upgrade.timer 2>/dev/null; then APT_UPGRADE_TIMER_WAS_ACTIVE=true; fi
  APT_TIMERS_QUIESCED=true
  systemctl stop apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
}

restore_apt_timers() {
  [ "$APT_TIMERS_QUIESCED" = "true" ] || return 0
  apt_restore_ok=true
  if [ "$APT_DAILY_TIMER_WAS_ACTIVE" = "true" ]; then systemctl start apt-daily.timer >/dev/null 2>&1 || apt_restore_ok=false; fi
  if [ "$APT_UPGRADE_TIMER_WAS_ACTIVE" = "true" ]; then systemctl start apt-daily-upgrade.timer >/dev/null 2>&1 || apt_restore_ok=false; fi
  APT_TIMERS_QUIESCED=false
  [ "$apt_restore_ok" = "true" ]
}

clear_upgrade_drain() {
  if [ -f "$DRAIN_PATH" ] && grep -q "\"pid\":$$" "$DRAIN_PATH" 2>/dev/null; then
    rm -f "$DRAIN_PATH"
  fi
}

append_component() {
  append_kind="$1"
  append_name="$2"
  if [ "$append_kind" = "restored" ]; then
    if [ -n "$ROLLBACK_RESTORED_COMPONENTS" ]; then ROLLBACK_RESTORED_COMPONENTS="$ROLLBACK_RESTORED_COMPONENTS,$append_name"; else ROLLBACK_RESTORED_COMPONENTS="$append_name"; fi
  else
    if [ -n "$ROLLBACK_FAILED_COMPONENTS" ]; then ROLLBACK_FAILED_COMPONENTS="$ROLLBACK_FAILED_COMPONENTS,$append_name"; else ROLLBACK_FAILED_COMPONENTS="$append_name"; fi
  fi
}

csv_json_array() {
  csv_value="$1"
  if [ -z "$csv_value" ]; then printf '[]'; return; fi
  printf '["%s"]' "$(printf '%s' "$csv_value" | sed 's/,/","/g')"
}

backup_transaction_file() {
  transaction_path="$1"
  transaction_name="$2"
  if [ -e "$transaction_path" ]; then
    cp -p "$transaction_path" "$TRANSACTION_DIR/$transaction_name"
  else
    : >"$TRANSACTION_DIR/$transaction_name.missing"
  fi
}

wait_unit_inactive() {
  wait_unit="$1"
  wait_limit="$2"
  waited=0
  while [ "$waited" -lt "$wait_limit" ]; do
    [ "$(systemctl show "$wait_unit" -p ActiveState --value 2>/dev/null)" = "active" ] || return 0
    sleep 1
    waited=$((waited + 1))
    if [ "$VERBOSE" = "true" ]; then echo "Maintenance preflight: $wait_unit still active after ${waited}s."; fi
  done
  [ "$(systemctl show "$wait_unit" -p ActiveState --value 2>/dev/null)" != "active" ]
}

maintenance_preflight() {
  PREFLIGHT_UNITS_FILE=$(mktemp)
  systemctl list-units --all --no-legend 'beszel-maintenance@*.service' 2>/dev/null >"$PREFLIGHT_UNITS_FILE" || true
  status_operation=$(json_string_value "$MAINTENANCE_STATUS_PATH" operation 2>/dev/null || true)
  status_request_id=$(json_string_value "$MAINTENANCE_STATUS_PATH" request_id 2>/dev/null || true)
  status_started_at=$(json_string_value "$MAINTENANCE_STATUS_PATH" started_at 2>/dev/null || true)
  status_state=$(json_string_value "$MAINTENANCE_STATUS_PATH" status 2>/dev/null || true)
  while IFS=' ' read -r unit _rest; do
    case "$unit" in beszel-maintenance@*.service) ;; *) continue ;; esac
    active_state=$(systemctl show "$unit" -p ActiveState --value 2>/dev/null || true)
    sub_state=$(systemctl show "$unit" -p SubState --value 2>/dev/null || true)
    pid=$(systemctl show "$unit" -p MainPID --value 2>/dev/null || true)
    exec_pid=$(systemctl show "$unit" -p ExecMainPID --value 2>/dev/null || true)
    started=$(systemctl show "$unit" -p ActiveEnterTimestamp --value 2>/dev/null || true)
    elapsed="unknown"
    children="none"
    held_locks="none"
    cmdline="unknown"
    [ "$active_state" = "active" ] || continue
    case "$pid" in ''|0|*[!0-9]*) operation="unknown" ;; *)
      exe=$(readlink "/proc/$pid/exe" 2>/dev/null || true)
      cmdline=$(tr '\000' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)
      [ -n "$cmdline" ] || cmdline="unknown"
      elapsed=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ' || true)
      [ -n "$elapsed" ] || elapsed="unknown"
      if command -v pgrep >/dev/null 2>&1; then children=$(pgrep -P "$pid" 2>/dev/null | tr '\n' ' ' || true); [ -n "$children" ] || children="none"; fi
      if command -v fuser >/dev/null 2>&1; then held_locks=$(fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock 2>/dev/null | tr '\n' ' ' || true); [ -n "$held_locks" ] || held_locks="none"; fi
      cgroup=$(cat "/proc/$pid/cgroup" 2>/dev/null || true)
      if [ "$exe" != "$MAINTENANCE_HELPER_PATH" ] || ! printf '%s' "$cgroup" | grep -Fq "$unit"; then
        operation="unknown"
      elif [ "$status_state" = "running" ] && [ -n "$status_operation" ]; then
        operation="$status_operation"
      else
        operation="unknown"
      fi
      ;;
    esac
    if [ "$VERBOSE" = "true" ]; then
      echo "Maintenance unit: $unit MainPID=$pid ExecMainPID=$exec_pid state=$active_state/$sub_state operation=$operation request_id=${status_request_id:-unknown} started=${status_started_at:-$started} elapsed=$elapsed executable=${exe:-unknown} cmdline=$cmdline children=$children apt_lock_pids=$held_locks"
    fi
    case "$operation" in
      get-capabilities|get-update-policy|detect-repositories|get-operation-status|get-power-capabilities|get-poweroff-status)
        echo "Waiting briefly for the active maintenance operation to finish..."
        if wait_unit_inactive "$unit" "$WAIT_FOR_MAINTENANCE"; then continue; fi
        ;;
      validate-update-policy|run-update-dry-run)
        echo "A short maintenance validation is active."
        echo "Waiting up to $WAIT_FOR_MAINTENANCE seconds for it to finish..."
        systemctl stop "$unit" >/dev/null 2>&1 || true
        if wait_unit_inactive "$unit" "$WAIT_FOR_MAINTENANCE"; then continue; fi
        ;;
    esac
    BLOCKING_OPERATION="$operation"
    BLOCKING_UNIT="$unit"
    BLOCKING_PID="$pid"
    BLOCKING_REQUEST_ID="$status_request_id"
    BLOCKING_STARTED_AT="${status_started_at:-$started}"
    BLOCKING_ELAPSED="$elapsed"
    BLOCKING_CHILDREN="$children"
    BLOCKING_LOCKS="$held_locks"
    echo "Beszel Plus Agent upgrade postponed." >&2
    echo "" >&2
    echo "Active maintenance operation:" >&2
    echo "  Operation: $BLOCKING_OPERATION" >&2
    echo "  Unit: $BLOCKING_UNIT" >&2
    echo "  PID: ${BLOCKING_PID:-unknown}" >&2
    echo "  ExecMainPID: ${exec_pid:-unknown}" >&2
    echo "  Request ID: ${BLOCKING_REQUEST_ID:-unknown}" >&2
    echo "  Started: ${BLOCKING_STARTED_AT:-unknown}" >&2
    echo "  Elapsed: ${BLOCKING_ELAPSED:-unknown}" >&2
    echo "  Child PIDs: ${BLOCKING_CHILDREN:-none}" >&2
    echo "  APT lock holder PIDs: ${BLOCKING_LOCKS:-none}" >&2
    echo "" >&2
    echo "No binaries or configuration files were changed." >&2
    echo "Run the installer again after the operation finishes." >&2
    return 75
  done <"$PREFLIGHT_UNITS_FILE"
  rm -f "$PREFLIGHT_UNITS_FILE"
  return 0
}

restore_transaction_file() {
  transaction_path="$1"
  transaction_name="$2"
  transaction_changed="$3"
  [ "$transaction_changed" = "true" ] || return 0
  if [ -e "$TRANSACTION_DIR/$transaction_name" ]; then
    mkdir -p "$(dirname "$transaction_path")"
    rollback_new="${transaction_path}.rollback.$$"
    rm -f "$rollback_new"
    cp -p "$TRANSACTION_DIR/$transaction_name" "$rollback_new" && mv -f "$rollback_new" "$transaction_path"
  elif [ -e "$TRANSACTION_DIR/$transaction_name.missing" ]; then
    rm -f "$transaction_path"
  fi
}

rollback_install() {
  transaction_status="$1"
  [ "$TRANSACTION_ACTIVE" = "true" ] || return "$transaction_status"
  TRANSACTION_ACTIVE=false
  PHASE="PHASE_ROLLBACK"
  echo "Installation failed after the transaction started; restoring the previous Beszel Plus installation." >&2
  systemctl stop beszel-agent.service >/dev/null 2>&1 || true
  systemctl stop beszel-maintenance.socket >/dev/null 2>&1 || true
  systemctl list-units --all --no-legend 'beszel-maintenance@*.service' 2>/dev/null >"${TRANSACTION_DIR}/rollback-units" || true
  while IFS=' ' read -r rollback_unit _rest; do
    case "$rollback_unit" in beszel-maintenance@*.service) systemctl stop "$rollback_unit" >/dev/null 2>&1 || true ;; esac
  done <"${TRANSACTION_DIR}/rollback-units"
  for restore_spec in \
    "$BIN_PATH|agent|$AGENT_REPLACED|Agent" \
    "$MAINTENANCE_HELPER_PATH|helper|$HELPER_REPLACED|Maintenance helper" \
    "/etc/systemd/system/beszel-agent.service|agent-service|$AGENT_SERVICE_CHANGED|Agent service" \
    "/etc/systemd/system/beszel-maintenance.socket|maintenance-socket|$SOCKET_CHANGED|Maintenance socket" \
    "/etc/systemd/system/beszel-maintenance@.service|maintenance-template|$TEMPLATE_CHANGED|Maintenance template" \
    "/etc/systemd/system/beszel-maintenance-helper@.service|maintenance-legacy-template|$TEMPLATE_CHANGED|Legacy template" \
    "/etc/systemd/system/beszel-agent.service.d/update-monitoring.conf|update-monitoring|$MONITORING_DROPIN_CHANGED|Monitoring drop-in" \
    "/etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf|connection-settings|$CONNECTION_DROPIN_CHANGED|Connection drop-in" \
    "$AGENT_ENV_PATH|agent-env|$AGENT_ENV_CHANGED|Agent environment" \
    "/etc/systemd/system/beszel-agent-update.service|agent-update-service|$UPDATE_SERVICE_CHANGED|Update service" \
    "/etc/systemd/system/beszel-agent-update.timer|agent-update-timer|$UPDATE_TIMER_CHANGED|Update timer"
  do
    restore_path=$(printf '%s' "$restore_spec" | cut -d'|' -f1)
    restore_name=$(printf '%s' "$restore_spec" | cut -d'|' -f2)
    restore_changed=$(printf '%s' "$restore_spec" | cut -d'|' -f3)
    restore_label=$(printf '%s' "$restore_spec" | cut -d'|' -f4)
    [ "$restore_changed" = "true" ] || continue
    if restore_transaction_file "$restore_path" "$restore_name" "$restore_changed"; then
      echo "✓ $restore_label restored" >&2
      append_component restored "$restore_name"
    else
      echo "✗ $restore_label restoration failed" >&2
      append_component failed "$restore_name"
    fi
  done
  if systemctl daemon-reload >/dev/null 2>&1; then echo "✓ daemon-reload completed" >&2; else append_component failed daemon-reload; fi
  if restore_quiesced_services; then echo "✓ Previous service state restored" >&2; else append_component failed service-state; fi
  mkdir -p "$(dirname "$ROLLBACK_REPORT_PATH")"
  rollback_timestamp=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  rollback_restored_json=$(csv_json_array "$ROLLBACK_RESTORED_COMPONENTS")
  rollback_failed_json=$(csv_json_array "$ROLLBACK_FAILED_COMPONENTS")
  printf '{"timestamp":"%s","target_version":"%s","previous_version":"%s","restored_components":%s,"failed_components":%s,"final_service_state":{"agent":"%s","socket":"%s"}}\n' \
    "$rollback_timestamp" "${INSTALL_VERSION:-unknown}" "${OLD_AGENT_VERSION:-unknown}" "$rollback_restored_json" "$rollback_failed_json" "$(service_state beszel-agent.service)" "$(service_state beszel-maintenance.socket)" \
    >"$ROLLBACK_REPORT_PATH" 2>/dev/null || append_component failed rollback-report
  rm -f "${BIN_PATH}.new.$$" "${MAINTENANCE_HELPER_PATH}.new.$$" "${AGENT_ENV_PATH}.new.$$" \
    "/etc/systemd/system/beszel-agent.service.new.$$" \
    "/etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf.new.$$"
  if [ -n "$ROLLBACK_FAILED_COMPONENTS" ]; then
    echo "CRITICAL: Rollback was incomplete. Manual recovery is required." >&2
    return 70
  fi
  echo "Rollback completed and validated." >&2
  return "$transaction_status"
}

on_installer_exit() {
  exit_status="$1"
  trap - 0 INT TERM HUP
  if [ "$VERBOSE" = "true" ]; then echo "Installer exit from $PHASE with status $exit_status." >&2; fi
  if [ "$TRANSACTION_ACTIVE" = "true" ]; then
    rollback_install "$exit_status"
    rollback_status=$?
    if [ "$rollback_status" -eq 70 ]; then exit_status=70; fi
  elif [ "$QUIESCE_ACTIVE" = "true" ]; then
    restore_quiesced_services || exit_status=70
  fi
  restore_apt_timers || exit_status=70
  clear_upgrade_drain
  rm -f "${DRAIN_PATH}.new.$$"
  rm -f "${AGENT_ENV_PATH}.new.$$" "/etc/systemd/system/beszel-agent.service.new.$$" \
    "/etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf.new.$$"
  cleanup_temp_dir
  [ -z "${PREFLIGHT_UNITS_FILE:-}" ] || rm -f "$PREFLIGHT_UNITS_FILE"
  [ -z "$TRANSACTION_DIR" ] || rm -rf "$TRANSACTION_DIR"
  if [ "$LOCK_KIND" = "mkdir" ] && [ -n "$LOCK_DIR" ]; then rm -rf "$LOCK_DIR"; fi
  exit "$exit_status"
}

acquire_installer_lock() {
  mkdir -p /run/lock
  if command -v flock >/dev/null 2>&1; then
    eval "exec 9>\"$INSTALL_LOCK_PATH\""
    if ! flock -n 9; then echo "Another Beszel Plus Agent installation is already running." >&2; return 75; fi
    LOCK_KIND="flock"
    return 0
  fi
  LOCK_DIR="${INSTALL_LOCK_PATH}.d"
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n%s\n' "$$" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$LOCK_DIR/owner"
    LOCK_KIND="mkdir"
    return 0
  fi
  lock_pid=$(sed -n '1p' "$LOCK_DIR/owner" 2>/dev/null || true)
  case "$lock_pid" in ''|*[!0-9]*) lock_stale=true ;; *) if kill -0 "$lock_pid" 2>/dev/null; then lock_stale=false; else lock_stale=true; fi ;; esac
  if [ "$lock_stale" = "true" ]; then
    rm -rf "$LOCK_DIR"
    mkdir "$LOCK_DIR" 2>/dev/null && { printf '%s\n%s\n' "$$" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$LOCK_DIR/owner"; LOCK_KIND="mkdir"; return 0; }
  fi
  echo "Another Beszel Plus Agent installation is already running." >&2
  return 75
}

write_upgrade_drain() {
  mkdir -p "$(dirname "$DRAIN_PATH")"
  umask 022
  drain_new="${DRAIN_PATH}.new.$$"
  printf '{"pid":%s,"started_at":"%s","target_version":"%s"}\n' "$$" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "${INSTALL_VERSION:-unknown}" >"$drain_new" || return 1
  mv -f "$drain_new" "$DRAIN_PATH"
}

quiesce_for_upgrade() {
  PHASE="PHASE_QUIESCE"
  AGENT_PREVIOUS_STATE=$(service_state beszel-agent.service)
  SOCKET_PREVIOUS_STATE=$(service_state beszel-maintenance.socket)
  QUIESCE_ACTIVE=true
  write_upgrade_drain || return 1
  if [ "$AGENT_PREVIOUS_STATE" = "active" ] && ! systemctl stop beszel-agent.service; then
    echo "Upgrade not started: the Agent could not be stopped safely." >&2
    echo "No binaries or configuration files were changed." >&2
    return 75
  fi
  if [ "$SOCKET_PREVIOUS_STATE" = "active" ] && ! systemctl stop beszel-maintenance.socket; then
    echo "Upgrade not started: the maintenance socket could not be stopped safely." >&2
    echo "No binaries or configuration files were changed." >&2
    return 75
  fi
  maintenance_preflight
}

prepare_transaction_quiesce() {
  if [ "$EXISTING_INSTALLATION" = "true" ]; then
    quiesce_for_upgrade
    return $?
  fi
  PHASE="PHASE_QUIESCE"
  QUIESCE_ACTIVE=true
  AGENT_PREVIOUS_STATE="not-found"
  SOCKET_PREVIOUS_STATE="not-found"
  write_upgrade_drain || return 1
  maintenance_preflight
}

begin_install_transaction() {
  TRANSACTION_DIR=$(mktemp -d)
  backup_transaction_file "$BIN_PATH" agent || return 1
  backup_transaction_file "$MAINTENANCE_HELPER_PATH" helper || return 1
  backup_transaction_file /etc/systemd/system/beszel-agent.service agent-service || return 1
  backup_transaction_file /etc/systemd/system/beszel-maintenance.socket maintenance-socket || return 1
  backup_transaction_file /etc/systemd/system/beszel-maintenance@.service maintenance-template || return 1
  backup_transaction_file /etc/systemd/system/beszel-maintenance-helper@.service maintenance-legacy-template || return 1
  backup_transaction_file /etc/systemd/system/beszel-agent.service.d/update-monitoring.conf update-monitoring || return 1
  backup_transaction_file /etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf connection-settings || return 1
  backup_transaction_file "$AGENT_ENV_PATH" agent-env || return 1
  backup_transaction_file /etc/systemd/system/beszel-agent-update.service agent-update-service || return 1
  backup_transaction_file /etc/systemd/system/beszel-agent-update.timer agent-update-timer || return 1
  PHASE="PHASE_TRANSACTION"
  TRANSACTION_ACTIVE=true
}

install_agent_binary() {
  [ "$AGENT_INSTALLED" = "false" ] || return 0
  if [ -f "$BIN_PATH" ]; then echo "Backing up existing Agent..."; fi
  echo "Installing Agent v${INSTALL_VERSION}..."
  AGENT_NEW_PATH="${BIN_PATH}.new.$$"
  if ! install -m 0755 -o beszel -g beszel "$TEMP_DIR/beszel-agent" "$AGENT_NEW_PATH" ||
    ! { AGENT_REPLACED=true; mv -f "$AGENT_NEW_PATH" "$BIN_PATH"; } ||
    ! chown beszel:beszel "$BIN_PATH" ||
    ! chmod 755 "$BIN_PATH"; then
    echo "Failed to install Agent v${INSTALL_VERSION}." >&2
    return 1
  fi
  AGENT_REPLACED=true
  AGENT_INSTALLED=true
}

EXISTING_INSTALLATION=false
if [ -f "$BIN_PATH" ]; then EXISTING_INSTALLATION=true; fi
OLD_AGENT_VERSION="none"
if [ -x "$BIN_PATH" ]; then OLD_AGENT_VERSION=$($BIN_PATH --version 2>/dev/null | sed -n 's/^Beszel Plus Agent v//p' | head -n 1); [ -n "$OLD_AGENT_VERSION" ] || OLD_AGENT_VERSION="legacy"; fi
EXISTING_HELPER_VERSION="none"
EXISTING_HELPER_PROTOCOL="none"
if [ -x "$MAINTENANCE_HELPER_PATH" ]; then
  EXISTING_HELPER_OUTPUT=$($MAINTENANCE_HELPER_PATH --version 2>/dev/null || true)
  EXISTING_HELPER_VERSION=$(printf '%s\n' "$EXISTING_HELPER_OUTPUT" | sed -n -e 's/^Beszel Plus Maintenance Helper v//p' -e 's/^beszel-maintenance-helper v//p' | head -n 1)
  EXISTING_HELPER_PROTOCOL=$(printf '%s\n' "$EXISTING_HELPER_OUTPUT" | sed -n 's/^protocol //p' | head -n 1)
  [ -n "$EXISTING_HELPER_VERSION" ] || EXISTING_HELPER_VERSION="legacy"
  [ -n "$EXISTING_HELPER_PROTOCOL" ] || EXISTING_HELPER_PROTOCOL="unknown"
fi
if [ "$EXISTING_INSTALLATION" = "true" ] && [ "$EXISTING_HELPER_VERSION" != "none" ] && { [ "$OLD_AGENT_VERSION" != "$EXISTING_HELPER_VERSION" ] || [ "$EXISTING_HELPER_PROTOCOL" != "2" ]; }; then
  echo "Existing installation is inconsistent:"
  echo "Agent: v$OLD_AGENT_VERSION"
  echo "Maintenance Helper: v$EXISTING_HELPER_VERSION (protocol $EXISTING_HELPER_PROTOCOL)"
  echo "The installer will repair and validate both components transactionally."
fi
MAINTENANCE_CONFIGURED_BEFORE=false
if [ -e "$MAINTENANCE_HELPER_PATH" ] || [ -e /etc/systemd/system/beszel-maintenance.socket ] || [ -e /etc/systemd/system/beszel-maintenance@.service ] || [ -e /etc/systemd/system/beszel-maintenance-helper@.service ] || grep -qs 'OS_UPDATE_MANAGEMENT=true' /etc/systemd/system/beszel-agent.service.d/update-monitoring.conf; then
  MAINTENANCE_CONFIGURED_BEFORE=true
fi
MAINTENANCE_POLICY_EXISTED=false
if [ -f "$MAINTENANCE_POLICY_PATH" ]; then MAINTENANCE_POLICY_EXISTED=true; fi

read_existing_management_flag() {
  existing_name="$1"
  existing_value=$(sed -n "s/.*${existing_name}=\(true\|false\).*/\1/p" /etc/systemd/system/beszel-agent.service /etc/systemd/system/beszel-agent.service.d/*.conf 2>/dev/null | tail -n 1)
  [ -n "$existing_value" ] && echo "$existing_value" || echo "false"
}

read_existing_connection_value() {
  connection_name="$1"
  connection_value=$(sed -n "s/^${connection_name}=\"\(.*\)\"$/\1/p; s/^${connection_name}=\(.*\)$/\1/p; s/^Environment=\"${connection_name}=\(.*\)\"$/\1/p" \
    "$AGENT_ENV_PATH" /etc/systemd/system/beszel-agent.service /etc/systemd/system/beszel-agent.service.d/*.conf 2>/dev/null | tail -n 1)
  prefixed_value=$(sed -n "s/^BESZEL_AGENT_${connection_name}=\"\(.*\)\"$/\1/p; s/^BESZEL_AGENT_${connection_name}=\(.*\)$/\1/p; s/^Environment=\"BESZEL_AGENT_${connection_name}=\(.*\)\"$/\1/p" \
    "$AGENT_ENV_PATH" /etc/systemd/system/beszel-agent.service /etc/systemd/system/beszel-agent.service.d/*.conf 2>/dev/null | tail -n 1)
  [ -z "$prefixed_value" ] || connection_value="$prefixed_value"
  printf '%s' "$connection_value" | sed 's/\\"/"/g; s/\\\\/\\/g'
}

process_environment_value() {
  environment_pid="$1"
  environment_name="$2"
  tr '\000' '\n' <"/proc/$environment_pid/environ" 2>/dev/null | sed -n "s/^${environment_name}=//p" | tail -n 1
}

verify_agent_process_environment() {
  environment_pid=$(systemctl show beszel-agent.service -p MainPID --value 2>/dev/null || true)
  case "$environment_pid" in ''|0|*[!0-9]*)
    echo "Error: Beszel Plus Agent has no running process to validate." >&2
    return 1
    ;;
  esac
  if [ ! -r "/proc/$environment_pid/environ" ]; then
    echo "Error: Cannot inspect the effective Beszel Plus Agent environment." >&2
    return 1
  fi
  for environment_name in PORT KEY TOKEN HUB_URL; do
    case "$environment_name" in
      PORT) environment_expected="$PORT" ;;
      KEY) environment_expected="$KEY" ;;
      TOKEN) environment_expected="$TOKEN" ;;
      HUB_URL) environment_expected="$HUB_URL" ;;
    esac
    environment_actual=$(process_environment_value "$environment_pid" "$environment_name")
    environment_prefixed=$(process_environment_value "$environment_pid" "BESZEL_AGENT_$environment_name")
    if [ "$environment_actual" != "$environment_expected" ] || [ -n "$environment_prefixed" ]; then
      echo "Error: Effective Agent setting $environment_name does not match the protected installer configuration." >&2
      echo "No connection secret was printed. Remove conflicting systemd overrides and retry." >&2
      return 1
    fi
  done
  echo "Protected Agent connection settings verified in the running process."
}

agent_journal_after_restart() {
  if [ -n "$AGENT_JOURNAL_CURSOR" ]; then
    journalctl -u beszel-agent.service --after-cursor="$AGENT_JOURNAL_CURSOR" --no-pager 2>/dev/null || true
  else
    journalctl -u beszel-agent.service --since "$AGENT_JOURNAL_SINCE" --no-pager 2>/dev/null || true
  fi
}

verify_websocket_enrollment() {
  enrollment_waited=0
  while [ "$enrollment_waited" -lt 20 ]; do
    enrollment_log=$(agent_journal_after_restart)
    if printf '%s\n' "$enrollment_log" | grep -q 'WebSocket connected'; then
      echo "Hub WebSocket connection verified."
      return 0
    fi
    if printf '%s\n' "$enrollment_log" | grep -qE 'unexpected status code: 401|status code.? 401|Invalid token'; then
      ENROLLMENT_FAILURE_REASON="token_rejected"
      return 1
    fi
    if printf '%s\n' "$enrollment_log" | grep -qi 'invalid signature'; then
      ENROLLMENT_FAILURE_REASON="key_mismatch"
      return 1
    fi
    if printf '%s\n' "$enrollment_log" | grep -qi 'fingerprint mismatch'; then
      ENROLLMENT_FAILURE_REASON="fingerprint_mismatch"
      return 1
    fi
    if printf '%s\n' "$enrollment_log" | grep -qiE 'invalid hub URL|HUB_URL environment variable not set'; then
      ENROLLMENT_FAILURE_REASON="hub_url_invalid"
      return 1
    fi
    sleep 1
    enrollment_waited=$((enrollment_waited + 1))
  done
  enrollment_log=$(agent_journal_after_restart)
  if printf '%s\n' "$enrollment_log" | grep -qiE 'no such host|connection refused|network is unreachable|i/o timeout|dial tcp'; then
    ENROLLMENT_FAILURE_REASON="hub_unreachable"
  else
    ENROLLMENT_FAILURE_REASON="connection_timeout"
  fi
  return 1
}

report_websocket_failure() {
  case "$ENROLLMENT_FAILURE_REASON" in
    token_rejected) echo "Hub enrollment failed: TOKEN was rejected (401). Save the system/fingerprint in the Hub or generate a new token." >&2 ;;
    key_mismatch) echo "Hub enrollment failed: KEY does not match the Hub signing key." >&2 ;;
    fingerprint_mismatch) echo "Hub enrollment failed: this token is registered to another machine fingerprint." >&2 ;;
    hub_url_invalid) echo "Hub enrollment failed: HUB_URL is invalid." >&2 ;;
    hub_unreachable) echo "Hub enrollment failed: the configured Hub is unreachable from this host." >&2 ;;
    *) echo "Hub enrollment failed: a complete WebSocket handshake was not observed within 20 seconds." >&2 ;;
  esac
  echo "The Agent service remains installed and SSH fallback may still be available." >&2
  echo "No connection secret was printed." >&2
}

if [ "$EXISTING_INSTALLATION" = "true" ]; then
  if [ "$PORT_PROVIDED" = "false" ]; then PORT=$(read_existing_connection_value PORT); [ -n "$PORT" ] || PORT=45876; fi
  if [ "$KEY_PROVIDED" = "false" ]; then KEY=$(read_existing_connection_value KEY); fi
  if [ "$TOKEN_PROVIDED" = "false" ]; then TOKEN=$(read_existing_connection_value TOKEN); fi
  if [ "$HUB_URL_PROVIDED" = "false" ]; then HUB_URL=$(read_existing_connection_value HUB_URL); fi
fi
if [ -z "$AUTO_UPDATE_FLAG" ] && [ "$EXISTING_INSTALLATION" = "true" ]; then
  if systemctl is-enabled --quiet beszel-agent-update.timer 2>/dev/null; then AUTO_UPDATE_FLAG=true; else AUTO_UPDATE_FLAG=false; fi
  echo "Agent auto-update flag not provided; preserving existing setting: $AUTO_UPDATE_FLAG."
fi
if [ -z "$OS_UPDATE_MANAGEMENT_FLAG" ]; then
  if [ "$EXISTING_INSTALLATION" = "true" ]; then OS_UPDATE_MANAGEMENT_FLAG=$(read_existing_management_flag OS_UPDATE_MANAGEMENT); echo "OS update management flag not provided; preserving existing setting: $OS_UPDATE_MANAGEMENT_FLAG."; else OS_UPDATE_MANAGEMENT_FLAG=false; fi
fi
if [ -z "$POWER_MANAGEMENT_FLAG" ]; then
  if [ "$EXISTING_INSTALLATION" = "true" ]; then POWER_MANAGEMENT_FLAG=$(read_existing_management_flag POWER_MANAGEMENT); echo "Power management flag not provided; preserving existing setting: $POWER_MANAGEMENT_FLAG."; else POWER_MANAGEMENT_FLAG=false; fi
fi

diagnose_install() {
  diagnose_helper_version="not installed"
  diagnose_helper_protocol="unknown"
  if [ -x "$MAINTENANCE_HELPER_PATH" ]; then
    diagnose_output=$($MAINTENANCE_HELPER_PATH --version 2>/dev/null || true)
    diagnose_helper_version=$(printf '%s\n' "$diagnose_output" | sed -n -e 's/^Beszel Plus Maintenance Helper v//p' -e 's/^beszel-maintenance-helper v//p' | head -n 1)
    diagnose_helper_protocol=$(printf '%s\n' "$diagnose_output" | sed -n 's/^protocol //p' | head -n 1)
  fi
  diagnose_operation=$(json_string_value "$MAINTENANCE_STATUS_PATH" operation 2>/dev/null || true)
  diagnose_request=$(json_string_value "$MAINTENANCE_STATUS_PATH" request_id 2>/dev/null || true)
  diagnose_status=$(json_string_value "$MAINTENANCE_STATUS_PATH" status 2>/dev/null || true)
  if [ "$diagnose_status" != "running" ]; then diagnose_operation=""; diagnose_request=""; fi
  diagnose_pending="no"; [ -f /var/lib/beszel-maintenance/pending-policy.json ] && diagnose_pending="yes"
  diagnose_apt_required=false
  if [ "$POWER_MANAGEMENT_FLAG" = "true" ] && ! command -v ethtool >/dev/null 2>&1; then diagnose_apt_required=true; fi
  if [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] && ! dpkg-query -W -f='${db:Status-Status}' unattended-upgrades 2>/dev/null | grep -qx installed; then diagnose_apt_required=true; fi
  diagnose_apt="none"; if command -v fuser >/dev/null 2>&1; then diagnose_apt=$(fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock /var/cache/apt/archives/lock /run/unattended-upgrades.lock 2>/dev/null | tr ' ' '\n' | sed '/^$/d' | sort -u | tr '\n' ' '); [ -n "$diagnose_apt" ] || diagnose_apt="none"; fi
  diagnose_units_file=$(mktemp)
  systemctl list-units --all --no-legend 'beszel-maintenance@*.service' 2>/dev/null >"$diagnose_units_file" || true
  diagnose_unit=$(awk '$3 == "active" { print $1; exit }' "$diagnose_units_file")
  rm -f "$diagnose_units_file"
  if [ -z "$diagnose_operation" ] && [ -n "$diagnose_unit" ]; then diagnose_operation="unknown"; fi
  echo "Installed Agent version: $OLD_AGENT_VERSION"
  echo "Installed Helper version: ${diagnose_helper_version:-legacy}"
  echo "Maintenance protocol: $diagnose_helper_protocol"
  echo "Agent service state: $(service_state beszel-agent.service)"
  echo "Maintenance socket state: $(service_state beszel-maintenance.socket)"
  echo "Active maintenance operation: ${diagnose_operation:-none}"
  echo "Active maintenance unit: ${diagnose_unit:-none}"
  echo "Operation request ID: ${diagnose_request:-none}"
  echo "APT lock holder PID: $diagnose_apt"
  echo "APT package changes required: $diagnose_apt_required"
  echo "Pending policy: $diagnose_pending"
  echo "OS update management: $OS_UPDATE_MANAGEMENT_FLAG"
  echo "Power management: $POWER_MANAGEMENT_FLAG"
  if [ -n "$diagnose_operation" ] || { [ "$diagnose_apt_required" = "true" ] && [ "$diagnose_apt" != "none" ]; }; then echo "Upgrade can proceed: no"; else echo "Upgrade can proceed: yes"; fi
}
if [ "$DIAGNOSE" = "true" ]; then diagnose_install; exit 0; fi

acquire_installer_lock || exit $?
trap 'on_installer_exit $?' 0
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
PHASE="PHASE_PREFLIGHT"
available_kb=$(df -Pk /opt 2>/dev/null | awk 'NR==2 {print $4}')
case "$available_kb" in ''|*[!0-9]*) ;; *)
  if [ "$available_kb" -lt 65536 ]; then
    echo "Upgrade not started: at least 64 MiB of free space is required on /opt." >&2
    echo "No binaries or configuration files were changed." >&2
    exit 75
  fi
  ;;
esac
# Uninstall process
if [ "$UNINSTALL" = true ]; then
  # Clean up SELinux contexts before removing files
  cleanup_selinux_context

  if is_alpine; then
    echo "Stopping and disabling the agent service..."
    rc-service beszel-agent stop
    rc-update del beszel-agent default

    echo "Removing the OpenRC service files..."
    rm -f /etc/init.d/beszel-agent

    # Remove the daily update cron job if it exists
    echo "Removing the daily update cron job..."
    if crontab -u root -l 2>/dev/null | grep -q "beszel-agent.*update"; then
      crontab -u root -l 2>/dev/null | grep -v "beszel-agent.*update" | crontab -u root -
    fi

    # Remove log files
    echo "Removing log files..."
    rm -f /var/log/beszel-agent.log /var/log/beszel-agent.err
  elif is_openwrt; then
    echo "Stopping and disabling the agent service..."
    /etc/init.d/beszel-agent stop
    /etc/init.d/beszel-agent disable

    echo "Removing the OpenWRT service files..."
    rm -f /etc/init.d/beszel-agent

    # Remove the update service if it exists
    echo "Removing the daily update service..."
    # Remove legacy beszel account based crontab file
    rm -f /etc/crontabs/beszel
    # Install root crontab job
    if crontab -u root -l 2>/dev/null | grep -q "beszel-agent.*update"; then
      crontab -u root -l 2>/dev/null | grep -v "beszel-agent.*update" | crontab -u root -
    fi

  elif is_freebsd; then
    echo "Stopping and disabling the agent service..."
    service beszel-agent stop
    sysrc beszel_agent_enable="NO"

    echo "Removing the FreeBSD service files..."
    rm -f /usr/local/etc/rc.d/beszel-agent

    # Remove the daily update cron job if it exists
    echo "Removing the daily update cron job..."
    rm -f /etc/cron.d/beszel-agent

    # Remove log files
    echo "Removing log files..."
    rm -f /var/log/beszel-agent.log

    # Remove env file and directories
    echo "Removing environment configuration file..."
    rm -f "$AGENT_DIR/env"
    rm -f "$BIN_PATH"
    rmdir "$AGENT_DIR" 2>/dev/null || true

  else
    echo "Stopping and disabling the agent service..."
    systemctl stop beszel-agent.service
    systemctl disable beszel-agent.service >/dev/null 2>&1
		systemctl disable --now beszel-maintenance.socket 2>/dev/null || true

    echo "Removing the systemd service file..."
    rm /etc/systemd/system/beszel-agent.service

    # Remove the update timer and service if they exist
    echo "Removing the daily update service and timer..."
    systemctl stop beszel-agent-update.timer 2>/dev/null
    systemctl disable beszel-agent-update.timer >/dev/null 2>&1
    rm -f /etc/systemd/system/beszel-agent-update.service
    rm -f /etc/systemd/system/beszel-agent-update.timer
		rm -f /etc/systemd/system/beszel-maintenance.socket
		rm -f /etc/systemd/system/beszel-maintenance@.service
		rm -f /etc/systemd/system/beszel-maintenance-helper@.service
		rm -f /usr/local/libexec/beszel/maintenance-helper
		rmdir /usr/local/libexec/beszel 2>/dev/null || true
		rm -rf /var/lib/beszel-maintenance

    systemctl daemon-reload
  fi

  echo "Removing the Beszel Plus Agent directory..."
  rm -rf "$AGENT_DIR"

  echo "Removing the dedicated user for the agent service..."
  killall beszel-agent 2>/dev/null
  if is_alpine || is_openwrt; then
    deluser beszel 2>/dev/null
  elif is_freebsd; then
    pw user del beszel 2>/dev/null
  else
    userdel beszel 2>/dev/null
  fi

  echo "Beszel Plus Agent has been uninstalled successfully!"
  exit 0
fi

# Check if a package is installed
package_installed() {
  command -v "$1" >/dev/null 2>&1
}

# Bootstrap tools are preflight requirements. Installing them here would make
# release/download failures mutate the host before the Beszel transaction.
for required_tool in tar curl sha256sum; do
  if ! package_installed "$required_tool"; then
    echo "Upgrade not started: required tool '$required_tool' is missing." >&2
    echo "Install it with the operating-system package manager and run this command again." >&2
    exit 75
  fi
done

# If no SSH key is provided, ask for the SSH key interactively (skip if upgrading)
if [ -z "$KEY" ]; then
  if [ -t 0 ]; then
    printf "Enter your SSH key: "
    read -r KEY
  else
    echo "No SSH key is configured. Provide -k when running non-interactively." >&2
    exit 64
  fi
fi

# Remove newlines from KEY
KEY=$(echo "$KEY" | tr -d '\n')

case "$PORT" in ''|*[!0-9]*) echo "Invalid Agent port: $PORT" >&2; exit 64 ;; esac
if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then echo "Agent port must be between 1 and 65535." >&2; exit 64; fi
if printf '%s%s%s' "$KEY" "$TOKEN" "$HUB_URL" | grep -q '[[:cntrl:]]'; then
  echo "KEY, TOKEN and HUB_URL cannot contain control characters." >&2
  exit 64
fi
case "$HUB_URL" in ''|http://*|https://*) ;; *) echo "HUB_URL must use http:// or https://." >&2; exit 64 ;; esac

# TOKEN and HUB_URL are optional for backwards compatibility - no interactive prompts
# They will be set as empty environment variables if not provided

# Verify checksum
if command -v sha256sum >/dev/null; then
  CHECK_CMD="sha256sum"
elif command -v sha256 >/dev/null; then
  # FreeBSD uses 'sha256' instead of 'sha256sum', with different output format
  CHECK_CMD="sha256 -q"
else
  echo "No SHA256 checksum utility found"
  exit 1
fi

# Create a dedicated user for the service if it doesn't exist
AGENT_USER="beszel"
if [ "$EXISTING_INSTALLATION" != "true" ]; then
  echo "Configuring the dedicated user for the Beszel Plus Agent service..."
  if is_alpine; then
  if ! id -u beszel >/dev/null 2>&1; then
    addgroup beszel
    adduser -S -D -H -s /sbin/nologin -G beszel beszel
  fi
  # Add the user to the docker group to allow access to the Docker socket if group docker exists
  if getent group docker >/dev/null 2>&1; then
    echo "Adding beszel to docker group"
    addgroup beszel docker
  fi
  
  elif is_openwrt; then
  # Create beszel group first if it doesn't exist (check /etc/group directly)
  if ! grep -q "^beszel:" /etc/group >/dev/null 2>&1; then
    echo "beszel:x:999:" >> /etc/group
  fi
  
  # Create beszel user if it doesn't exist (double-check to prevent duplicates)
  if ! id -u beszel >/dev/null 2>&1 && ! grep -q "^beszel:" /etc/passwd >/dev/null 2>&1; then
    echo "beszel:x:999:999::/nonexistent:/bin/false" >> /etc/passwd
  fi
  
  # Add the user to the docker group if docker group exists and user is not already in it
  if grep -q "^docker:" /etc/group >/dev/null 2>&1; then
    echo "Adding beszel to docker group"
    # Check if beszel is already in docker group
    if ! grep "^docker:" /etc/group | grep -q "beszel"; then
      # Add beszel to docker group by modifying /etc/group
      # Handle both cases: group with existing members and group without members
      if grep "^docker:" /etc/group | grep -q ":.*:.*$"; then
        # Group has existing members, append with comma
        sed -i 's/^docker:\([^:]*:[^:]*:\)\(.*\)$/docker:\1\2,beszel/' /etc/group
      else
        # Group has no members, just append
        sed -i 's/^docker:\([^:]*:[^:]*:\)$/docker:\1beszel/' /etc/group
      fi
    fi
  fi

  elif is_freebsd; then
  if is_opnsense; then
    echo "OPNsense detected: skipping user creation (using daemon user instead)"
    AGENT_USER="daemon"
  else
    if ! id -u beszel >/dev/null 2>&1; then
      pw user add beszel -d /nonexistent -s /usr/sbin/nologin -c "beszel user"
    fi
    # Add the user to the wheel group to allow self-updates
    if pw group show wheel >/dev/null 2>&1; then
      echo "Adding beszel to wheel group for self-updates"
      pw group mod wheel -m beszel
    fi
  fi

  else
  if ! id -u beszel >/dev/null 2>&1; then
    useradd --system --home-dir /nonexistent --shell /bin/false beszel
  fi
  # Add the user to the docker group to allow access to the Docker socket if group docker exists
  if getent group docker >/dev/null 2>&1; then
    echo "Adding beszel to docker group"
    usermod -aG docker beszel
  fi
  # Add the user to the disk group to allow access to disk devices if group disk exists
  if getent group disk >/dev/null 2>&1; then
    echo "Adding beszel to disk group"
    usermod -aG disk beszel
  fi

  # Debian/Ubuntu expose APT logs to adm/systemd-journal. Membership is best-effort;
  # monitoring remains useful (but partial) when either group is unavailable.
  if grep -Eq '^ID=("?)(debian|ubuntu)\1$' /etc/os-release 2>/dev/null; then
    for log_group in adm systemd-journal; do
      if getent group "$log_group" >/dev/null 2>&1; then
        usermod -aG "$log_group" beszel || echo "Warning: could not add beszel to $log_group"
      fi
    done
  fi
  fi
fi

# Create the directory for the Beszel Plus Agent

if [ ! -d "$AGENT_DIR" ]; then
  echo "Creating the directory for the Beszel Plus Agent..."
  mkdir -p "$AGENT_DIR"
  chown "${AGENT_USER}:${AGENT_USER}" "$AGENT_DIR"
  chmod 755 "$AGENT_DIR"
fi

if [ ! -d "$BIN_DIR" ]; then
  mkdir -p "$BIN_DIR"
fi

# Download and install the Beszel Plus Agent

OS=$(uname -s | sed -e 'y/ABCDEFGHIJKLMNOPQRSTUVWXYZ/abcdefghijklmnopqrstuvwxyz/')
ARCH=$(detect_architecture)
FILE_NAME="beszel-agent_${OS}_${ARCH}.tar.gz"

# Determine version to install
if [ "$VERSION" = "latest" ]; then
  API_RELEASE_URL="https://api.github.com/repos/$REPOSITORY/releases/latest"
  if ! RELEASE_JSON=$(curl -fsSL "$API_RELEASE_URL"); then
    echo "Failed to get latest version"
    exit 1
  fi
  INSTALL_VERSION=$(printf '%s' "$RELEASE_JSON" | sed -n 's/.*"tag_name":[[:space:]]*"v\([^"]*\)".*/\1/p' | head -n 1)
  if [ -z "$INSTALL_VERSION" ]; then
    echo "Latest release metadata does not contain a valid version tag." >&2
    exit 1
  fi
else
  INSTALL_VERSION="$VERSION"
  # Remove 'v' prefix if present
  INSTALL_VERSION=$(echo "$INSTALL_VERSION" | sed 's/^v//')
fi
if ! printf '%s' "$INSTALL_VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'; then
  echo "Resolved release version is invalid: $INSTALL_VERSION" >&2
  exit 1
fi

echo "Downloading beszel-agent v${INSTALL_VERSION}..."

# Download checksums file
if ! TEMP_DIR=$(mktemp -d) || [ ! -d "$TEMP_DIR" ]; then
  echo "Failed to create a temporary download directory." >&2
  exit 1
fi
CHECKSUM_MANIFEST="$TEMP_DIR/beszel_${INSTALL_VERSION}_checksums.txt"
AGENT_ARCHIVE="$TEMP_DIR/$FILE_NAME"
AGENT_EXTRACTED="$TEMP_DIR/beszel-agent"
if ! curl -fsSL "$GITHUB_URL/$REPOSITORY/releases/download/v${INSTALL_VERSION}/beszel_${INSTALL_VERSION}_checksums.txt" -o "$CHECKSUM_MANIFEST"; then
  echo "Failed to download the checksum manifest." >&2
  exit 1
fi
CHECKSUM=$(awk -v file="$FILE_NAME" '$2 == file { print $1 }' "$CHECKSUM_MANIFEST")
if [ -z "$CHECKSUM" ] || ! echo "$CHECKSUM" | grep -qE "^[a-fA-F0-9]{64}$"; then
  echo "Failed to get checksum or invalid checksum format"
  echo "Try again with --mirror (or --mirror <url>) if GitHub is not reachable."
  exit 1
fi

if ! curl -fL# --retry 3 --retry-delay 2 --connect-timeout 10 "$GITHUB_URL/$REPOSITORY/releases/download/v${INSTALL_VERSION}/$FILE_NAME" -o "$AGENT_ARCHIVE"; then
  echo "Failed to download the agent from $GITHUB_URL/$REPOSITORY/releases/download/v${INSTALL_VERSION}/$FILE_NAME"
  echo "Try again with --mirror (or --mirror <url>) if GitHub is not reachable."
  exit 1
fi

if ! tar -tzf "$AGENT_ARCHIVE" >/dev/null 2>&1; then
  echo "Downloaded archive is invalid or incomplete (possible network/proxy issue)."
  echo "Try again with --mirror (or --mirror <url>) if the download path is unstable."
  exit 1
fi

if [ "$($CHECK_CMD "$AGENT_ARCHIVE" | cut -d' ' -f1)" != "$CHECKSUM" ]; then
  echo "Checksum verification failed: $($CHECK_CMD "$AGENT_ARCHIVE" | cut -d' ' -f1) & $CHECKSUM"
  exit 1
fi

if ! tar -xzf "$AGENT_ARCHIVE" -C "$TEMP_DIR" beszel-agent; then
  echo "Failed to extract the agent"
  exit 1
fi

if [ ! -s "$AGENT_EXTRACTED" ]; then
  echo "Downloaded binary is missing or empty."
  exit 1
fi
AGENT_VERSION_OUTPUT=$("$AGENT_EXTRACTED" --version 2>&1) || { echo "Downloaded Agent version check failed" >&2; exit 1; }
if ! printf '%s\n' "$AGENT_VERSION_OUTPUT" | grep -qx "Beszel Plus Agent v${INSTALL_VERSION}"; then
  echo "Downloaded Agent does not report Beszel Plus v${INSTALL_VERSION}." >&2
  exit 1
fi

AGENT_INSTALLED=false

APT_REQUIRED=false
NEED_ETHTOOL=false
NEED_UNATTENDED_UPGRADES=false
if [ "$POWER_MANAGEMENT_FLAG" = "true" ] && ! command -v ethtool >/dev/null 2>&1; then
  APT_REQUIRED=true
  NEED_ETHTOOL=true
fi
if [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] && ! dpkg-query -W -f='${db:Status-Status}' unattended-upgrades 2>/dev/null | grep -qx installed; then
  APT_REQUIRED=true
  NEED_UNATTENDED_UPGRADES=true
fi

# Install the privileged one-shot helper only on supported Linux systems when requested.
OS_UPDATE_SUPPORTED=false
if [ "$OS" = "linux" ] && grep -Eq '^ID=("?)(debian|ubuntu|raspbian)\1$' /etc/os-release 2>/dev/null; then OS_UPDATE_SUPPORTED=true; fi
if { [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] || [ "$POWER_MANAGEMENT_FLAG" = "true" ]; } && [ "$OS_UPDATE_SUPPORTED" = "true" ]; then
  HELPER_FILE_NAME="beszel-maintenance-helper_${OS}_${ARCH}.tar.gz"
  HELPER_ARCHIVE="$TEMP_DIR/$HELPER_FILE_NAME"
  HELPER_EXTRACTED="$TEMP_DIR/beszel-maintenance-helper"
  echo "Downloading maintenance helper v${INSTALL_VERSION}..."
  HELPER_CHECKSUM=$(awk -v file="$HELPER_FILE_NAME" '$2 == file { print $1 }' "$CHECKSUM_MANIFEST")
  if [ -z "$HELPER_CHECKSUM" ] || ! echo "$HELPER_CHECKSUM" | grep -qE '^[a-fA-F0-9]{64}$'; then echo "Invalid maintenance helper checksum" >&2; exit 1; fi
  if ! curl -fL# --retry 3 --retry-delay 2 --connect-timeout 10 "$GITHUB_URL/$REPOSITORY/releases/download/v${INSTALL_VERSION}/$HELPER_FILE_NAME" -o "$HELPER_ARCHIVE"; then echo "Failed to download maintenance helper" >&2; exit 1; fi
  if [ "$($CHECK_CMD "$HELPER_ARCHIVE" | cut -d' ' -f1)" != "$HELPER_CHECKSUM" ]; then echo "Maintenance helper checksum verification failed" >&2; exit 1; fi
  if ! tar -xzf "$HELPER_ARCHIVE" -C "$TEMP_DIR" beszel-maintenance-helper; then echo "Failed to extract maintenance helper" >&2; exit 1; fi
  chmod 0755 "$HELPER_EXTRACTED"
  HELPER_VERSION_OUTPUT=$("$HELPER_EXTRACTED" --version 2>&1) || { echo "New maintenance helper version check failed" >&2; exit 1; }
  if ! printf '%s\n' "$HELPER_VERSION_OUTPUT" | grep -qx "Beszel Plus Maintenance Helper v${INSTALL_VERSION}" || ! printf '%s\n' "$HELPER_VERSION_OUTPUT" | grep -qx 'protocol 2'; then
    echo "Maintenance helper version or protocol does not match Agent v${INSTALL_VERSION}." >&2
    exit 1
  fi
  if [ "$APT_REQUIRED" = "true" ]; then
    echo "Required package dependencies are missing; preparing bounded APT access."
    pause_apt_timers
    "$TEMP_DIR/beszel-agent" diagnose-install --apt-required=true --wait-for-apt="$WAIT_FOR_APT"
    apt_preflight_status=$?
    if [ "$apt_preflight_status" -ne 0 ]; then
      restore_apt_timers || true
      echo "Upgrade not started because APT/dpkg remained busy." >&2
      echo "No Beszel binaries or configuration files were changed." >&2
      exit "$apt_preflight_status"
    fi
    if ! apt-get -o DPkg::Lock::Timeout="$WAIT_FOR_APT" update; then
      restore_apt_timers || true
      echo "Failed to refresh APT metadata before the Beszel transaction started." >&2
      exit 1
    fi
    if [ "$NEED_ETHTOOL" = "true" ] && [ "$NEED_UNATTENDED_UPGRADES" = "true" ]; then
      DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout="$WAIT_FOR_APT" install -y ethtool unattended-upgrades || apt_dependency_status=$?
    elif [ "$NEED_ETHTOOL" = "true" ]; then
      DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout="$WAIT_FOR_APT" install -y ethtool || apt_dependency_status=$?
    else
      DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout="$WAIT_FOR_APT" install -y unattended-upgrades || apt_dependency_status=$?
    fi
    if [ "${apt_dependency_status:-0}" -ne 0 ]; then
      restore_apt_timers || true
      echo "Failed to install required package dependencies before the Beszel transaction started." >&2
      exit 1
    fi
    restore_apt_timers || { echo "Failed to restore APT timer state." >&2; exit 1; }
  else
    "$TEMP_DIR/beszel-agent" diagnose-install --apt-required=false --wait-for-apt=0 || true
  fi
  OLD_HELPER_VERSION="legacy"
  if [ -x "$MAINTENANCE_HELPER_PATH" ]; then
    echo "Checking existing maintenance helper version..."
    detect_existing_helper_version "$MAINTENANCE_HELPER_PATH"
    echo "Upgrading maintenance helper from ${OLD_HELPER_VERSION} to v${INSTALL_VERSION}..."
  fi
  prepare_transaction_quiesce || exit $?
  begin_install_transaction || exit 1
  if [ -e "$MAINTENANCE_HELPER_PATH" ]; then echo "Backing up existing maintenance helper..."; fi
  echo "Installing maintenance helper v${INSTALL_VERSION}..."
  mkdir -p "$(dirname "$MAINTENANCE_HELPER_PATH")"
  chown root:root "$(dirname "$MAINTENANCE_HELPER_PATH")"
  chmod 0755 "$(dirname "$MAINTENANCE_HELPER_PATH")"
  HELPER_NEW_PATH="${MAINTENANCE_HELPER_PATH}.new.$$"
  install -m 0755 -o root -g root "$TEMP_DIR/beszel-maintenance-helper" "$HELPER_NEW_PATH"
  HELPER_REPLACED=true
  if ! mv -f "$HELPER_NEW_PATH" "$MAINTENANCE_HELPER_PATH"; then
    echo "New maintenance helper smoke test failed." >&2
    exit 1
  fi
  INSTALLED_HELPER_OUTPUT=$("$MAINTENANCE_HELPER_PATH" --version 2>&1) || { echo "New maintenance helper smoke test failed." >&2; exit 1; }
  if ! printf '%s\n' "$INSTALLED_HELPER_OUTPUT" | grep -qx "Beszel Plus Maintenance Helper v${INSTALL_VERSION}" || ! printf '%s\n' "$INSTALLED_HELPER_OUTPUT" | grep -qx 'protocol 2'; then
    echo "Installed maintenance helper has an incompatible version or protocol." >&2
    exit 1
  fi
  echo "Maintenance helper protocol compatibility verified."
  chown root:root "$MAINTENANCE_HELPER_PATH"
  chmod 0755 "$MAINTENANCE_HELPER_PATH"
fi
if [ "$TRANSACTION_ACTIVE" != "true" ]; then
  prepare_transaction_quiesce || exit $?
  begin_install_transaction || exit 1
fi
install_agent_binary || exit 1
INSTALLED_AGENT_OUTPUT=$("$BIN_PATH" --version 2>&1) || { echo "Installed Agent version check failed." >&2; exit 1; }
if ! printf '%s\n' "$INSTALLED_AGENT_OUTPUT" | grep -qx "Beszel Plus Agent v${INSTALL_VERSION}"; then
  echo "Installed Agent version does not match v${INSTALL_VERSION}." >&2
  exit 1
fi
echo "Beszel Plus Agent v${INSTALL_VERSION} validated."
if [ "$HELPER_REPLACED" = "true" ] && [ "$HELPER_REMOVED" != "true" ]; then
  echo "Beszel Plus Maintenance Helper v${INSTALL_VERSION} validated."
  echo "Maintenance protocol 2 validated."
fi

# Set SELinux context if needed
set_selinux_context

# Cleanup
cleanup_temp_dir

# Make sure /etc/machine-id exists for persistent fingerprint
if [ ! -f /etc/machine-id ]; then
  cat /proc/sys/kernel/random/uuid | tr -d '-' > /etc/machine-id
fi

# Check for NVIDIA GPUs and grant device permissions for systemd service
detect_nvidia_devices() {
  devices=""
  for i in /dev/nvidia*; do
    if [ -e "$i" ]; then
      devices="${devices}DeviceAllow=$i rw\n"
    fi
  done
  echo "$devices"
}

# Modify service installation part, add Alpine check before systemd service creation
if is_alpine; then
  if [ ! -f /etc/init.d/beszel-agent ]; then
    echo "Creating OpenRC service for Alpine Linux..."
    cat >/etc/init.d/beszel-agent <<EOF
#!/sbin/openrc-run

name="beszel-agent"
description="Beszel Plus Agent Service"
command="$BIN_PATH"
command_user="beszel"
command_background="yes"
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="/var/log/beszel-agent.log"
error_log="/var/log/beszel-agent.err"

start_pre() {
    checkpath -f -m 0644 -o beszel:beszel "\$output_log" "\$error_log"
}

export PORT="$PORT"
export KEY="$KEY"
export TOKEN="$TOKEN"
export HUB_URL="$HUB_URL"

depend() {
    need net
    after firewall
}
EOF
    chmod +x /etc/init.d/beszel-agent
    rc-update add beszel-agent default
  else
    echo "Alpine OpenRC service file already exists. Skipping creation."
  fi

  # Create log files with proper permissions
  touch /var/log/beszel-agent.log /var/log/beszel-agent.err
  chown beszel:beszel /var/log/beszel-agent.log /var/log/beszel-agent.err

  # Start the service
  rc-service beszel-agent restart

  # Check if service started successfully
  sleep 2
  if ! rc-service beszel-agent status | grep -q "started"; then
    echo "Error: The Beszel Plus Agent service failed to start. Checking logs..."
    tail -n 20 /var/log/beszel-agent.err
    exit 1
  fi

  # Auto-update service for Alpine
  if [ "$AUTO_UPDATE_FLAG" = "true" ]; then
    AUTO_UPDATE="y"
  elif [ "$AUTO_UPDATE_FLAG" = "false" ]; then
    AUTO_UPDATE="n"
  else
    printf "\nEnable automatic daily updates for beszel-agent? (y/n): "
    read -r AUTO_UPDATE
  fi
  case "$AUTO_UPDATE" in
  [Yy]*)
    echo "Setting up daily automatic updates for beszel-agent..."

    # Create cron job to run beszel-agent update command daily at midnight
    if ! crontab -u root -l 2>/dev/null | grep -q "beszel-agent.*update"; then
      (crontab -u root -l 2>/dev/null; echo "12 0 * * * $BIN_PATH update >/dev/null 2>&1") | crontab -u root -
    fi

    printf "\nDaily updates have been enabled via cron job.\n"
    ;;
  esac

  # Check service status
  if ! rc-service beszel-agent status >/dev/null 2>&1; then
    echo "Error: The Beszel Plus Agent service is not running."
    rc-service beszel-agent status
    exit 1
  fi

elif is_openwrt; then
  if [ ! -f /etc/init.d/beszel-agent ]; then
    echo "Creating procd init script service for OpenWRT..."
    cat >/etc/init.d/beszel-agent <<EOF
#!/bin/sh /etc/rc.common

USE_PROCD=1
START=99

start_service() {
    procd_open_instance
    procd_set_param command $BIN_PATH
    procd_set_param user beszel
    procd_set_param pidfile /var/run/beszel-agent.pid
    procd_set_param env PORT="$PORT" KEY="$KEY" TOKEN="$TOKEN" HUB_URL="$HUB_URL"
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}

# Extra command to trigger agent update
EXTRA_COMMANDS="update restart"
EXTRA_HELP="        update          Update the Beszel agent
        restart         Restart the Beszel agent"

update() {
    $BIN_PATH update
}

EOF
    # Enable the service
    chmod +x /etc/init.d/beszel-agent
    /etc/init.d/beszel-agent enable
  else
    echo "OpenWRT init script already exists. Skipping creation."
  fi

  # Start the service
  /etc/init.d/beszel-agent restart

  # Auto-update service for OpenWRT using a crontab job
  if [ "$AUTO_UPDATE_FLAG" = "true" ]; then
    AUTO_UPDATE="y"
    sleep 1 # give time for the service to start
  elif [ "$AUTO_UPDATE_FLAG" = "false" ]; then
    AUTO_UPDATE="n"
    sleep 1 # give time for the service to start
  else
    printf "\nEnable automatic daily updates for beszel-agent? (y/n): "
    read -r AUTO_UPDATE
  fi
  case "$AUTO_UPDATE" in
  [Yy]*)
    echo "Setting up daily automatic updates for beszel-agent..."

    if ! crontab -u root -l 2>/dev/null | grep -q "beszel-agent.*update"; then
      (crontab -u root -l 2>/dev/null; echo "12 0 * * * /etc/init.d/beszel-agent update") | crontab -u root -
    fi

    /etc/init.d/cron restart

    printf "\nDaily updates have been enabled.\n"
    ;;
  esac

  # Check service status
  if ! /etc/init.d/beszel-agent running >/dev/null 2>&1; then
    echo "Error: The Beszel Plus Agent service is not running."
    /etc/init.d/beszel-agent status
    exit 1
  fi

elif is_freebsd; then
  echo "Checking for existing FreeBSD service configuration..."
  # Ensure rc.d directory exists on minimal FreeBSD installs
  mkdir -p /usr/local/etc/rc.d
  
  # Create environment configuration file with proper permissions if it doesn't exist
  if [ ! -f "$AGENT_DIR/env" ]; then
    echo "Creating environment configuration file..."
    cat >"$AGENT_DIR/env" <<EOF
LISTEN=$PORT
KEY="$KEY"
TOKEN=$TOKEN
HUB_URL=$HUB_URL
EOF
    chmod 640 "$AGENT_DIR/env"
    chown "root:${AGENT_USER}" "$AGENT_DIR/env"
  else
    echo "FreeBSD environment file already exists. Skipping creation."
  fi
  
  # Create the rc service file if it doesn't exist
  if [ ! -f /usr/local/etc/rc.d/beszel-agent ]; then
    echo "Creating FreeBSD rc service..."
    generate_freebsd_rc_service > /usr/local/etc/rc.d/beszel-agent
    # Set proper permissions for the rc script
    chmod 755 /usr/local/etc/rc.d/beszel-agent
  else
    echo "FreeBSD rc service file already exists. Skipping creation."
  fi

  # Enable and start the service
  echo "Enabling and starting the agent service..."
  sysrc beszel_agent_enable="YES"
  sysrc beszel_agent_user="${AGENT_USER}"
  service beszel-agent restart
  
  # Check if service started successfully
  sleep 2
  if ! service beszel-agent status | grep -q "is running"; then
    echo "Error: The Beszel Plus Agent service failed to start. Checking logs..."
    tail -n 20 /var/log/beszel_agent.log
    exit 1
  fi

  # Auto-update service for FreeBSD
  if [ "$AUTO_UPDATE_FLAG" = "true" ]; then
    AUTO_UPDATE="y"
  elif [ "$AUTO_UPDATE_FLAG" = "false" ]; then
    AUTO_UPDATE="n"
  else
    printf "\nEnable automatic daily updates for beszel-agent? (y/n): "
    read -r AUTO_UPDATE
  fi
  case "$AUTO_UPDATE" in
  [Yy]*)
    echo "Setting up daily automatic updates for beszel-agent..."

    # Create cron job in /etc/cron.d 
    cat >/etc/cron.d/beszel-agent <<EOF
# Beszel Plus Agent daily update job
12 0 * * * root $BIN_PATH update >/dev/null 2>&1
EOF
    chmod 644 /etc/cron.d/beszel-agent
    printf "\nDaily updates have been enabled via /etc/cron.d.\n"
    ;;
  esac

  # Check service status
  if ! service beszel-agent status >/dev/null 2>&1; then
    echo "Error: The Beszel Plus Agent service is not running."
    service beszel-agent status
    exit 1
  fi

else
  # Original systemd service installation code
  echo "Writing protected Agent connection settings..."
  mkdir -p "$(dirname "$AGENT_ENV_PATH")"
  chmod 0700 "$(dirname "$AGENT_ENV_PATH")"
  PORT_SYSTEMD=$(systemd_escape_environment "$PORT")
  KEY_SYSTEMD=$(systemd_escape_environment "$KEY")
  TOKEN_SYSTEMD=$(systemd_escape_environment "$TOKEN")
  HUB_URL_SYSTEMD=$(systemd_escape_environment "$HUB_URL")
  AGENT_ENV_NEW="${AGENT_ENV_PATH}.new.$$"
  AGENT_ENV_CHANGED=true
  cat >"$AGENT_ENV_NEW" <<EOF
PORT="$PORT_SYSTEMD"
KEY="$KEY_SYSTEMD"
TOKEN="$TOKEN_SYSTEMD"
HUB_URL="$HUB_URL_SYSTEMD"
EOF
  chown root:root "$AGENT_ENV_NEW"
  chmod 0600 "$AGENT_ENV_NEW"
  mv -f "$AGENT_ENV_NEW" "$AGENT_ENV_PATH"

  if [ ! -f /etc/systemd/system/beszel-agent.service ]; then
    echo "Creating the systemd service for the agent..."
    AGENT_SERVICE_CHANGED=true

    # Detect NVIDIA devices and grant device permissions
    NVIDIA_DEVICES=$(detect_nvidia_devices)

    cat >/etc/systemd/system/beszel-agent.service <<EOF
[Unit]
Description=Beszel Plus Agent Service
Wants=network-online.target
After=network-online.target

[Service]
Environment="UPDATE_MONITORING=true"
Environment="UPDATE_CHECK_INTERVAL=6h"
Environment="UPDATE_CHECK_TIMEOUT=30s"
Environment="UPDATE_MAX_PACKAGE_LIST=50"
Environment="UPDATE_APT_TIMEOUT=2m"
Environment="OS_UPDATE_MANAGEMENT=$OS_UPDATE_MANAGEMENT_FLAG"
Environment="POWER_MANAGEMENT=$POWER_MANAGEMENT_FLAG"
# Environment="EXTRA_FILESYSTEMS=sdb"
ExecStart=$BIN_PATH
User=beszel
Restart=on-failure
RestartSec=5
StateDirectory=beszel-agent

# Security/sandboxing settings
KeyringMode=private
LockPersonality=yes
ProtectClock=yes
ProtectHome=read-only
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectSystem=strict
RemoveIPC=yes
RestrictSUIDSGID=true

$(if [ -n "$NVIDIA_DEVICES" ]; then printf "%b" "# NVIDIA device permissions\n${NVIDIA_DEVICES}"; fi)

[Install]
WantedBy=multi-user.target
EOF
  else
    echo "Migrating existing systemd service away from inline connection secrets."
    AGENT_SERVICE_NEW="/etc/systemd/system/beszel-agent.service.new.$$"
    if sed '/^[[:space:]]*Environment="\?\(BESZEL_AGENT_\)\?\(PORT\|KEY\|TOKEN\|HUB_URL\)=/d' /etc/systemd/system/beszel-agent.service >"$AGENT_SERVICE_NEW"; then
      chmod --reference=/etc/systemd/system/beszel-agent.service "$AGENT_SERVICE_NEW" 2>/dev/null || chmod 0644 "$AGENT_SERVICE_NEW"
      chown --reference=/etc/systemd/system/beszel-agent.service "$AGENT_SERVICE_NEW" 2>/dev/null || chown root:root "$AGENT_SERVICE_NEW"
      AGENT_SERVICE_CHANGED=true
      mv -f "$AGENT_SERVICE_NEW" /etc/systemd/system/beszel-agent.service
    else
      rm -f "$AGENT_SERVICE_NEW"
      echo "Failed to migrate the existing Agent service." >&2
      exit 1
    fi
  fi

  # Keep monitoring enabled when upgrading an existing installation without
  # rewriting the user's service file.
  mkdir -p /etc/systemd/system/beszel-agent.service.d
  CONNECTION_DROPIN_CHANGED=true
  CONNECTION_DROPIN_NEW="/etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf.new.$$"
  cat >"$CONNECTION_DROPIN_NEW" <<EOF
[Service]
EnvironmentFile=-$AGENT_ENV_PATH
UnsetEnvironment=BESZEL_AGENT_PORT BESZEL_AGENT_KEY BESZEL_AGENT_TOKEN BESZEL_AGENT_HUB_URL
EOF
  chown root:root "$CONNECTION_DROPIN_NEW"
  chmod 0644 "$CONNECTION_DROPIN_NEW"
  mv -f "$CONNECTION_DROPIN_NEW" /etc/systemd/system/beszel-agent.service.d/10-beszel-connection.conf
  echo "Updated protected Agent connection settings."
  MONITORING_DROPIN_CHANGED=true
  cat >/etc/systemd/system/beszel-agent.service.d/update-monitoring.conf <<EOF
[Service]
Environment="UPDATE_MONITORING=true"
Environment="UPDATE_CHECK_INTERVAL=6h"
Environment="UPDATE_CHECK_TIMEOUT=30s"
Environment="UPDATE_MAX_PACKAGE_LIST=50"
Environment="UPDATE_APT_TIMEOUT=2m"
Environment="OS_UPDATE_MANAGEMENT=$OS_UPDATE_MANAGEMENT_FLAG"
Environment="POWER_MANAGEMENT=$POWER_MANAGEMENT_FLAG"
EOF

  if { [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] || [ "$POWER_MANAGEMENT_FLAG" = "true" ]; } && [ "$OS_UPDATE_SUPPORTED" = "true" ]; then
    SOCKET_CHANGED=true
    TEMPLATE_CHANGED=true
    cat >/etc/systemd/system/beszel-maintenance.socket <<EOF
[Unit]
Description=Beszel Plus maintenance helper socket
[Socket]
ListenStream=/run/beszel-maintenance.sock
SocketUser=root
SocketGroup=beszel
SocketMode=0660
Accept=yes
RemoveOnStop=yes
[Install]
WantedBy=sockets.target
EOF
    cat >/etc/systemd/system/beszel-maintenance@.service <<EOF
[Unit]
Description=Beszel Plus privileged maintenance helper
[Service]
Type=oneshot
TimeoutStartSec=3h
WorkingDirectory=/
ExecStart=$MAINTENANCE_HELPER_PATH
StandardInput=socket
StandardOutput=socket
StandardError=journal
User=root
Group=root
UMask=0077
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
EOF
    # Remove the incompatible template name created by Beszel Plus <= v0.0.3.
    rm -f /etc/systemd/system/beszel-maintenance-helper@.service
    if command -v systemd-analyze >/dev/null 2>&1 && ! systemd-analyze verify \
      /etc/systemd/system/beszel-maintenance.socket \
      /etc/systemd/system/beszel-maintenance@.service; then
      echo "Error: Maintenance systemd units failed validation." >&2
      exit 1
    fi
  else
    echo "OS update and power management are disabled; removing the privileged maintenance surface."
    systemctl disable --now beszel-maintenance.socket >/dev/null 2>&1 || true
    SOCKET_CHANGED=true
    TEMPLATE_CHANGED=true
    rm -f /etc/systemd/system/beszel-maintenance.socket \
      /etc/systemd/system/beszel-maintenance@.service \
      /etc/systemd/system/beszel-maintenance-helper@.service
    if [ -e "$MAINTENANCE_HELPER_PATH" ]; then
      HELPER_REPLACED=true
      HELPER_REMOVED=true
      rm -f "$MAINTENANCE_HELPER_PATH"
    fi
  fi

  # Load and start the service
  PHASE="PHASE_VALIDATE"
  printf "\nLoading and starting the agent service...\n"
  systemctl daemon-reload
  if { [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] || [ "$POWER_MANAGEMENT_FLAG" = "true" ]; } && [ "$OS_UPDATE_SUPPORTED" = "true" ]; then
    echo "Starting maintenance socket..."
    if ! systemctl enable beszel-maintenance.socket >/dev/null 2>&1 || ! systemctl restart beszel-maintenance.socket; then
      echo "Error: Failed to enable or restart beszel-maintenance.socket." >&2
      systemctl status beszel-maintenance.socket --no-pager 2>/dev/null || true
      exit 1
    fi
    if ! systemctl is-active --quiet beszel-maintenance.socket; then
      echo "Error: beszel-maintenance.socket is not active." >&2
      systemctl status beszel-maintenance.socket --no-pager 2>/dev/null || true
      exit 1
    fi
    systemctl reset-failed 'beszel-maintenance@*.service' 2>/dev/null || true
    echo "Running maintenance IPC smoke test..."
    if ! "$BIN_PATH" maintenance-smoke; then
      echo "Error: Maintenance IPC smoke test failed." >&2
      systemctl status 'beszel-maintenance@*.service' --no-pager 2>/dev/null || true
      exit 1
    fi
    if systemctl --failed --no-legend 2>/dev/null | grep -q 'beszel-maintenance@'; then
      echo "Error: Maintenance helper unit failed during smoke test." >&2
      exit 1
    fi
    echo "Maintenance IPC smoke test passed."
  fi
  if [ "$OS_UPDATE_MANAGEMENT_FLAG" = "true" ] && [ "$OS_UPDATE_SUPPORTED" = "true" ] && { [ "$EXISTING_INSTALLATION" = "false" ] || { [ "$MAINTENANCE_CONFIGURED_BEFORE" = "true" ] && [ "$MAINTENANCE_POLICY_EXISTED" = "false" ]; }; }; then
    echo "Applying initial update policy..."
    case "$OS_UPDATE_POLICY" in monitor) POLICY_MODE="monitor_only" ;; official-all) POLICY_MODE="official_all" ;; *) POLICY_MODE="security" ;; esac
    INSTALLER_REQUEST_ID="installer-$(date +%s)-$$"
    if ! POLICY_RESPONSE=$(printf '{"version":2,"request_id":"%s","operation":"apply-update-policy","idempotency_key":"%s","policy":{"enabled":true,"mode":"%s","update_package_lists_days":1,"unattended_upgrade_days":1,"automatic_reboot":false,"automatic_reboot_time":"04:00","remove_unused_dependencies":false}}\n' "$INSTALLER_REQUEST_ID" "$INSTALLER_REQUEST_ID" "$POLICY_MODE" | "$MAINTENANCE_HELPER_PATH"); then
      echo "Error: Initial OS update policy validation failed; maintenance setup is incomplete." >&2
      exit 1
    fi
    if ! printf '%s' "$POLICY_RESPONSE" | grep -q '"status":"completed"'; then
      if printf '%s' "$POLICY_RESPONSE" | grep -q '"retryable":true'; then
        POLICY_ERROR_CODE=$(printf '%s' "$POLICY_RESPONSE" | sed -n 's/.*"error_code":"\([^"]*\)".*/\1/p')
        POLICY_STAGE=$(printf '%s' "$POLICY_RESPONSE" | sed -n 's/.*"stage":"\([^"]*\)".*/\1/p')
        echo "Initial OS update policy could not be applied yet."
        echo "Stage: ${POLICY_STAGE:-unknown}"
        echo "Reason: ${POLICY_ERROR_CODE:-temporary maintenance condition}"
        echo "Retryable: yes"
        echo "The Agent and helper were installed successfully. The policy will be retried by the Agent."
        POLICY_PENDING=true
      else
        echo "Error: Initial OS update policy failed: $POLICY_RESPONSE" >&2
        exit 1
      fi
    fi
  fi
  echo "Starting Beszel Plus Agent..."
  AGENT_JOURNAL_CURSOR=$(journalctl -u beszel-agent.service -n 0 --show-cursor --no-pager 2>/dev/null | sed -n 's/^-- cursor: //p' | tail -n 1)
  AGENT_JOURNAL_SINCE=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
  systemctl enable beszel-agent.service >/dev/null 2>&1
  if ! systemctl restart beszel-agent.service; then
    echo "Error: Failed to restart the Beszel Plus Agent service." >&2
    exit 1
  fi



  # Prompt for auto-update setup
  if [ "$AUTO_UPDATE_FLAG" = "true" ]; then
    AUTO_UPDATE="y"
    sleep 1 # give time for the service to start
  elif [ "$AUTO_UPDATE_FLAG" = "false" ]; then
    AUTO_UPDATE="n"
    sleep 1 # give time for the service to start
  else
    printf "\nEnable automatic daily updates for beszel-agent? (y/n): "
    read -r AUTO_UPDATE
  fi
  case "$AUTO_UPDATE" in
  [Yy]*)
    echo "Setting up daily automatic updates for beszel-agent..."

    # Create systemd service for the daily update
    UPDATE_SERVICE_CHANGED=true
    cat >/etc/systemd/system/beszel-agent-update.service <<EOF
[Unit]
Description=Update beszel-agent if needed
Wants=beszel-agent.service

[Service]
Type=oneshot
ExecStart=$BIN_PATH update
EOF

    # Create systemd timer for the daily update
    UPDATE_TIMER_CHANGED=true
    cat >/etc/systemd/system/beszel-agent-update.timer <<EOF
[Unit]
Description=Run beszel-agent update daily

[Timer]
OnCalendar=daily
Persistent=true
RandomizedDelaySec=4h

[Install]
WantedBy=timers.target
EOF

    systemctl daemon-reload
    systemctl enable --now beszel-agent-update.timer >/dev/null 2>&1

    printf "\nDaily updates have been enabled.\n"
    ;;
  esac

  # Wait for the service to start or fail
  if ! systemctl is-active --quiet beszel-agent.service; then
    echo "Error: The Beszel Plus Agent service is not running."
    systemctl status beszel-agent.service --no-pager 2>/dev/null || true
    exit 1
  fi
  if ! verify_agent_process_environment; then
    exit 1
  fi
  if [ -n "$TOKEN" ] && [ -n "$HUB_URL" ]; then
    if ! verify_websocket_enrollment; then
      ENROLLMENT_FAILURE=true
      report_websocket_failure
    fi
  fi
fi

PHASE="PHASE_COMMIT"
TRANSACTION_ACTIVE=false
QUIESCE_ACTIVE=false
clear_upgrade_drain
rm -f "${DRAIN_PATH}.new.$$"
trap - 0 INT TERM HUP
[ -z "$TRANSACTION_DIR" ] || rm -rf "$TRANSACTION_DIR"
if [ "$LOCK_KIND" = "mkdir" ] && [ -n "$LOCK_DIR" ]; then rm -rf "$LOCK_DIR"; fi
PHASE="PHASE_COMPLETE"
if [ "$POLICY_PENDING" = "true" ]; then
  echo "Agent v${INSTALL_VERSION} installed."
  echo "Maintenance helper v${INSTALL_VERSION} installed."
  if [ "$POLICY_ERROR_CODE" = "apt_lock_busy" ]; then
    echo "APT/dpkg currently holds a real kernel lock."
  else
    echo "Initial policy validation is temporarily pending (${POLICY_ERROR_CODE:-unknown reason})."
  fi
  echo "The initial update policy was not applied yet."
  echo "The policy will be retried automatically by the Agent."
  echo "Installation completed with a pending maintenance policy."
fi
if [ "$ENROLLMENT_FAILURE" = "true" ]; then
  echo "Local installation completed, but Hub enrollment failed." >&2
  exit 69
fi
echo "Installation completed."
printf "\n\033[32mBeszel Plus Agent has been installed successfully! It is now running on %s.\033[0m\n" "$PORT"
