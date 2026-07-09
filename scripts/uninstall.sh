#!/bin/bash

BIN_NAME="polaris"
INSTALL_DIR="$HOME/.polarisagi/polaris/bin"
PLIST_LABEL="com.polarisagi.polaris"
PLIST_PATH="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"

if [[ "$LANG" == *"zh"* ]] || [[ "$LC_ALL" == *"zh"* ]] || [[ "$LANGUAGE" == *"zh"* ]]; then
    LANG_ZH=true
else
    LANG_ZH=false
fi

msg() {
    if [ "$LANG_ZH" = true ]; then echo "$1"; else echo "$2"; fi
}

msg "🗑️  正在卸载 PolarisAGI Polaris..." "🗑️  Uninstalling PolarisAGI Polaris..."

OS=$(uname -s | tr '[:upper:]' '[:lower:]')

# ── 1. 停止并移除系统服务 ─────────────────────────────────────────────────────
if [ "$OS" = "darwin" ]; then
    if [ -f "$PLIST_PATH" ]; then
        msg "⚙️  卸载 macOS launchd 服务..." "⚙️  Unloading macOS launchd service..."
        launchctl unload "$PLIST_PATH" 2>/dev/null || true
        rm -f "$PLIST_PATH"
    fi
    pkill -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true

elif [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
    SYSTEMD_USER_DIR="$HOME/.config/systemd/user"
    SVC_FILE="$SYSTEMD_USER_DIR/${BIN_NAME}.service"

    if systemctl --user is-active --quiet "$BIN_NAME" 2>/dev/null; then
        msg "⚙️  停止 systemd 用户服务..." "⚙️  Stopping systemd user service..."
        systemctl --user stop "$BIN_NAME" || true
    fi
    if systemctl --user is-enabled --quiet "$BIN_NAME" 2>/dev/null; then
        msg "⚙️  禁用 systemd 用户服务..." "⚙️  Disabling systemd user service..."
        systemctl --user disable "$BIN_NAME" || true
    fi
    if [ -f "$SVC_FILE" ]; then
        msg "⚙️  删除 systemd 服务配置..." "⚙️  Removing systemd service file..."
        rm -f "$SVC_FILE"
        systemctl --user daemon-reload
    fi
fi

# ── 2. 删除二进制 ─────────────────────────────────────────────────────────────
if [ -f "${INSTALL_DIR}/${BIN_NAME}" ]; then
    msg "🗑️  删除二进制: ${INSTALL_DIR}/${BIN_NAME}" \
        "🗑️  Removing binary: ${INSTALL_DIR}/${BIN_NAME}"
    rm -f "${INSTALL_DIR}/${BIN_NAME}"
fi

echo ""
msg "⚠️  数据目录 ~/.polarisagi/polaris 已保留（含数据库、配置、模型）。" \
    "⚠️  Data directory ~/.polarisagi/polaris has been kept (DB, configs, models)."
msg "    彻底清除所有数据请手动执行:" \
    "    To fully remove all data, run manually:"
echo "    rm -rf ~/.polarisagi/polaris"
echo ""
msg "✅ 卸载完成！" "✅ Uninstallation complete!"
