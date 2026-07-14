#!/bin/sh

PRODUCT_NAME="Beszel Plus"
PRODUCT_VERSION="0.2.2"
REPOSITORY="guzassis/beszel-plus"
GITHUB_URL="https://github.com"
PORT=8090
PORT_PROVIDED=false
AUTO_UPDATE_FLAG=""
POWER_MANAGEMENT_FLAG=""
POWER_INTERFACE=""
POWER_INTERFACE_PROVIDED=false
UNINSTALL=false

HUB_DIR="/opt/beszel"
BIN_PATH="$HUB_DIR/beszel"
SERVICE_PATH="/etc/systemd/system/beszel-hub.service"
UPDATE_SERVICE_PATH="/etc/systemd/system/beszel-hub-update.service"
UPDATE_TIMER_PATH="/etc/systemd/system/beszel-hub-update.timer"
LOCK_PATH="/run/lock/beszel-plus-hub-install.lock"

TRANSACTION_ACTIVE=false
TRANSACTION_DIR=""
TEMP_DIR=""
LOCK_KIND=""
LOCK_DIR=""
HUB_PREVIOUS_STATE="not-found"
HUB_PREVIOUS_ENABLED=false
UPDATE_TIMER_PREVIOUS_ENABLED=false
UPDATE_TIMER_PREVIOUS_ACTIVE=false
QUIESCE_ACTIVE=false
BIN_CHANGED=false
SERVICE_CHANGED=false
UPDATE_SERVICE_CHANGED=false
UPDATE_TIMER_CHANGED=false
USER_CREATED=false
HUB_DIR_CREATED=false

usage() {
  printf "%s Hub installer v%s\n\n" "$PRODUCT_NAME" "$PRODUCT_VERSION"
  printf "Usage: %s [options]\n\n" "$0"
  printf "  -u                         Uninstall the Hub\n"
  printf "  -p PORT                    HTTP port (default: 8090)\n"
  printf "  -c, --mirror [URL]         Use GitHub mirror/proxy\n"
  printf "  --auto-update[=true|false] Control automatic Hub updates\n"
  printf "  --power-management=true|false\n"
  printf "  --power-interface NAME     Prefer a local power interface\n"
  printf "  -h, --help                 Show this help\n"
}

ensure_trailing_slash() { case "$1" in */) printf '%s' "$1" ;; *) printf '%s/' "$1" ;; esac; }
systemd_escape_environment() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

if [ "$(id -u)" != "0" ]; then
  if command -v sudo >/dev/null 2>&1; then exec sudo -- "$0" "$@"; fi
  echo "This installer must run as root or through sudo." >&2
  exit 77
fi

while [ "$#" -gt 0 ]; do
  case "$1" in
    -u) UNINSTALL=true ;;
    -h|--help) usage; exit 0 ;;
    -p) [ "$#" -ge 2 ] || { echo "Missing value for -p" >&2; exit 64; }; PORT="$2"; PORT_PROVIDED=true; shift ;;
    -c|--mirror)
      if [ "$#" -ge 2 ] && [ -n "$2" ] && ! printf '%s' "$2" | grep -q '^-'; then GITHUB_URL="$(ensure_trailing_slash "$2")https://github.com"; shift; else GITHUB_URL="https://gh.beszel.dev"; fi
      ;;
    --auto-update) AUTO_UPDATE_FLAG=true ;;
    --auto-update=*) AUTO_UPDATE_FLAG=${1#*=}; case "$AUTO_UPDATE_FLAG" in true|false) ;; *) echo "Invalid --auto-update value" >&2; exit 64 ;; esac ;;
    --power-management=*) POWER_MANAGEMENT_FLAG=${1#*=}; case "$POWER_MANAGEMENT_FLAG" in true|false) ;; *) echo "Invalid --power-management value" >&2; exit 64 ;; esac ;;
    --power-interface) [ "$#" -ge 2 ] || { echo "Missing --power-interface value" >&2; exit 64; }; POWER_INTERFACE="$2"; POWER_INTERFACE_PROVIDED=true; shift ;;
    *) echo "Invalid option: $1" >&2; exit 64 ;;
  esac
  shift
done

