# hideck#90 Vodafone DE 调试分支说明（MWI 策略版 + 调试日志）

本分支 = 上一轮的 `test-vodafone-de-mwi-policy`（MWI 策略）+ 通用调试日志。目的：**下一次 `transport_timeout` 或隧道重建出现时，一轮就把本机能看到的部分记全，尽量确定断在哪一层**。MWI 行为和上一轮一样，Vodafone DE 仍然默认不订阅 MWI。这一轮唯一的行为变化是 3.1 的 DPD 设置，结束后改回去。

请按顺序做第一步到第七步。每一步都写了"应该看到什么"，对不上就停下，把输出贴到 #90。所有命令都用 root 执行；`/root/hideck` 换成 HiDeck 的实际目录（systemd 的 `WorkingDirectory`）。

## 第一步：先导出旧日志（不用换版本）

先确认现在的版本没有打开 Debug 日志，打开时日志里会有短信正文：

```sh
grep -nE '^\s*debug:' /root/hideck/config/config.yaml; [ $? -eq 1 ] && echo "没有 debug 这一行（默认关闭）"   # 应为 debug: false，或者没有这一行；如果报文件不存在，先确认路径
```

如果是 `debug: true`，**先别导出**，在 #90 告诉我们。否则继续：在**换版本之前**，取出 11-04 两次 `transport_timeout` 前后的完整日志，所有行都要：

```sh
LOGS=/root/hideck/logs
for w in "08:46:00 08:53:00" "17:52:00 17:59:00"; do
  set -- $w
  awk -v a="2026-11-04 $1" -v b="2026-11-04 $2" \
    '/^\[[0-9][0-9][0-9][0-9]-/ { t = substr($0, 2, 19); keep = (t >= a && t <= b) } keep' \
    "$LOGS/app-2026-11-04.log" | sed -E 's/\x1b\[[0-9;]*m//g; s/[0-9]{11,}/<masked>/g; s/(imei:)[0-9A-Fa-f-]+/\1<masked>/g' > "issue90-old-$1.log"
  wc -l "issue90-old-$1.log"
done
```

应该看到两个文件，每个几百到几千行。贴之前再检查一遍号码、IMSI、IMEI、ICCID 有没有打码，然后放到**私有仓库**（见第七步）。

## 第二步：构建

```sh
( set -e
  git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-debug
  git checkout FETCH_HEAD
  git log -1 --oneline   # 应为本说明文件所在的提交，记下这一行
  make frontend-dist
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -tags "with_utls nomsgpack" \
    -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-debug'" \
    -o hideck-issue90 ./cmd/hideck
  echo BUILD_OK )
```

最后应打印 `BUILD_OK`；中间任何一步失败都会停在那一步。

arm64 设备把 `GOARCH=amd64` 换成 `GOARCH=arm64`。后面的步骤都在这个构建目录里执行（要用到 `debug/` 下的脚本）。

## 第三步：部署

### 3.1 临时把 Vodafone DE 的 DPD 间隔调到 40 秒（这一轮专用，会改变行为）

DPD 是 IKE 层的存活探测：连续一段时间没有收到认证通过的入向 IKE 或 ESP 包，就发一次探测，然后每隔几秒重发，最多等约 165 秒。Vodafone DE 预设是 120 秒才探测，而上一轮的中断在几十秒到几分钟内就会被隧道重建打断，探测来不及发。调到 40 秒后：

- **探测得到回应**：只说明那一刻 IKE 往返是通的；ESP 仍可能在外层被单独丢掉，要结合同一时间的 ESP 收发计数一起看
- **到隧道重建为止一直没有回应**：外层路径丢包和 ePDG 不回应都有可能，单凭这一点分不开
- **代价**：在默认重传设置、没有被别的 IKE 交换挡住的情况下，中断持续超过约 3.5 分钟（40 + 165 秒）时 DPD 会先判定超时、拆掉隧道重建，这种情况下可能看不到 `transport_timeout`，看到的是 `[dev] DPD probe timeout` 加一次重建。这也算目标事件。探测本身也可能让中断更早结束或更不容易出现
- 平时隧道里有入向流量（port-s 每 30 秒左右就有保活），不会多发探测；入向流量少的时候（比如 port-s 断开到重新注册之间）会多探几次，这是正常的

先备份 `config/carrier_overrides.yaml`，备份成功后才写入。下面的写法假设文件现在是 `carrier_overrides: {}`；如果里面已经有别的运营商条目，不要整份覆盖，只在 `carrier_overrides:` 下加 `"262-02"` 这一段；如果文件不存在，跳过 `cat` 和 `cp`，直接用 `cat > "$CFG"` 那一段新建：

```sh
CFG=/root/hideck/config/carrier_overrides.yaml
cat "$CFG"   # 应为 carrier_overrides: {}
cp "$CFG" "$CFG.before-issue90-debug-$(date +%Y%m%d-%H%M%S)" && cat > "$CFG" <<'YAML'
carrier_overrides:
  "262-02":
    # hideck#90 debug run only: DPD after 40 s of inbound silence. Remove afterwards.
    dpd_interval_seconds: 40
YAML
```

