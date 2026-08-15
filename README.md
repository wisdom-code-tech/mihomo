# Mihomo for fnOS

原生运行的 fnOS FPK 应用，不依赖 Docker。包内包含 Mihomo、轻量管理服务和
Zashboard。Mihomo API 只监听 `127.0.0.1:9090`，外部访问统一经过 fnOS 登录网关。

## 构建

```bash
./scripts/build.sh
```

构建脚本会校验固定版本依赖的 SHA-256，交叉编译管理服务，检查 FPK 结构并调用
`fnpack build`。产物位于 `dist/mihomo.fpk`。

GitHub Actions 会在推送到 `main`、Pull Request、手动触发及 `v*` 标签时执行相同
构建。普通构建产物保留 30 天；推送版本标签时会同时创建 GitHub Release 并附加
FPK 与 SHA-256 文件。

## 数据位置

- `config.yaml`：`${TRIM_PKGVAR}/config.yaml`
- 上一份配置：`${TRIM_PKGVAR}/config.yaml.bak`
- 订阅地址：`${TRIM_PKGVAR}/subscription.url`（权限 `0600`）
- Mihomo 日志：`${TRIM_PKGVAR}/mihomo.log`
- 管理服务日志：`${TRIM_PKGVAR}/manager.log`

订阅更新采用“HTTPS 下载 → 强制安全控制字段 → mihomo 校验 → 原子替换 → 重启”流程。