if [ "$(uname -s)" != "Linux" ] || ! grep -Eq '^ID=("?)(debian|ubuntu|raspbian)\1$' /etc/os-release 2>/dev/null; then
  echo "$PRODUCT_NAME v$PRODUCT_VERSION supports only Debian, Ubuntu, or Raspbian Linux." >&2
  exit 1
fi
case "$(uname -m)" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac

case "$PORT" in ''|*[!0-9]*) echo "Invalid Hub port: $PORT" >&2; exit 64 ;; esac
if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then echo "Hub port must be between 1 and 65535." >&2; exit 64; fi
case "$POWER_INTERFACE" in *[!A-Za-z0-9_.:-]*) echo "Invalid power interface." >&2; exit 64 ;; esac

service_state() {
  [ "$(systemctl show beszel-hub.service -p LoadState --value 2>/dev/null)" != "not-found" ] || { echo not-found; return; }
  systemctl show beszel-hub.service -p ActiveState --value 2>/dev/null || echo unknown
}

read_existing_value() {
  name="$1"
  sed -n "s/^Environment=\"${name}=\(.*\)\"$/\1/p" "$SERVICE_PATH" 2>/dev/null | tail -n 1 | sed 's/\\"/"/g; s/\\\\/\\/g'
}

if [ -f "$SERVICE_PATH" ]; then
  if [ "$PORT_PROVIDED" = "false" ]; then existing_port=$(sed -n 's/.*--http "0\.0\.0\.0:\([0-9][0-9]*\)".*/\1/p' "$SERVICE_PATH" | tail -n 1); [ -n "$existing_port" ] && PORT="$existing_port"; fi
  if [ -z "$POWER_MANAGEMENT_FLAG" ]; then POWER_MANAGEMENT_FLAG=$(read_existing_value POWER_MANAGEMENT); [ -n "$POWER_MANAGEMENT_FLAG" ] || POWER_MANAGEMENT_FLAG=true; fi
  if [ "$POWER_INTERFACE_PROVIDED" = "false" ]; then POWER_INTERFACE=$(read_existing_value POWER_INTERFACE); fi
  if [ -z "$AUTO_UPDATE_FLAG" ]; then if systemctl is-enabled --quiet beszel-hub-update.timer 2>/dev/null; then AUTO_UPDATE_FLAG=true; else AUTO_UPDATE_FLAG=false; fi; fi
else
  [ -n "$POWER_MANAGEMENT_FLAG" ] || POWER_MANAGEMENT_FLAG=true
  [ -n "$AUTO_UPDATE_FLAG" ] || AUTO_UPDATE_FLAG=false
fi

acquire_lock() {
  mkdir -p /run/lock
  if command -v flock >/dev/null 2>&1; then
    exec 9>"$LOCK_PATH"
    flock -n 9 || { echo "Another Beszel Plus Hub installation is already running." >&2; return 75; }
    LOCK_KIND=flock
    return 0
  fi
  LOCK_DIR="${LOCK_PATH}.d"
  if mkdir "$LOCK_DIR" 2>/dev/null; then printf '%s\n' "$$" >"$LOCK_DIR/pid"; LOCK_KIND="mkdir"; return 0; fi
  lock_pid=$(sed -n '1p' "$LOCK_DIR/pid" 2>/dev/null || true)
  if [ -n "$lock_pid" ] && ! kill -0 "$lock_pid" 2>/dev/null; then rm -rf "$LOCK_DIR"; mkdir "$LOCK_DIR" && { printf '%s\n' "$$" >"$LOCK_DIR/pid"; LOCK_KIND="mkdir"; return 0; }; fi
  echo "Another Beszel Plus Hub installation is already running." >&2
  return 75
}

backup_file() { path="$1"; name="$2"; if [ -e "$path" ]; then cp -p "$path" "$TRANSACTION_DIR/$name"; else : >"$TRANSACTION_DIR/$name.missing"; fi; }
restore_file() {
  path="$1"; name="$2"; changed="$3"; [ "$changed" = true ] || return 0
  if [ -e "$TRANSACTION_DIR/$name" ]; then mkdir -p "$(dirname "$path")"; cp -p "$TRANSACTION_DIR/$name" "${path}.rollback.$$" && mv -f "${path}.rollback.$$" "$path"
  else rm -f "$path"; fi
}

