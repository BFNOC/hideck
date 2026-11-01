# hideck#90 Vodafone DE 测试分支说明（port-s 抓包与单变量实验）

本分支只用于找出 Vodafone DE（262/02，预设 `vodafone_de_26202`）下 port-s 被对端 RST 的原因。它不是正式版本，测试完请换回正式版本。

## 一、为什么要再跑一轮

上一轮日志只有 SIP 层：能看到 NOTIFY 进来、我们回了 200、几十秒后 RST，但看不到中间 TCP/ESP 层发生了什么：

- 我们的 200 和 TCP ACK 有没有真正发出去，对方有没有在重传
- 那个 RST 是经过 IMS SA 解密进来的，还是明文进来被放行的
- RST 之前我们有没有发过 keepalive 探测或别的包

这些只有包级别的记录才能回答。内层流量有 SWu 和 IMS IPsec 两层加密，外部抓包解不开，所以这一版让 HiDeck 自己在加解密的位置写包头。

## 二、相对上一版改了什么

1. **撤掉"RST 后等 3 分钟"**（上一版第 3 条，29/29 没有等到重连）。保留"刷新收到 403 后在隧道内做初始注册"和"403 不再换模板重试"。
2. **内层包头抓包**（`HIDECK_DEV90_PCAP_DIR`）。设置后，每条 IMS 隧道写一个 `ims-inner-<时间>-<序号>.pcapng`，Wireshark 或 tcpdump 可以直接打开。
   - 只保留 IP 和 TCP/UDP 头，**丢掉全部载荷**，文件里没有 SIP 文本，没有 IMSI、IMEI、号码。
   - 每个包带一条注释，说明它是经 SA 收发的（`in esp spi=… seq=…` / `out esp spi=… seq=…`）、明文收发的（`in plaintext` / `out plaintext`），还是被丢弃的及原因（`in drop …: <错误>`）；有载荷时附长度，SIP 只附方法或状态码和 CSeq（例如 `len=2834 sip=NOTIFY cseq=1_NOTIFY`），CRLF 保活标成 `crlf-ping` / `crlf-pong`。
   - 单个文件最多 64 MB，到上限后停止写入并记一条日志。
3. **port-s 打开和关闭时各打一次 `IMS downlink path diagnostics`**（`event=port_s_opened` / `port_s_closed`），带 port-c、port-s 两路 ESP 的收发计数、RST 计数、明文放行计数。
4. **单变量实验开关**（`HIDECK_DEV90_EXPERIMENT`），一次只能选一个，只对 `vodafone_de_26202` 生效，其他卡不受影响：

| 值 | 改动 |
|---|---|
| `baseline`（或不设） | 不改行为 |
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

用 `issues/90-vodafone-de/run-experiments.sh`，**启动一次即可**，它会依次切换每个实验、每个跑 25 分钟（共约 3 小时），结束后自动去掉实验设置并重启服务：

```sh
sudo DEVICE=wwan0 sh issues/90-vodafone-de/run-experiments.sh
```

- `DEVICE` 是 Vodafone DE 那张卡在日志里的前缀，例如 `[wwan0] IMS ...` 就填 `wwan0`。
- 脚本通过 systemd drop-in（`/etc/systemd/system/hideck.service.d/issue90.conf`）设置环境变量，中途按 Ctrl+C 也会清理掉。
- 只想跑其中几项：`EXPERIMENTS="baseline no-options"`；改每项时长：`MINUTES=30`。
- 默认从 `journalctl -u hideck` 取日志。如果日志只写在文件里，加 `LOG_FILE=/path/to/app.log`（注意跨天时日志文件会换名，跨天请用 journalctl）。

运行期间正常使用即可。如果能在某个实验运行时给这张卡发一条短信，记下时间和当时的实验名，会更有用，但不是必须的。

## 五、回传

脚本最后会打印输出目录，里面有：

- `summary.txt`：每个实验的 port-s 打开次数、关闭次数、其中 RST 次数、结束时是否仍然存活、每次存活时长
- `<实验名>.log`：该实验期间筛选过的日志
- `pcap-<实验名>/`：抓包文件

请把 **`summary.txt` 直接贴出来**，把整个输出目录打包作为附件。

隐私：

- `[dev]` 日志的打码规则和上一版相同（SIP URI 用户部分、IMEI、号码、Authorization、AP MAC 都会遮掉）。**但非 `[dev]` 的基础日志行里仍可能有明文 IMSI、ICCID、IMEI，例如设备启动时的"SIM 身份已同步到数据库"。** 脚本只筛选 IMS 相关的行，启动阶段的这些行一般不会进去，打包前请再用 `grep -E '[0-9]{14,}'` 检查一遍。
- 抓包文件里只有包头：内层 IPv6 地址、P-CSCF 地址、端口、ESP 的 SPI 和序号。没有载荷。

## 六、结果怎么看

| 看到的 | 说明 |
|---|---|
| 某个实验 `peer_reset` 明显变少、存活时长明显变长 | 这个实验去掉的东西就是触发条件 |
| 抓包里 RST 的注释是 `in plaintext` | RST 不是经 IMS SA 来的，要查是谁发的 |
| 抓包里 RST 之前，对方同一个 seq 的包出现多次 | 对方在重传，说明我们的 ACK 没到对方 |
| 抓包里 RST 紧跟在我们的 keepalive 探测之后 | RST 是我们的探测引出来的，对方在那之前已经丢了这条连接的状态 |
| 以上都没有，我们最后一个包已被确认，对方突然 RST | 对方主动断开，看哪个实验能让它消失 |
