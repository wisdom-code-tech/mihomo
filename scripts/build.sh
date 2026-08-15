#!/bin/bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
PACKAGE_DIR="${ROOT_DIR}/packaging/mihomo"
CACHE_DIR="${ROOT_DIR}/.cache"
DIST_DIR="${ROOT_DIR}/dist"

MIHOMO_VERSION="1.19.29"
MIHOMO_FILE="mihomo-linux-amd64-v1-go123-v${MIHOMO_VERSION}.gz"
MIHOMO_SHA256="169ef2f65e914ef8ecfb4d340ffcc41894265d3389c2812996ac5cdd3dde8199"
MIHOMO_URL="https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/${MIHOMO_FILE}"
ZASHBOARD_VERSION="3.19.0"
ZASHBOARD_FILE="zashboard-dist-no-fonts-v${ZASHBOARD_VERSION}.zip"
ZASHBOARD_SHA256="d6e1c34771885ceb642f92c75864951bf3a35d682eafd8e088521da17aab7375"
ZASHBOARD_URL="https://github.com/Zephyruso/zashboard/releases/download/v${ZASHBOARD_VERSION}/dist-no-fonts.zip"

mkdir -p "${CACHE_DIR}" "${DIST_DIR}" "${PACKAGE_DIR}/app/bin" \
  "${PACKAGE_DIR}/app/zashboard" "${PACKAGE_DIR}/app/licenses"

download_verified() {
  url=$1
  output=$2
  expected=$3
  if [ ! -f "${output}" ]; then
    curl --fail --location --retry 3 --output "${output}.part" "${url}"
    mv "${output}.part" "${output}"
  fi
  if command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "${output}" | awk '{print $1}')
  else
    actual=$(sha256sum "${output}" | awk '{print $1}')
  fi
  if [ "${actual}" != "${expected}" ]; then
    printf 'SHA-256 校验失败: %s\n期望: %s\n实际: %s\n' "${output}" "${expected}" "${actual}" >&2
    return 1
  fi
}

download_verified "${MIHOMO_URL}" "${CACHE_DIR}/${MIHOMO_FILE}" "${MIHOMO_SHA256}"
download_verified "${ZASHBOARD_URL}" "${CACHE_DIR}/${ZASHBOARD_FILE}" "${ZASHBOARD_SHA256}"

gzip -dc "${CACHE_DIR}/${MIHOMO_FILE}" > "${PACKAGE_DIR}/app/bin/mihomo"
chmod 755 "${PACKAGE_DIR}/app/bin/mihomo"

rm -rf "${PACKAGE_DIR}/app/zashboard" "${PACKAGE_DIR}/app/.zashboard-stage"
mkdir -p "${PACKAGE_DIR}/app/.zashboard-stage"
unzip -q "${CACHE_DIR}/${ZASHBOARD_FILE}" -d "${PACKAGE_DIR}/app/.zashboard-stage"
if [ -d "${PACKAGE_DIR}/app/.zashboard-stage/dist" ]; then
  mv "${PACKAGE_DIR}/app/.zashboard-stage/dist" "${PACKAGE_DIR}/app/zashboard"
  rmdir "${PACKAGE_DIR}/app/.zashboard-stage"
else
  mv "${PACKAGE_DIR}/app/.zashboard-stage" "${PACKAGE_DIR}/app/zashboard"
fi

(
  cd "${ROOT_DIR}/manager"
  go mod download
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
    go build -trimpath -ldflags='-s -w' -o "${PACKAGE_DIR}/app/bin/mihomo-manager" .
)

find "${PACKAGE_DIR}/cmd" -type f -exec chmod 755 {} +
find "${PACKAGE_DIR}/app/bin" -type f -exec chmod 755 {} +

"${ROOT_DIR}/scripts/validate.sh"

if ! command -v fnpack >/dev/null 2>&1; then
  printf '未找到 fnpack。请安装 fnpack 1.2.1 后重新运行。\n' >&2
  exit 1
fi

rm -f "${PACKAGE_DIR}/mihomo.fpk"
(
  cd "${PACKAGE_DIR}"
  fnpack build
)
mv "${PACKAGE_DIR}/mihomo.fpk" "${DIST_DIR}/mihomo.fpk"
printf '构建完成: %s\n' "${DIST_DIR}/mihomo.fpk"