restore_service_state() {
  systemctl disable --now beszel-hub-update.timer >/dev/null 2>&1 || true
  if [ "$UPDATE_TIMER_PREVIOUS_ENABLED" = true ]; then systemctl enable beszel-hub-update.timer >/dev/null 2>&1 || return 1; fi
  if [ "$UPDATE_TIMER_PREVIOUS_ACTIVE" = true ]; then systemctl start beszel-hub-update.timer >/dev/null 2>&1 || return 1; fi
  if [ "$HUB_PREVIOUS_ENABLED" = true ]; then systemctl enable beszel-hub.service >/dev/null 2>&1 || return 1; else systemctl disable beszel-hub.service >/dev/null 2>&1 || true; fi
  if [ "$HUB_PREVIOUS_STATE" = active ]; then systemctl start beszel-hub.service >/dev/null 2>&1 || return 1; fi
  QUIESCE_ACTIVE=false
}

rollback() {
  status="$1"; TRANSACTION_ACTIVE=false
  echo "Hub installation failed after the transaction started; restoring the previous installation." >&2
  systemctl stop beszel-hub.service >/dev/null 2>&1 || true
  failed=false
  restore_file "$BIN_PATH" hub "$BIN_CHANGED" || failed=true
  restore_file "$SERVICE_PATH" service "$SERVICE_CHANGED" || failed=true
  restore_file "$UPDATE_SERVICE_PATH" update-service "$UPDATE_SERVICE_CHANGED" || failed=true
  restore_file "$UPDATE_TIMER_PATH" update-timer "$UPDATE_TIMER_CHANGED" || failed=true
  systemctl daemon-reload >/dev/null 2>&1 || failed=true
  restore_service_state || failed=true
  if [ "$USER_CREATED" = true ]; then userdel beszel >/dev/null 2>&1 || failed=true; fi
  if [ "$HUB_DIR_CREATED" = true ]; then rm -rf "$HUB_DIR" || failed=true; fi
  if [ "$failed" = true ]; then echo "CRITICAL: Hub rollback was incomplete." >&2; return 70; fi
  echo "Hub rollback completed and validated." >&2
  return "$status"
}

on_exit() {
  status="$1"; trap - 0 INT TERM HUP
  if [ "$TRANSACTION_ACTIVE" = true ]; then rollback "$status"; rollback_status=$?; [ "$rollback_status" -eq 70 ] && status=70
  elif [ "$QUIESCE_ACTIVE" = true ]; then restore_service_state || status=70; fi
  [ -z "$TEMP_DIR" ] || rm -rf "$TEMP_DIR"
  [ -z "$TRANSACTION_DIR" ] || rm -rf "$TRANSACTION_DIR"
  rm -f "${BIN_PATH}.new.$$" "${SERVICE_PATH}.new.$$" "${UPDATE_SERVICE_PATH}.new.$$" "${UPDATE_TIMER_PATH}.new.$$"
  if [ "$LOCK_KIND" = mkdir ] && [ -n "$LOCK_DIR" ]; then rm -rf "$LOCK_DIR"; fi
  exit "$status"
}

acquire_lock || exit $?
trap 'on_exit $?' 0
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ "$UNINSTALL" = true ]; then
  systemctl disable --now beszel-hub.service beszel-hub-update.timer >/dev/null 2>&1 || true
  rm -f "$SERVICE_PATH" "$UPDATE_SERVICE_PATH" "$UPDATE_TIMER_PATH"
  systemctl daemon-reload
  rm -rf "$HUB_DIR"
  userdel beszel >/dev/null 2>&1 || true
  trap - 0 INT TERM HUP
  if [ "$LOCK_KIND" = mkdir ]; then rm -rf "$LOCK_DIR"; fi
  echo "$PRODUCT_NAME Hub uninstalled successfully."
  exit 0
fi

for tool in curl tar sha256sum; do command -v "$tool" >/dev/null 2>&1 || { echo "Required tool '$tool' is missing; install it and retry." >&2; exit 75; }; done

