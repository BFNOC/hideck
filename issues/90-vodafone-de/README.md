# hideck#90 Vodafone DE 测试分支说明（port-s 抓包与单变量实验）

本分支只用于找出 Vodafone DE（262/02，预设 `vodafone_de_26202`）下 port-s 被对端 RST 的原因。它不是正式版本，测试完请换回正式版本。

## 一、为什么要再跑一轮

上一轮日志里，TCP/ESP 层只有注册成功那一刻的一次计数快照（`IMS downlink path diagnostics`），port-s 建立之后没有任何包级记录。能看到 NOTIFY 进来、我们回了 200、几十秒后 RST，但看不到中间发生了什么：

- 我们的 200 和 TCP ACK 有没有真正发出去，对方有没有在重传
- 那个 RST 是经过 IMS SA 解密进来的，还是明文进来被放行的
- RST 之前我们有没有发过 keepalive 探测或别的包

这些需要包级别的记录。内层流量有 SWu 和 IMS IPsec 两层加密，外部抓包解不开，所以这一版让 HiDeck 自己在加解密的位置写包头。抓包能看到我们发出和收到的每个包，但看不到对方实际收到了什么，所以它能缩小范围，不一定能单独证明原因。

## 二、相对上一版改了什么

1. **撤掉"RST 后等 3 分钟"**（上一版第 3 条，29/29 没有等到重连）。保留"刷新收到 403 后在隧道内做初始注册"和"403 不再换模板重试"。
2. **内层包头抓包**（`HIDECK_DEV90_PCAP_DIR`）。设置后，每条 IMS 隧道写一个 `ims-inner-<时间>-<序号>.pcapng`，Wireshark 或 tcpdump 可以直接打开。
   - 只保留能完整解析出的头：IP 头（含 IPv6 扩展头）、TCP/UDP 头、ESP 的 SPI 和序号；ICMP 只保留 ICMP 头，差错报文再加上被引用包的 IP 头和传输层头。**载荷一律不保留**；解析不了的包（非首片分片、长度字段对不上、未知格式）只保留已确认的部分或什么都不留。这些规则都有测试。
   - 每个包带一条注释，说明它是经 SA 收发的（`in esp spi=… seq=…` / `out esp spi=… seq=…`）、明文收发的（`in plaintext` / `out plaintext`），还是被丢弃的及原因（`in drop …: <错误>`）；有载荷时附长度，SIP 只附方法或状态码和 CSeq（例如 `len=2834 sip=NOTIFY cseq=1_NOTIFY`），CRLF 保活标成 `crlf-ping` / `crlf-pong`。
   - 写文件在单独的协程里做，不阻塞收发；队列满时丢掉这条记录，关闭时日志里的 `dropped` 会报告丢了多少。单个文件最多 64 MB，到上限后停止写入并记一条日志。
   - 抓包对这台机器上所有卡生效，实验开关只对 Vodafone DE 生效。
3. **port-s 打开和关闭时各打一次 `IMS downlink path diagnostics`**（`event=port_s_opened` / `port_s_closed`），带 port-c、port-s 两路 ESP 的收发计数、RST 计数、明文放行计数。
4. **单变量实验开关**（`HIDECK_DEV90_EXPERIMENT`），一次只能选一个，只对 `vodafone_de_26202` 生效，其他卡不受影响：

| 值 | 改动 |
|---|---|
| `baseline`（或不设） | 不改行为（相对于本测试版） |
| `no-options` | 注册后不发能力查询 OPTIONS |
| `no-alias` | 受保护 REGISTER 的 Via 去掉 `;alias` |
| `via-port-c` | 受保护 REGISTER 的 Via 端口改为 port-c |
| `via-port-s` | 受保护 REGISTER 的 Via 端口改为 port-s |
| `no-outbound` | REGISTER 的 Supported 去掉 `outbound` |
| `no-port-s-keepalive` | 关掉 port-s 连接上的 TCP keepalive 探测 |

   当前生效的值会出现在每条 `[dev] IMS REGISTER attempt context` 的 `experiment` 字段里。写错的值按 `baseline` 运行，并记一条 `[dev] IMS experiment not recognised`。

改动的文件：

- `third_party/vowifi-go/internal/vowifi/netstack/dev_pcap.go`、`packet_bridge.go`：抓包
- `third_party/vowifi-go/internal/vowifi/imscore/dev_experiment.go`，以及 `register.go`、`register_transport.go`、`dev_trace.go` 里调用它的地方：实验开关和 port-s 诊断
- `third_party/vowifi-go/internal/vowifi/netstack/gvisor_tcp.go`、`gvisor_tcp_errors.go`：关闭 keepalive 用的接口
- 对应的 `_test.go`

## 三、构建、部署、回滚

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-port-s-trace
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-trace'" \
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

用 `issues/90-vodafone-de/run-experiments.sh`，**启动一次即可**。它会先跑一轮 baseline，再依次跑每个实验，最后再跑一轮 baseline 作对照，每轮 25 分钟（共 8 轮，约 3 小时 20 分钟）。每轮结束时会停一下服务，好把这一轮的连接关闭也记进去；全部跑完后自动去掉实验设置并重启服务：

