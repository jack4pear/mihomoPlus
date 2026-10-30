# 验证记录

日期：2026-10-29（Asia/Taipei）
工作目录：/home/edenz/codes/mihomoPlus
分支：codex/smart-fallback
上游：v1.19.33 / 3f27ef39f8c96bbfbc8b0b9bb10f5c9ef60149d8

## 通过的检查

- 最终全量测试：go test -timeout 90s ./...，退出码 0。
- 受影响包竞态测试：CGO_ENABLED=1 go test -race ./adapter ./adapter/outboundgroup ./adapter/provider ./tunnel，退出码 0。
- 新增 19 个测试函数及配置子用例，覆盖窗口边界、重复/重叠事件、冷却升级封顶、恢复次数、调度抖动、稳定重置、应急验证、实例替换、组间隔离、手动选择、订阅注销、重载、关闭功能兼容和 TCP/UDP 不可用错误。
- 示例配置：mihomo-smart-linux-amd64 -t，退出码 0。
- 完整二进制故障注入：scripts/smoke-smart-fallback.py，退出码 0。
- git diff --check，通过。

## 二进制故障注入

测试在 WSL 内启动两个可控本地 SOCKS5 节点和本地 HTTP 服务，不依赖机场节点或公网探测站点。
使用缩短后的测试参数（2 秒周期、2 次失败、4/6/8 秒冷却、3 次恢复成功）。

已验证：

1. 初始优先选择 A，实际 HTTP 代理业务成功。
2. A 故障并熔断，B 承载实际业务。
3. A 连续成功验证后恢复优先级，实际业务成功。
4. A、B 都故障时，选择结果为不可用，业务无法绕过熔断。
5. B 经应急恢复验证重新承载实际业务。
6. 测试退出后终止内核，关闭临时本地服务。

默认 10 分钟窗口、30/60/120 分钟冷却和一小时稳定重置由假时钟测试覆盖，不需要在验收中真实等待数小时。
原始日志见交付目录 validation/。

## 构建与限制

使用上游正式版附带的 MetaCubeX Go 1.26.10，工具链归档 SHA256 已校验，通过 mise 调用。
Linux amd64 使用 GOAMD64=v1；Linux arm64 为交叉编译。两个文件均为静态链接 ELF，带 with_gvisor。
版本为 v1.19.33-smart.1，详细构建参数见 BUILD-INFO.txt。
本记录对应首次本地交付，源码快照及校验清单随当时交付包提供。后续提交与 GitHub CI 结果以仓库历史和 Actions 页面为准。

尚未做 OpenWrt、OpenClash 或 Nikki 实机验收。
HTTP 健康检查不验证 UDP/QUIC 业务通路；已有会话不自动迁移。
重启或重载会清空熔断历史，这是本版设计。