release_json=$(curl -fsSL "https://api.github.com/repos/$REPOSITORY/releases/latest") || { echo "Failed to resolve latest release." >&2; exit 1; }
INSTALL_VERSION=$(printf '%s' "$release_json" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n 1)
printf '%s' "$INSTALL_VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' || { echo "Invalid resolved release version." >&2; exit 1; }

FILE_NAME="beszel_linux_${ARCH}.tar.gz"
TEMP_DIR=$(mktemp -d)
CHECKSUM_FILE="$TEMP_DIR/beszel_${INSTALL_VERSION}_checksums.txt"
ARCHIVE_PATH="$TEMP_DIR/$FILE_NAME"
BASE_URL="$GITHUB_URL/$REPOSITORY/releases/download/v${INSTALL_VERSION}"
curl -fsSL "$BASE_URL/beszel_${INSTALL_VERSION}_checksums.txt" -o "$CHECKSUM_FILE" || { echo "Failed to download checksum manifest." >&2; exit 1; }
EXPECTED=$(awk -v file="$FILE_NAME" '$2 == file { print $1 }' "$CHECKSUM_FILE")
printf '%s' "$EXPECTED" | grep -qE '^[a-fA-F0-9]{64}$' || { echo "Checksum entry is missing or invalid." >&2; exit 1; }
curl -fL# --retry 3 --retry-delay 2 --connect-timeout 10 "$BASE_URL/$FILE_NAME" -o "$ARCHIVE_PATH" || { echo "Failed to download Hub archive." >&2; exit 1; }
[ "$(sha256sum "$ARCHIVE_PATH" | awk '{print $1}')" = "$EXPECTED" ] || { echo "Hub checksum verification failed." >&2; exit 1; }
tar -tzf "$ARCHIVE_PATH" >/dev/null 2>&1 || { echo "Hub archive is invalid." >&2; exit 1; }
tar -xzf "$ARCHIVE_PATH" -C "$TEMP_DIR" beszel || { echo "Failed to extract Hub." >&2; exit 1; }
[ -f "$TEMP_DIR/beszel" ] && [ ! -L "$TEMP_DIR/beszel" ] || { echo "Extracted Hub is not a regular file." >&2; exit 1; }
chmod 0755 "$TEMP_DIR/beszel"
"$TEMP_DIR/beszel" --version | grep -qx "Beszel Plus Hub v${INSTALL_VERSION}" || { echo "Downloaded Hub version does not match v${INSTALL_VERSION}." >&2; exit 1; }

HUB_PREVIOUS_STATE=$(service_state)
if systemctl is-enabled --quiet beszel-hub.service 2>/dev/null; then HUB_PREVIOUS_ENABLED=true; fi
if systemctl is-enabled --quiet beszel-hub-update.timer 2>/dev/null; then UPDATE_TIMER_PREVIOUS_ENABLED=true; fi
if systemctl is-active --quiet beszel-hub-update.timer 2>/dev/null; then UPDATE_TIMER_PREVIOUS_ACTIVE=true; fi
QUIESCE_ACTIVE=true
if [ "$HUB_PREVIOUS_STATE" = active ]; then systemctl stop beszel-hub.service || { echo "Hub could not be stopped safely." >&2; exit 75; }; fi

TRANSACTION_DIR=$(mktemp -d)
backup_file "$BIN_PATH" hub
backup_file "$SERVICE_PATH" service
backup_file "$UPDATE_SERVICE_PATH" update-service
backup_file "$UPDATE_TIMER_PATH" update-timer
TRANSACTION_ACTIVE=true

if ! id -u beszel >/dev/null 2>&1; then useradd --system --home-dir /nonexistent --shell /bin/false beszel; USER_CREATED=true; fi
if [ ! -d "$HUB_DIR" ]; then mkdir -p "$HUB_DIR"; HUB_DIR_CREATED=true; fi
mkdir -p "$HUB_DIR/beszel_data"
chown -R beszel:beszel "$HUB_DIR"
chmod 0755 "$HUB_DIR"

install -m 0755 -o beszel -g beszel "$TEMP_DIR/beszel" "${BIN_PATH}.new.$$"
BIN_CHANGED=true
mv -f "${BIN_PATH}.new.$$" "$BIN_PATH"

