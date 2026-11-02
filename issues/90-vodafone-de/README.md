# hideck#90 Vodafone DE 测试分支说明（NOTIFY 的 200 与 MWI）

本分支只用于找出 Vodafone DE（262/02，预设 `vodafone_de_26202`）下 port-s 被对端 RST 的原因。它不是正式版本，测试完请换回正式版本。

## 一、上一轮看到了什么

上一轮（`test-vodafone-de-port-s-route`，7 轮）：

- REGISTER 去掉 `Route`，以及同时去掉 `Route` 和 `Feature-Caps`，RST 照旧，RST 后的刷新仍没有 200。
- RST 按节点固定节拍出现，`*.vodafone.com` 约 30–35 秒；同一张卡在同一个 P-CSCF 地址（`954::5`）上，VoHive 连续 97 分钟没有 RST。这支持"触发条件和 HiDeck 的行为有关"，但同一个地址后面不一定是同一个 SBC 实例，所以还不是定论。
- 改过、没有效果的：Via 的几种改法、`alias`、`Supported: outbound`、port-s 的 TCP keepalive、能力查询 OPTIONS、REGISTER 去掉 Route，以及同时去掉 Route 和 Feature-Caps（Feature-Caps 没有单独测过）。第一次 RST 之前我们发出的另外几样：REGISTER 本身、SUBSCRIBE(reg)、SUBSCRIBE(mwi)、回 NOTIFY 的 200，还没单独测过。

回 NOTIFY 的 200 有不合规的地方：

- TS 24.229 附录 A Table A.66：NOTIFY 的 2xx 里 `Record-Route` 为 c3（支持 SIP 事件通知时为 m，必须带），`Contact` 为 o（可选）。
- RFC 6665 §4.4.1：新订阅（`Subscription-State` 不是 `terminated`）的对话在 NOTIFY 事务完成时建立；RFC 3261 §12.1.1：建立对话的响应 MUST 原样按序复制请求里的全部 `Record-Route`，并 MUST 带 `Contact`。所以至少新订阅的第一个 NOTIFY，它的 200 应该两样都带。

Vodafone DE 的每个 NOTIFY 都带 `Record-Route`，我们的 200 两样都没有。另外 SUBSCRIBE(mwi) 每个周期都收到 480。

## 二、相对上一版改了什么

1. **两个新实验开关**（仍用 `HIDECK_DEV90_EXPERIMENT`，可以逗号组合）：

| 值 | 改动 |
|---|---|
| `notify-dialog` | 回 NOTIFY 的 2xx 带上 `Contact`（与 SUBSCRIBE 的 Contact 相同的地址，加 `transport=tcp`），并按原样、原顺序复制 NOTIFY 的全部 `Record-Route` 行。每个 2xx 都加，不只第一个 |
| `no-mwi` | 不发 SUBSCRIBE(mwi)，每次跳过记一条 `[dev] IMS experiment skips SUBSCRIBE(mwi)` |

   上一版的开关仍然可用。

2. **补全 SIP 日志**（所有轮次）：
   - `[dev] IMS inbound response`：我们回给网络的每个响应全文，带写入结果，以及距离 port-s 打开多久。MESSAGE 的响应只记头部。回 NOTIFY 的响应另带 `contact` 和 `record_route_copied` 两个字段，表示有没有 Contact、Record-Route 是否与 NOTIFY 逐行一致。
   - IMS TCP 收到的报文改为按原始字节交给后续处理，和受保护 UDP 一致；之前交的是 sipgo 解析后重新拼出的文本，会丢掉 `>` 后面的头参数，日志里看到的 NOTIFY 也是拼出来的。
   - `[dev] IMS SUBSCRIBE exchange`：SUBSCRIBE(reg/mwi) 的请求和最终响应全文、耗时；没收到响应时记错误。
   - 打码规则和 REGISTER 日志相同。

改动的文件：`third_party/vowifi-go/internal/vowifi/imscore/` 下的 `dev_experiment.go`、`dev_trace.go`，以及 `subscription_notify.go`、`subscribe_mwi.go`、`subscribe_reg.go`、`transport_runtime.go`、`register_transport.go`、`sip_parser.go` 里调用它们的地方；对应的 `_test.go`；`issues/90-vodafone-de/run-experiments.sh`。

## 三、构建、部署、回滚

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-notify-dialog
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-notify'" \
  -o hideck-issue90 ./cmd/hideck
