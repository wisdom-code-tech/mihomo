#!/bin/bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
PACKAGE_DIR="${ROOT_DIR}/packaging/mihomo"

required=(
  manifest config/privilege config/resource ICON.PNG ICON_256.PNG
  app/ui/config app/ui/images/icon_64.png app/ui/images/icon_256.png
  app/bin/mihomo app/bin/mihomo-manager app/defaults/config.yaml
  cmd/main cmd/install_init cmd/install_callback cmd/uninstall_init
  cmd/uninstall_callback cmd/upgrade_init cmd/upgrade_callback
  cmd/config_init cmd/config_callback wizard/install wizard/uninstall
  wizard/upgrade wizard/config
)

for path in "${required[@]}"; do
  if [ ! -e "${PACKAGE_DIR}/${path}" ]; then
    printf '缺少必需文件: %s\n' "${path}" >&2
    exit 1
  fi
done

jq empty "${PACKAGE_DIR}/config/privilege" "${PACKAGE_DIR}/config/resource" \
  "${PACKAGE_DIR}/app/ui/config" "${PACKAGE_DIR}/wizard/install" \
  "${PACKAGE_DIR}/wizard/uninstall" "${PACKAGE_DIR}/wizard/upgrade" \
  "${PACKAGE_DIR}/wizard/config"

for script in "${PACKAGE_DIR}"/cmd/*; do
  bash -n "${script}"
done

file "${PACKAGE_DIR}/app/bin/mihomo" | grep -q 'x86-64'
file "${PACKAGE_DIR}/app/bin/mihomo-manager" | grep -q 'x86-64'

file "${PACKAGE_DIR}/ICON.PNG" | grep -Eq 'PNG image data, 64 x 64'
file "${PACKAGE_DIR}/ICON_256.PNG" | grep -Eq 'PNG image data, 256 x 256'

printf 'FPK 结构与静态检查通过。\n'