POWER_MANAGEMENT_SYSTEMD=$(systemd_escape_environment "$POWER_MANAGEMENT_FLAG")
POWER_INTERFACE_SYSTEMD=$(systemd_escape_environment "$POWER_INTERFACE")
cat >"${SERVICE_PATH}.new.$$" <<EOF
[Unit]
Description=Beszel Plus Hub Service
After=network.target

[Service]
Environment="POWER_MANAGEMENT=$POWER_MANAGEMENT_SYSTEMD"
Environment="POWER_INTERFACE=$POWER_INTERFACE_SYSTEMD"
ExecStart=$BIN_PATH serve --http "0.0.0.0:$PORT"
WorkingDirectory=$HUB_DIR
User=beszel
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${SERVICE_PATH}.new.$$"; chown root:root "${SERVICE_PATH}.new.$$"; SERVICE_CHANGED=true; mv -f "${SERVICE_PATH}.new.$$" "$SERVICE_PATH"

if [ "$AUTO_UPDATE_FLAG" = true ]; then
  cat >"${UPDATE_SERVICE_PATH}.new.$$" <<EOF
[Unit]
Description=Update Beszel Plus Hub if needed
Wants=beszel-hub.service
[Service]
Type=oneshot
ExecStart=$BIN_PATH update
WorkingDirectory=$HUB_DIR
User=beszel
EOF
  chmod 0644 "${UPDATE_SERVICE_PATH}.new.$$"; chown root:root "${UPDATE_SERVICE_PATH}.new.$$"; UPDATE_SERVICE_CHANGED=true; mv -f "${UPDATE_SERVICE_PATH}.new.$$" "$UPDATE_SERVICE_PATH"
  cat >"${UPDATE_TIMER_PATH}.new.$$" <<EOF
[Unit]
Description=Run Beszel Plus Hub update daily
[Timer]
OnCalendar=daily
Persistent=true
RandomizedDelaySec=4h
[Install]
WantedBy=timers.target
EOF
  chmod 0644 "${UPDATE_TIMER_PATH}.new.$$"; chown root:root "${UPDATE_TIMER_PATH}.new.$$"; UPDATE_TIMER_CHANGED=true; mv -f "${UPDATE_TIMER_PATH}.new.$$" "$UPDATE_TIMER_PATH"
else
  systemctl disable --now beszel-hub-update.timer >/dev/null 2>&1 || true
  UPDATE_SERVICE_CHANGED=true; UPDATE_TIMER_CHANGED=true
  rm -f "$UPDATE_SERVICE_PATH" "$UPDATE_TIMER_PATH"
fi

systemctl daemon-reload
if [ "$AUTO_UPDATE_FLAG" = true ]; then
  systemd-analyze verify "$SERVICE_PATH" "$UPDATE_SERVICE_PATH" "$UPDATE_TIMER_PATH" >/dev/null 2>&1 || { echo "Hub systemd units failed validation." >&2; exit 1; }
else
  systemd-analyze verify "$SERVICE_PATH" >/dev/null 2>&1 || { echo "Hub systemd unit failed validation." >&2; exit 1; }
fi
systemctl enable beszel-hub.service >/dev/null 2>&1
systemctl restart beszel-hub.service
systemctl is-active --quiet beszel-hub.service || { echo "Hub service failed to start." >&2; exit 1; }
if [ "$AUTO_UPDATE_FLAG" = true ]; then systemctl enable --now beszel-hub-update.timer >/dev/null 2>&1 || { echo "Hub update timer failed to start." >&2; exit 1; }; fi
"$BIN_PATH" --version | grep -qx "Beszel Plus Hub v${INSTALL_VERSION}" || { echo "Installed Hub version validation failed." >&2; exit 1; }

TRANSACTION_ACTIVE=false
QUIESCE_ACTIVE=false
trap - 0 INT TERM HUP
rm -rf "$TEMP_DIR" "$TRANSACTION_DIR"
if [ "$LOCK_KIND" = mkdir ]; then rm -rf "$LOCK_DIR"; fi
printf '\n\033[32m%s Hub v%s installed successfully on port %s.\033[0m\n' "$PRODUCT_NAME" "$INSTALL_VERSION" "$PORT"