```sh
sudo DEVICE=wwan0 sh issues/90-vodafone-de/run-experiments.sh
```

- `DEVICE` 是 Vodafone DE 那张卡在日志里的前缀，例如 `[wwan0] IMS ...` 就填 `wwan0`。
- 脚本通过 systemd drop-in（`/etc/systemd/system/hideck.service.d/issue90.conf`）设置环境变量，中途按 Ctrl+C 也会清理掉。如果这个文件已经存在，脚本会拒绝运行，不会覆盖。
- 只想跑其中几项：`EXPERIMENTS="baseline no-options"`；改每轮时长：`MINUTES=30`。
- 日志按每轮的 systemd invocation 从 journald 取，不会混进上一轮的记录。
- 每轮启动 2 分钟后会检查实验是否生效、抓包是否打开，任一项不满足就直接停止并说明原因，不用白等 25 分钟。
- 如果装了 `tcpdump`，脚本会同时抓外层的 IKE/ESP 包头（UDP 500、4500 和 ESP，每包只留前 96 字节）。用来分辨"对方的包到了这台机器但在 SWu 层被丢"和"对方根本没发"。默认抓所有接口，可以用 `OUTER_IFACE=eth0` 指定，`OUTER_IFACE=off` 关闭。
- 抓包文件不带卡的名字。多张卡同时在跑时，用文件里的内层地址对照日志里的 `inner_ip` 区分。

运行期间正常使用即可。如果能在某个实验运行时给这张卡发一条短信，记下时间和当时的实验名，会更有用，但不是必须的。

## 五、回传

脚本最后会打印输出目录，里面有：

- `summary.txt`：每轮一行。`valid=no` 表示这一轮的数据不能用（没看到实验生效、抓包没打开或丢了记录、keepalive 没关掉、读不到日志）。其余字段：port-s 打开次数、RST 次数和每次 RST 前的存活时长、一直活到本轮结束的连接（`survived_to_end`）、其他方式关闭的连接、没有记录到关闭的连接数
- `<序号>-<实验名>.log`：这一轮里带 `[wwan0]` 这类卡前缀（即 `DEVICE`）的全部日志，加上抓包状态行。11 位以上的连续数字（IMSI、ICCID、IMEI、号码）已替换成 `<masked>`，10 位的 SPI 保留
- `pcap-<序号>-<实验名>/`：内层包头抓包
- `outer-<序号>-<实验名>.pcap`：外层包头抓包（装了 tcpdump 时）

请把 **`summary.txt` 直接贴出来**，把整个输出目录打包作为附件。**外层抓包里有你的公网 IP 和 ePDG 地址**，如果不想公开，就不要把 `outer-*.pcap` 放进附件，我们需要时再请你从里面数具体的包。

隐私：

- `[dev]` 日志的打码规则和上一版相同（SIP URI 用户部分、IMEI、号码、Authorization、AP MAC 都会遮掉）。非 `[dev]` 的基础日志行里有明文 IMSI、ICCID、IMEI，例如设备启动时的"SIM 身份已同步到数据库"，脚本会把 11 位以上的数字统一遮掉。打包前仍请人工看一遍：这条规则遮不掉字母形式的身份（例如 SIP URI 里的用户名），也遮不掉 10 位以下的号码。
- 抓包文件里只有包头：内层 IP 地址、P-CSCF 地址、端口、TCP 序号、ESP 的 SPI 和序号。按上面的规则不保留载荷。上面的 `grep` 只能查出长数字串，查不出短号码或字母形式的身份，只能当辅助检查。

## 六、结果怎么看

| 看到的 | 说明 |
|---|---|
| 某个实验的 `peer_reset` 明显少于前后两轮 baseline、存活时长明显变长 | 支持"这个实验去掉的东西是触发条件"，需要再重复确认 |
| 抓包里 RST 的注释是 `in plaintext` | 这个 RST 没有经过 IMS SA，要查是谁发的 |
| RST 之前，对方带数据的同一个 seq 出现多次 | 对方在重传，说明它没收到我们的 ACK，或者没有按时收到 |
| RST 之前，**我们**发的同一段（例如 NOTIFY 的 200）出现多次，对方的 ACK 不前进 | 我们的包没到对方。如果 RST 总在 NOTIFY 之后 32 秒左右，和 P-CSCF 那边 NOTIFY 事务的超时（RFC 3261 Timer F = 64×T1）对得上 |
| 内层抓包里看不到对方的回应，外层抓包里却有对方发来的 ESP 包，或者 `port_s_closed` 的 `inner_endpoint` 里 `DecapsulationErrors`、`SAMisses` 增加了 | 对方的包到了这台机器，但在 SWu 层被丢了 |
| RST 紧跟在我们的 keepalive 探测之后 | 说明探测和 RST 有关，可能是对方已经丢了这条连接的状态，需要更多样本 |
| 以上都没有，双方的数据都已被确认，对方仍然 RST | 更像是对方主动断开，原因要看实验结果 |