### 3.2 换二进制

部署时间在停服务**之前**记录。任何一步失败都会停下来；备份文件名带时间，不会覆盖之前的备份：

```sh
DEPLOY="$(date '+%F %T')"; echo "部署时间 $DEPLOY"; \
BIN=/root/hideck/hideck; BAK="$BIN.before-issue90-debug-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-issue90 && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-issue90 "$BIN" && systemctl start hideck
```

记下打印出来的部署时间，后面每一步都要用。

- 不需要任何环境变量或 systemd 附加设置；`/etc/systemd/system/hideck.service.d/` 里不要有 `issue90.conf`
- 「语音信箱提示」保持「自动」
- 回滚：`systemctl stop hideck`，把 `/root/hideck/hideck` 和 `carrier_overrides.yaml` 都换回上面的备份，然后 `systemctl start hideck`。不改数据库，回滚不用处理数据库

## 第四步：确认调试日志和 DPD 设置已生效（部署后 5 分钟内）

`debug/since.sh` 打印某个时间之后的日志行，下面都用它，只看部署之后的。把 `DEPLOY` 换成第三步打印的部署时间：

```sh
DEPLOY="2026-11-05 10:00:00"
sh debug/since.sh "$DEPLOY" | grep 'debug-field build' | tail -1      # 1 行：version 为 v2.1.38-issue90-debug，server_debug 为 false
sh debug/since.sh "$DEPLOY" | grep 'carrier_overrides' | tail -2      # 应有"carrier_overrides 已加载"，"entries": 1；如果是"加载 carrier_overrides 失败"或者一行都没有，停下
sh debug/since.sh "$DEPLOY" | grep -c 'SUBSCRIBE(mwi) skipped'        # 大于 0
sh debug/since.sh "$DEPLOY" | grep -c 'SUBSCRIBE(mwi) starting' || true   # 应为 0
sh debug/since.sh "$DEPLOY" | grep -c '\[dev\] SWu ESP per SA'        # 每 30 秒 1 行，持续增加
sh debug/since.sh "$DEPLOY" | grep -c '\[dev\] IMS SIP'               # 大于 0
ls -l /root/hideck/logs/dev-pcap/ | tail -3                           # 有部署之后创建的 ims-inner-*.pcapng；过一两分钟再看一次，文件应该变大
```

`server_debug` 如果是 `true`，停下告诉我们。日志时间现在精确到毫秒，例如 `[2026-11-05 01:28:21.504]`，这是正常的。

## 第五步：正常使用，跑到出现目标事件

不用盯着，正常使用即可。下面任意一种都算目标事件：

```sh
sh debug/since.sh "$DEPLOY" | grep 'port-s lifecycle' | grep transport_timeout | tail -3
sh debug/since.sh "$DEPLOY" | grep -E '\[dev\] DPD probe (timeout|bad_response|error)' | tail -3
sh debug/since.sh "$DEPLOY" | grep 'rekey attempt failed' | tail -3
```

- 出现**至少 1 次**目标事件，并且之后已经恢复（日志里重新出现 `IMS REGISTER succeeded`），再等 5 分钟，然后做第六步
- 如果 48 小时都没出现，也做第六步，同样有用
- 期间如果方便，做一次主动短信测试：用别的手机给这张卡发一条，再用这张卡发一条出去。分别记下发送时间、是否收到、收到时间
- 磁盘占用：日志约每天 20–50 MB；抓包 32 MB 一个文件，已关闭的旧文件会清理到总数 24 个左右（正在写的不删，所以偶尔会多一两个；平时约每天 15 MB）

## 第六步：收集

`--since` 填第三步记下的部署时间：

```sh
sh debug/collect.sh --since "$DEPLOY"
```

脚本默认从 systemd 的工作目录里找 `logs/`，找不到时加 `--logs /root/hideck/logs`。最后会打印 `written: .../hideck-debug-时间.tar.gz` 和摘要。摘要开头的 coverage 段会列出：日志时间范围、启动过的版本和 `server_debug`、override 是否加载、抓包文件清单和健康情况、每次 `transport_timeout`、最近 20 次 DPD 结果、换密钥失败。宿主机网络事件是尽力从 journal 回查的，最早能查到的时间写在 `network.txt` 开头，可能覆盖不到整个实验。

收集成功不代表抓包一定覆盖了故障那段：抓包队列满时会丢包（关闭时才在日志里报 `dropped`），正在写的文件末尾可能不完整。我们拿到后会自己核对。

如果打印"unmasked digit runs remain"并退出，说明发现了没打码的长数字，把这条提示贴给我们，不要上传压缩包。

## 第七步：回传，然后把 DPD 改回去

1. 把第一步的两个文件和第六步的压缩包放到**私有仓库**，邀请 BFNOC 访问。`Solomeyou/hideck-issue90-assets` 现在是公开仓库；可以把它改成私有，也可以新建一个私有仓库
2. 在 #90 贴：
   - 第二步记下的 `git log -1 --oneline`
   - 每次目标事件的时间，以及你当时有没有做什么（重启 Wi-Fi、切卡、重启模组等）
   - 主动短信测试的记录
   - `summary.txt` 开头的 coverage 和计数部分