```

部署（需要 root，在构建目录里执行；`/root/hideck/hideck` 换成实际路径）。整段任何一步失败都会停下；备份文件名带时间，不会覆盖之前的 `hideck.v2.1.38.bak`（它可能已经被前几轮的测试版覆盖，不要把它当正式版）：

```sh
BIN=/root/hideck/hideck; BAK="$BIN.before-notify-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-issue90 && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-issue90 "$BIN" && systemctl start hideck
```

回滚：`systemctl stop hideck && cp <上面打印的 backup 路径> /root/hideck/hideck && systemctl start hideck`。`config/carrier_overrides.yaml` 里不要给 26202 加任何 override。

## 四、运行

和上一版一样，**启动一次即可**：

```sh
sudo DEVICE=wwan0 sh issues/90-vodafone-de/run-experiments.sh
```

默认跑 6 轮，每轮 10 分钟，共约 1 小时。RST 约 30 秒一次，扣掉每轮开头 2 分钟的检查，每轮还能看到十几个周期：

| 轮 | 实验 | 要回答的问题 |
|---|---|---|
| 01 | `baseline` | 对照 |
| 02 | `notify-dialog` | 200 补全后，port-s 能不能跨过几个 RST 周期 |
| 03 | `no-mwi` | 不发 SUBSCRIBE(mwi) 后 RST 还在不在 |
| 04 | `baseline` | 对照，排除换节点的影响 |
| 05 | `notify-dialog` | 重复 02 |
| 06 | `no-mwi,notify-dialog` | 两项一起 |

- `DEVICE` 是 Vodafone DE 那张卡在日志里的前缀，例如 `[wwan0] IMS ...` 就填 `wwan0`。
- 只跑其中几轮：`EXPERIMENTS="baseline notify-dialog"`；组合用逗号，不加空格。改每轮时长：`MINUTES=15`。写错实验名时脚本在开始前就会退出。
- 其余（每轮 2 分钟后的检查、systemd drop-in、按 invocation 取日志、抓包、`OUTER_IFACE`）和上一版相同。

如果 02 或 05 轮里 port-s 一直没断，请在那一轮给这张卡发一条短信或申请一次验证码，记下发送和收到的时间。

## 五、回传

输出目录里有：

- `summary.txt`：每轮一行，字段：
  - `valid`：`no` 表示这一轮数据不能用（实验没生效、抓包没打开或丢了记录、该关的 keepalive 没关、`recovery-jitter` 轮里有 RST 却没有延迟记录、`notify-dialog` 轮写出的 NOTIFY 2xx 没带 Contact、或 Record-Route 与 NOTIFY 不一致、`no-mwi` 轮仍发了 SUBSCRIBE(mwi)、读不到日志）
  - `warn`：数据能用但要注意。`no-port-s-<秒>` 表示有一段超过 5 分钟一条 port-s 都没有；`user-not-registered-<次>` 表示出现过 `480 User not registered`；`notify-200-write-failed-<次>` 表示有回 NOTIFY 的 2xx 没写出去
  - `opened`、`peer_reset`、`reset_lifetimes`、`survived_to_end`、`other_close`、`unclosed`：port-s 打开、RST、存活时长等。按"隧道 + generation"计数，服务内部重连后不会再漏算
  - `cycle_s`：同一条隧道里相邻两次 RST 的间隔（秒）
  - `refresh_after_reset`：RST 后由 port-s 恢复触发的那次 REGISTER 的最终状态码和次数。401/407 挑战不算最终结果；`challenged` 表示收到挑战后没有最终应答，`none` 表示什么都没收到就开始了下一次注册
  - `jitter_refresh`：`recovery-jitter` 轮里每次的延迟和对应的最终状态码，例如 `41.2s:200`
  - `pcscf`、`sbc`：分到的 P-CSCF 和 NOTIFY `Contact` 里的 SBC 主机名
  - `user_not_registered`、`runtime_reconnects`、`max_port_s_gap_s`：SUBSCRIBE(reg) 收到 `480 User not registered` 的次数、隧道重连次数、最长一段一条 port-s 都没有的时间
- `<序号>-<实验名>.log`、`pcap-<序号>-<实验名>/`、`outer-<序号>-<实验名>.pcap`：同上一版。组合实验名里的逗号换成 `+`

请把 **`summary.txt` 直接贴出来**，把整个输出目录打包作为附件。外层抓包里有你的公网 IP 和 ePDG 地址，不想公开就不要放进附件。隐私规则和打码方式同上一版，打包前请再人工看一遍。

## 六、结果怎么看

| 看到的 | 说明 |
|---|---|
| `notify-dialog` 轮 port-s 跨过多个周期不断（`peer_reset` 很少、`survived_to_end` 有值），两次都这样 | 强烈支持"200 缺 Contact/Record-Route 和 RST 有关"，会整理成正式修复，再确认短信能收到 |
| `notify-dialog` 轮 RST 间隔明显变长但仍有 RST | 方向可能对，但还有别的条件；看 `IMS inbound response` 和网络后续有没有在这个对话里发请求 |
| `notify-dialog` 轮网络开始回别的状态码，或 NOTIFY 变了 | 说明网络对这个改动有反应，原因待查，把对应的日志行贴出来 |
| `no-mwi` 轮 RST 消失 | 支持"MWI 订阅和 RST 有关" |
| 几轮都和 `baseline` 一样 | 不支持这两项是触发条件。剩下还没单独测过的主要是 REGISTER 本身和 SUBSCRIBE(reg)，最好拿到能正常工作的客户端的报文来对照 |
