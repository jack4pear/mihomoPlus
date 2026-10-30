# Smart Fallback v1

基于 Mihomo v1.19.33，上游提交
3f27ef39f8c96bbfbc8b0b9bb10f5c9ef60149d8。
功能默认关闭，不改变普通 fallback 的优先级算法。

## 行为

正常节点每次完成的 provider 健康检查失败记一次。统计窗口为
(完成时间 - window, 完成时间]；成功不清除窗口内失败。
达到阈值后进入冷却，依次采用 1800、3600、7200 秒，最高等级封顶。

冷却期间照常探测，但不累计恢复次数。冷却结束后，只计算开始时间不早于恢复阶段的探测；
连续三次成功才恢复业务资格。恢复探测失败立即升级冷却。
高优先级节点恢复资格后，后续连接可自动回切；已有连接不迁移。

恢复成功后连续一小时探测成功，冷却等级归零。
相邻有效探测开始时间超过“检查周期 + 最大探测超时”，视为探测中断，重新计算一小时稳定时间。
恢复成功样本的间隔不得小于检查周期；短时间额外探测不能加速恢复；跳过或缺失的探测不算成功，实际失败会中断连续恢复次数。
手动测速不计入熔断或恢复统计。真实业务错误仅保留上游触发健康检查的行为。

所有节点都在冷却时，提前验证截止时间最早的一个；并列按配置顺序。
提前验证也必须三次成功。失败后的下一次提前验证至少间隔一个检查周期。
无合格节点时返回 smart-fallback: no eligible healthy proxy。
状态接口的 now 此时为 SMART-FALLBACK-UNAVAILABLE，不是可配置节点。
真正没有候选节点时保留上游 empty-fallback。

手动选择和启动恢复选择不能绕过熔断。状态按组独立，重载或重启后清空。
同名节点若被 provider 替换为新实例，不继承旧实例的惩罚；旧实例迟到结果被忽略。

## 配置

参见 smart-fallback-example.yaml。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| enabled | false | 启用增强功能 |
| window | 600 | 滑动窗口秒数 |
| failure-threshold | 3 | 窗口失败次数 |
| cooldown | [1800, 3600, 7200] | 冷却秒数，非空、正整数、非递减 |
| recover-success | 3 | 连续恢复成功次数 |
| reset-after | 3600 | 重置等级所需连续健康秒数 |

数值字段必须是正整数，拒绝溢出、字符串和小数。未知字段（包括 sticky）报错。
组和关联 provider 必须使用 lazy: false。
provider 必须启用持续健康检查；检查周期必须大于超时。
同组 provider 的检查周期必须相同，测试 URL 与 expected-status 必须匹配实际检查任务。
推荐周期 60 秒、超时 3000 毫秒。使用 use 时应在 provider 中显式设置这些参数；
组的配置不会强行覆盖 provider 已有的检查语义。

HTTP 状态码不符合 expected-status 也算失败，即使上游 URLTest 返回 nil 错误。
HTTP 探测不能证明 UDP/QUIC 业务可用；本功能只统一限制 TCP/UDP 新连接的节点资格。

## 构建与验证

在 WSL ~/codes/mihomoPlus 内运行：

    bash scripts/build-smart-fallback.sh
    CGO_ENABLED=1 mise exec go@1.26.10-mihomo-v1.19.33 -- go test -race ./adapter ./adapter/outboundgroup ./adapter/provider ./tunnel
    mise exec go@1.26.10-mihomo-v1.19.33 -- go test -timeout 90s ./...

构建脚本使用正式版附带的 MetaCubeX Go 1.26.10 工具链，校验发布摘要后通过 mise 管理。
采用上游 with_gvisor、trimpath 和版本链接参数；amd64 使用 v1 指令集提高兼容性。
产物位于 dist/，包括 amd64、arm64 二进制、SHA256SUMS 和 BUILD-INFO.txt。
arm64 交叉编译不等于设备实机验收。

本版没有自动同步、自动发布、路由器安装或新 HTTP API。
关闭 enabled 即恢复普通 fallback 行为。更换为官方内核前删除新增配置段。
