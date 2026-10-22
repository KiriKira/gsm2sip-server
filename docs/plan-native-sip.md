# 旧自研 SIP 计划入口

> **0.4.0 起**：服务器改名 SoulGem，SIP、Asterisk 与 ARI 已移除，通话只走 RTC 呼叫控制与 WebRTC。本文有关 SIP 的内容为历史记录；当前行为见 [rtc-call-control-v1.md](rtc-call-control-v1.md) 与 [release-0.4.md](release-0.4.md)。

状态：已被 [三端 WebRTC 通话迁移计划](plan-webrtc.md) 取代，更新于 2026-10-16。

后续直接将三端通话信令与媒体迁移到 WebRTC，不先实现自研 SIP/B2BUA/RTP 中继。
现有 Asterisk/SIP 实现保留为行为基线和迁移期回退路径，待新计划的兼容、真机、UI/外围回归及回退门槛通过后再移除。

迁移只改变通话传输实现；UI、短信、配对、SIM、备份、多节点和后台行为按新计划冻结。
旧文档的完整内容保留在 Git 历史；本文件保留用于兼容已有链接，不再作为实施清单。
