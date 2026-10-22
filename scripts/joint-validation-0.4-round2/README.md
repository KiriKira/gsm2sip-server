# GSM 0.4 joint-test round two helpers

This folder preserves the lab scripts used for the final-head continuation of
the 0.4 joint test. The authoritative acceptance cases are in
[`docs/plan-0.4-joint-test.md`](../../docs/plan-0.4-joint-test.md), especially
“Second-round supplemental validation”; upgrade and rollback order are in
[`docs/release-0.4.md`](../../docs/release-0.4.md). The detailed helper
workflow is in [`ROUND2-HELPERS.md`](ROUND2-HELPERS.md).

## Handoff state (2026-10-22)

Testing is stopped at the user's request. No test, capture, or logcat worker
was running at handoff. The Gateway API 36 emulator (`emulator-5554`) and Host
API 29 emulator (`emulator-5556`) remain booted. The final-head APKs were
installed in place with `adb install -r`; the install evidence records retained
fixtures/data. Do not interpret the interrupted capture as a complete R3 run.

The exact build manifest and current JSON evidence are attached to GitHub
Release `validation-0.4-20261022` as
`final-head-partial-evidence-20261022T0535Z.zip` and its JSON index. The ZIP
contains 13 checked files, including APK/build hashes and the final audit; it
contains no APKs, raw logs, private directory, database, or packet captures.
The report at
[`docs/release-0.4-validation-20261022.md`](../../docs/release-0.4-validation-20261022.md)
keeps the earlier round's results intact and records the final-head continuation
separately.

The latest source/build identities are:

| Component | Source used by final build | Notes |
| --- | --- | --- |
| SoulGem Server | code `c6e6013c44cb8d01df586bd53e98e1da55e8f157`; branch head `b8575f65438f7f4bdc5e44d12b39dac75c943692` | The later branch commits changed validation documentation only; the running 0.4 image was built from the code commit above. |
| Homura Gateway | `15e9ac7e3ad8a7168ba51ccaeda6b00325a32b0d` | Final 0.4 candidate APK. |
| Madoka Host | `f2680de748381fd09cc23d84c99d723b6ffb903a` | Includes the preceding stale-removed-SIM fix `264a0b2a18f10a6ee0b4197ab4bf117811ac6343` and the node-probe/refresh-isolation fix. |

Both Android apps use version code `1000000000`, the same local debug
keystore (SHA-256 `8aae81ba21787afc6cb041492e72ee6dbddb5fcd719b9eccce4898162efdce33`),
and signing certificate SHA-256
`2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`. The
installed APK hashes are recorded in the evidence manifest. The Host fix has
16 focused regression tests passing (including the new isolation cases) and
main/test APK builds passing; no test timeout or product threshold was
increased.

## Continue the plan

The final-head continuation is **R1 PASS**, **R2 partial** (the partial-node
case passed; the full idle-failover helper remains FAIL because it recorded
`Broken pipe`), **R3 INCOMPLETE**, and **R4/R5 not rerun on these final heads**.
R3 must be repeated as one fresh capture window that covers R1, R2, fresh SMS,
one restart of each app, R5 screenshots, and at least 15 minutes idle. Use a
new unique run ID and capture phase; the interrupted run already used and
closed phase `r3-all-candidate-final`, and its evidence files must not be
overwritten. Re-run R4 only after R3 is complete, following Server → Gateway →
Host rollback and forward order in `docs/release-0.4.md`. Preserve all prior
FAIL evidence and stop at the first new failure.

On the lab host, the final APKs are already present at
`/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/builds/candidate-final-f2680de`
and the previous-build APKs at
`/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/builds/baseline`. The
latest source helpers were staged at
`/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/scripts-15e9ac7-followup`.
To prepare a new isolated run, first transfer this folder's `.py` files to a
fresh helper-source directory on the lab, then run:

```sh
python3 /path/to/helper-source/round2-prepare.py \
  --lab-root /home/ubuntu/gsm-webrtc-validation/joint-0.4 \
  --run-id final-head-resume-UNIQUE_UTC_ID \
  --baseline-builds /home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/builds/baseline \
  --candidate-builds /home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/builds/candidate-final-f2680de
```

Review its plan-only output first. Add `--execute` only when intentionally
creating the new isolated run directory. Then follow the exact sequence and
stop conditions in `ROUND2-HELPERS.md`; do not use the interrupted run's
evidence directory for new results. Keep the simulators warm and do not
uninstall or clear app data.

The helper set is lab-specific and contains topology addresses and paths, but
no private keys, environment files, access tokens, or live SMS credentials.
Runtime secrets remain in the lab's private directory and are excluded from
GitHub and evidence packages.

## 续测接手记录（2026-10-22 13:30Z）

上一节之后由续测代理在最终 head 上继续，未重新构建或覆盖既有证据：

- R2 `idle-failover-hold-failback` 的 `Broken pipe` 已定位为完成记录边界错误并修复（先落盘再输出、容忍 EPIPE、失败分支不再改写已完成结果），最终 head 上两次独立重跑 **PASS**（实际 Telecom 各 1 通）。回归与证据见报告新节。
- R3 两次新窗口（`r3-all-candidate-resume-20261022T1220Z`、`r3-all-candidate-resume2-20261022T1240Z`）都在 R2 阶段中断：第一次是故障后拨号早于网关在 B 上线，第二次是"B 在 offer 前被排除"未被旧断言接受。两次抓包审计本身通过（A/B/C 5060、5061、8088 全 0），但窗口不完整，R3 记 **INCOMPLETE**。
- 第二次中断时用例在断言处未挂断，留下已接通的 `8aab9419-fe70-4350-95e3-6abc295bb38b`。该通话控制过期后 A 与网关各留占用槽，**final APK 无恢复路径**（无 `recover_idle_call`；旧 recovery fixture 报 `recovery_precondition_or_operation_failed`；hangup/reject/clear、App 重启、整机重启均无效），此后去电在授权阶段被拒、来电不振铃。该终态在正常使用中可达（App 被杀 / 电池优化暂停同步），按用户指示中断测试并记录，未删除通话、未改数据库。
- 因此 R4、R5 未在最终 head 上执行；实验室需先决定如何处理该遗留通话。helper 变更见 `ROUND2-HELPERS.md` 与报告新节。
