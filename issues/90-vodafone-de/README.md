# hideck#90 Vodafone DE 测试分支说明（刷新 Route 与 RST 节拍）

本分支只用于找出 Vodafone DE（262/02，预设 `vodafone_de_26202`）下 port-s 被对端 RST 的原因。它不是正式版本，测试完请换回正式版本。

## 一、上一轮看到了什么

上一轮（`test-vodafone-de-port-s-trace`，8 轮）的内层抓包和日志说明：

- port-s 上没有任何数据重传，NOTIFY 的 200 都被对方 TCP 确认；每个 RST 都经 IMS SA 进来。
- RST 时长和实验无关，跟分到的 P-CSCF 有关：NOTIFY `Contact` 里是 `*.vodafone.com` 的约 30 秒，是 `*.nokia.com` 的约 55 秒。三个改 Via 的实验恰好都分到了后一种。
- 同一轮里两次 RST 的间隔大多是整数秒加约 0.07 秒（31.07、32.07、56.1、60.1 秒），同一节点上也会混着出现 55.1、56.1 秒。看起来像 SBC 按自己的周期发 RST，但还没排除和注册有关的计时器，这一轮专门验证。
- RST 之后的刷新 REGISTER 全部收到 403。这个刷新把 200 OK 里的 `Path`（`…;ottag=ue_term;…`）当成 `Route` 发了回去：Vodafone DE 的 200 OK 没有 `Service-Route`，代码就退回用 `Path`。TS 24.229（V19.7.0）附录 A 的 Table A.119 里，REGISTER 的 Route 头在 profile 一栏是 `x`（禁止）；§5.1.1.4.1 NOTE 5 也说存下的 Service-Route 用于 REGISTER 以外的请求。所以 403 可能是在拒绝这个 Route，不能据此说注册已被删掉。
- 首次注册不带 Route（还没有 Path），而每条隧道的第一次 RST 都发生在第一次刷新之前，所以 Route 解释不了第一次 RST。
- 有 4 次在 REGISTER 200 之后，SUBSCRIBE(reg) 收到 `480 User not registered`，随后 port-c 不再被确认，HiDeck 的 TCP 栈要重传约 15 分钟才放弃、重连，期间没有 port-s。

## 二、相对上一版改了什么

1. **实验开关可以组合**：`HIDECK_DEV90_EXPERIMENT` 可以写一个值，也可以用逗号连接多个，例如 `no-register-route,recovery-jitter`。日志里按字母序记录。只要有一个值写错，整轮按 `baseline` 运行，并记一条 `[dev] IMS experiment not recognised`。新增三个值：

| 值 | 改动 |
|---|---|
| `no-register-route` | REGISTER 不带 `Route` 头（不再把 `Path` 当 `Route`） |
| `no-feature-caps` | REGISTER 不带 `Feature-Caps` 头。RFC 6809 §1 说它用于代理、注册服务器、B2BUA 这类不由 Contact URI 代表的节点；TS 24.229 §5.1.1.2.1 列出的 UE REGISTER 头里没有它，P-CSCF 还应删掉普通 UE 发来的 Feature-Caps（§5.2.2.1）。所以它是触发条件的可能性不大，但首次注册就带着它，这个实验能直接用第一次 RST 检验 |
| `recovery-jitter` | port-s 被 RST 后，随机等 2–60 秒再发恢复 REGISTER，每次记一条 `[dev] IMS experiment delays the port-s recovery REGISTER`。用来判断下一次 RST 是跟着我们的 REGISTER 还是按 SBC 自己的周期，并看 RST 后多久 P-CSCF 还会应答（上一轮立刻刷新回 403，await 版 180 秒后刷新没有应答）。普通刷新或其他故障可能抢在延迟结束前发出 REGISTER，summary 只统计由 port-s 恢复触发的那次 |

   上一版的 `no-options`、`no-alias`、`via-port-c`、`via-port-s`、`no-outbound`、`no-port-s-keepalive` 仍然可用。

2. **IMS TCP 连接加 45 秒 user timeout**（所有卡、所有轮次都生效）。原来连接空闲时 keepalive 约 45 秒能发现对方不在，但一旦有数据没被确认，就改走重传，要退避约 15 分钟才放弃。现在未确认的数据超过 45 秒没有被确认就断开连接，随后 HiDeck 重建隧道和注册。按上一轮 03 轮的时序推算，从最后一次成功收发到断开约 1.5 分钟，之前是约 15 分钟；总空窗还要加上重连时间。旧 port-c 连接关闭后反复重传的 FIN 也会在 45 秒后停止（这些 FIN 不经过 IMS SA 的问题另外修）。连接关闭的错误里会带上 `(last read … ago)`，表示关闭前多久没收到对方的数据，用来分辨是哪种超时。
3. **`[dev] IMS port-s timing` 加了 `at` 字段**（毫秒时间），summary 用它算 RST 节拍。
4. **summary 加了字段，修了计数**，见第五节。

改动的文件：

- `third_party/vowifi-go/internal/vowifi/imscore/dev_experiment.go`，以及 `register.go`、`register_transport.go`、`dev_trace.go` 里调用它的地方
- `third_party/vowifi-go/internal/vowifi/netstack/gvisor_tcp.go`：TCP user timeout；`imscore/register_transport.go`：连接关闭错误里的 `last read`
- 对应的 `_test.go`，`issues/90-vodafone-de/run-experiments.sh`

## 三、构建、部署、回滚

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-port-s-route
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-route'" \
  -o hideck-issue90 ./cmd/hideck