3. **把 DPD 改回去**：这个设置写在配置文件里，换回正式版二进制也不会自己消失。把 `carrier_overrides.yaml` 换回备份（或者删掉 `"262-02"` 那一段），然后 `systemctl restart hideck`，再确认一下：

   ```sh
   cat /root/hideck/config/carrier_overrides.yaml   # 不应再有 dpd_interval_seconds: 40
   ```

   想同时停掉调试日志和抓包，就按 3.2 的回滚把二进制也换回去

## 这一版多记了什么

| 内容 | 日志行 | 用来看什么 |
|---|---|---|
| 每 30 秒，每个 SPI 的 ESP 收发包数 | `[dev] SWu ESP per SA` | 断的时候是收方向先停，还是发方向先停，停在哪个 SA；还有 SA 查找失败、解封装错误、NAT-T 保活尝试次数；隧道拆掉前再打一次（`"kind": "shutdown"`） |
| DPD 探测 | `[dev] DPD probe due` → `locked` → 结果 | 同一次探测的几行带同一个 `probe` 编号和 `trigger`（timer、nat_keepalive、network_event）。`due` 是该探测了，可能还在排队等别的 IKE 交换；`locked` 是拿到交换、交给发送；结果是 `answered`（有回应）、`timeout`（重发到上限都没有完成有效回应，也包括本机每次都没发出去的情况：`error` 里带 `last send failed` 就是发不出去，要结合 `[dev] IKE tx`、`[dev] outer UDP send failed` 一起看）、`interrupted`（还在等时隧道被拆，不能当作"没回应"）、`bad_response`（回应内容不对；认证不过的回应在更早一层就被丢了，会记成 `[dev] IKE open failed`）、`error`（本机出错，例如没有可用传输）。`network_event` 触发的探测失败不会拆隧道 |
| 换密钥失败 | `CHILD_SA rekey attempt failed` / `IKE SA rekey attempt failed`，配合 `[dev] IKE payloads rx` 里的 `notify` | 看 `error` 字段：有对应的 notify 行是对方拒绝（原因码在 notify 里），`timeout reached max retries` 是一直没回应 |
| 每 30 秒的内层统计 | `[dev] IMS inbound stats` | 内层 IMS SA（3GPP IPsec）的收发、重放拒绝、未知 SPI，以及 gVisor 协议栈的丢包、RST、重传计数 |
| CHILD_SA 建立、删除、退役 | `[dev] CHILD_SA installed`（`path` 为 initial/rekey/peer_pending）/ `delete sending` / `peer delete` / `inbound retired` | 和断开是否在同一时刻、换密钥后旧 SA 是否被提前删掉 |
| 每个 IKE 消息的头部和解密后的载荷摘要 | `[dev] IKE rx/tx`、`[dev] IKE payloads rx/tx` | 换密钥、DPD、对方发来的 Delete；身份、EAP、密钥内容不记。按包计数以 `[dev] IKE rx/tx` 为准 |
| 每条 SIP 消息（打码） | `[dev] IMS SIP rx/tx` | REGISTER 刷新发出去后有没有回应 |
| 每条 IMS 连接的建立、读失败、关闭 | `[dev] IMS conn opened/read failed/closed` | 断开前各方向静默了多久，TCP 重传、RTO 超时次数、RTT |
| 外层 UDP 发送失败和恢复、未知来源的包 | `[dev] outer UDP send failed/recovered`、`[dev] outer UDP datagram from an unexpected source dropped` | 本机外层发送是否出过错（只覆盖直连 UDP；走 SOCKS5 时没有这几行） |
| 每 5 分钟的 IMS 状态快照 | `[dev] IMS state snapshot` | 当时的注册、port-s、安全关联状态 |
| 内层抓包（只有包头） | `logs/dev-pcap/*.pcapng` | 隧道里的包有没有发出、有没有回来，TCP 有没有被确认 |
| 宿主机网络事件 | 压缩包里的 `network.txt` | 当时有没有 Wi-Fi 断开、DHCP 续租等 |

SWu 的 `[dev]` 行里 SPI 是十六进制，原有日志行里是十进制；IMS（3GPP）SA 的 SPI 和 SIP 头里一样用十进制。`session` 字段是会话对象的地址，同一个进程里旧会话结束后可能被复用，要配合 `ike_spi_i` 一起看。

这一版保持 `server.debug` 原样，不会强制打开 Debug。打码规则：`[dev]` 日志里的号码、IMSI、IMPI、IMEI、显示名、AP 的 MAC、鉴权信息、digest nonce/rspauth/ck/ik、SDP 密钥都已遮掉，AT 命令只记命令名（不含参数），短信正文不记；收集包里的宿主机网络日志会把 MAC、SSID 和连接名遮掉。收集脚本会把所有文本里 11 位以上的连续数字再遮一遍。**抓包和日志里保留了内层 IPv6 地址和 Call-ID**，所以请放私有仓库。