```

部署和回滚与上一版相同（下面用 `/root/hideck/hideck` 举例，以实际路径为准）：

```sh
systemctl stop hideck
cp /root/hideck/hideck /root/hideck/hideck.v2.1.38.bak
install -m 755 hideck-issue90 /root/hideck/hideck
systemctl start hideck
```

回滚：把 `.bak` 拷回去，再重启服务。`config/carrier_overrides.yaml` 里不要给 26202 加任何 override。

## 四、运行

和上一版一样，**启动一次即可**：

```sh
sudo DEVICE=wwan0 sh issues/90-vodafone-de/run-experiments.sh
```

默认跑 7 轮，每轮 25 分钟，共约 2 小时 55 分钟：

| 轮 | 实验 | 要回答的问题 |
|---|---|---|
| 01 | `baseline` | 对照 |
| 02 | `no-register-route` | RST 后不带 Route 的刷新是 200 还是 403；每条隧道第一次 RST 之后还会不会再 RST |
| 03 | `no-register-route,recovery-jitter` | 恢复 REGISTER 推迟后，RST 间隔是否仍是同一个周期；推迟多久后 P-CSCF 不再应答 |
| 04 | `no-feature-caps,no-register-route` | 去掉 Feature-Caps 后第一次 RST 还在不在 |
| 05 | `no-register-route` | 重复 02 |
| 06 | `no-feature-caps,no-register-route` | 重复 04 |
| 07 | `baseline` | 对照 |

- `DEVICE` 是 Vodafone DE 那张卡在日志里的前缀，例如 `[wwan0] IMS ...` 就填 `wwan0`。
- 只跑其中几轮：`EXPERIMENTS="baseline no-register-route"`；组合用逗号，不加空格。改每轮时长：`MINUTES=30`。写错实验名时脚本在开始前就会退出。
- 每轮启动 2 分钟后检查实验是否生效、抓包是否打开，不满足就停止并说明原因。
- 其余（systemd drop-in、按 invocation 取日志、外层包头抓包、`OUTER_IFACE`）和上一版相同。

每轮重启服务都会建新隧道，可能分到不同的 P-CSCF，summary 会把每轮实际分到的 P-CSCF 和 SBC 列出来。

如果能在 02 或 05 轮运行时给这张卡发一条短信（或申请一次验证码），记下发送和收到的时间，会很有用。

## 五、回传

输出目录里有：

- `summary.txt`：每轮一行，字段：
  - `valid`：`no` 表示这一轮数据不能用（实验没生效、抓包没打开或丢了记录、该关的 keepalive 没关、`recovery-jitter` 轮里有 RST 却没有延迟记录、读不到日志）
  - `warn`：数据能用但要注意。`no-port-s-<秒>` 表示有一段超过 5 分钟一条 port-s 都没有；`user-not-registered-<次>` 表示出现过 `480 User not registered`
  - `opened`、`peer_reset`、`reset_lifetimes`、`survived_to_end`、`other_close`、`unclosed`：port-s 打开、RST、存活时长等。按"隧道 + generation"计数，服务内部重连后不会再漏算
  - `cycle_s`：同一条隧道里相邻两次 RST 的间隔（秒）
  - `refresh_after_reset`：RST 后由 port-s 恢复触发的那次 REGISTER 的最终状态码和次数。401/407 挑战不算最终结果；`challenged` 表示收到挑战后没有最终应答，`none` 表示什么都没收到就开始了下一次注册
  - `jitter_refresh`：`recovery-jitter` 轮里每次的延迟和对应的最终状态码，例如 `41.2s:200`
  - `pcscf`、`sbc`：分到的 P-CSCF 和 NOTIFY `Contact` 里的 SBC 主机名
  - `user_not_registered`、`runtime_reconnects`、`max_port_s_gap_s`：480 次数、隧道重连次数、最长一段一条 port-s 都没有的时间
- `<序号>-<实验名>.log`、`pcap-<序号>-<实验名>/`、`outer-<序号>-<实验名>.pcap`：同上一版。组合实验名里的逗号换成 `+`

请把 **`summary.txt` 直接贴出来**，把整个输出目录打包作为附件。外层抓包里有你的公网 IP 和 ePDG 地址，不想公开就不要放进附件。隐私规则和打码方式同上一版，打包前请再人工看一遍。

## 六、结果怎么看

下面每条只说明结果支持或不支持哪个假设，不单独定结论；判断时要同时看节点（`sbc`）、port-s 打开次数和空窗。

| 看到的 | 说明 |
|---|---|
| `no-register-route` 轮的 `refresh_after_reset` 是 200 | 刷新在 RST 后仍被接受，支持"403 是 Route 造成的"。接下来看刷新成功后 P-CSCF 会不会再连 port-s、短信能不能收到 |
| `no-register-route` 轮每条隧道只剩第一次 RST | 支持"后续反复 RST 和反复重注册有关"。第一次 RST 发生在任何刷新之前，这个实验去不掉它 |
| `no-register-route` 轮刷新仍是 403 | 不支持"403 是 Route 造成的"，RST 时 P-CSCF 那边的状态可能已经变了，原因要在 RST 之前找 |
| `no-feature-caps` 轮第一次 RST 也没有了 | 支持"Feature-Caps 和 RST 有关"，要再重复确认 |
| `recovery-jitter` 轮的 `cycle_s` 仍集中在同一个值或它的整数倍 | 支持"RST 按 SBC 自己的周期来"。刷新晚于下一个周期点时，那一拍没有 port-s 可断，会出现整数倍 |
| `recovery-jitter` 轮的 `cycle_s` 跟着延迟变化 | 支持"RST 跟着我们的 REGISTER 走" |
| `jitter_refresh` 里延迟短的是 200/403、长的是 `none` | 给出 RST 后 P-CSCF 还应答的大致时限 |
| 出现 `user-not-registered`，且对应的 `connection timed out` 错误里 `last read` 约 1.5 分钟 | 45 秒 user timeout 起作用了 |
