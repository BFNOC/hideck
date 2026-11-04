# hideck#90 Vodafone DE 调试分支说明（MWI 策略版 + 调试日志）

本分支 = 上一轮的 `test-vodafone-de-mwi-policy`（MWI 策略，不改行为）+ 通用调试日志。目的只有一个：**下一次 `transport_timeout` 出现时，一次就把隧道里断在哪一层记下来**。功能和上一轮完全一样，Vodafone DE 仍然默认不订阅 MWI。

请按顺序做第一步到第六步。每一步都有"应该看到什么"，对不上就停下，把输出贴到 #90。

## 第一步：先贴旧日志（不用换版本）

在**换版本之前**，从现在的日志里取出 11-04 两次 `transport_timeout` 前后的完整日志。所有行都要，不要只挑 port-s 和 REGISTER 相关的：

```sh
LOGS=/root/hideck/logs   # 换成 HiDeck 实际的 logs 目录（systemd 的 WorkingDirectory 下的 logs/）
for w in "08:46:00 08:53:00" "17:52:00 17:59:00"; do
  set -- $w
  awk -v a="2026-11-04 $1" -v b="2026-11-04 $2" \
    '/^\[[0-9][0-9][0-9][0-9]-/ { t = substr($0, 2, 19); keep = (t >= a && t <= b) } keep' \
    "$LOGS/app-2026-11-04.log" | sed -E 's/\x1b\[[0-9;]*m//g; s/[0-9]{11,}/<masked>/g' > "issue90-old-$1.log"
  wc -l "issue90-old-$1.log"
done
```

应该看到两个文件，每个几百到几千行。贴之前再检查一遍号码、IMSI、IMEI、ICCID 有没有打码，然后放到**私有仓库**（见第七节）。

## 第二步：构建

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-debug
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交，记下这一行
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-debug'" \
  -o hideck-issue90 ./cmd/hideck
```

arm64 设备把 `GOARCH=amd64` 换成 `GOARCH=arm64`。

## 第三步：部署

需要 root，在构建目录里执行，`/root/hideck/hideck` 换成实际路径。任何一步失败都会停下来；备份文件名带时间，不会覆盖之前的备份：

```sh
BIN=/root/hideck/hideck; BAK="$BIN.before-issue90-debug-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-issue90 && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-issue90 "$BIN" && systemctl start hideck && date '+部署时间 %F %T'
```

记下打印出来的部署时间，第六步要用。

- 不需要任何环境变量或 systemd 附加设置；`/etc/systemd/system/hideck.service.d/` 里不要有 `issue90.conf`
- `config/carrier_overrides.yaml` 保持 `{}`，「语音信箱提示」保持「自动」
- 回滚：`systemctl stop hideck && cp <上面打印的 backup 路径> /root/hideck/hideck && systemctl start hideck`。不改数据库，回滚不用处理数据库

## 第四步：确认调试日志已生效（部署后 5 分钟内）

```sh
LOGS=/root/hideck/logs; F="$LOGS/app-$(date +%F).log"
grep 'debug-field build' "$F" | tail -1          # 1 行，version 为 v2.1.38-issue90-debug
grep -c 'SUBSCRIBE(mwi) skipped' "$F"            # 每次注册 1 次，大于 0
grep -c 'SUBSCRIBE(mwi) starting' "$F"           # 应为 0
grep -c '\[dev\] SWu ESP per SA' "$F"            # 每 30 秒 1 行，持续增加
grep -c '\[dev\] IMS SIP' "$F"                   # 大于 0
ls "$LOGS/dev-pcap/"                             # 有 ims-inner-*.pcapng（root 才能读）
```

日志时间现在精确到毫秒，例如 `[2026-11-05 01:28:21.504]`，这是正常的。

## 第五步：正常使用，跑到 `transport_timeout` 出现

不用盯着，正常使用即可。用下面这条命令可以随时看是否已经出现：

```sh
grep 'port-s lifecycle' /root/hideck/logs/app-*.log | grep transport_timeout | tail -3
```

- **至少等到出现 1 次 `transport_timeout`**，并且之后已经恢复（日志里重新出现 `IMS REGISTER succeeded`），再等 5 分钟，然后做第六步
- 如果 48 小时都没出现，也做第六步，同样有用
- 期间如果方便，做一次主动短信测试：用别的手机给这张卡发一条，再用这张卡发一条出去。分别记下发送时间、是否收到、收到时间
- 磁盘占用：日志约每天 60–100 MB，抓包约每天 15 MB

## 第六步：收集

用 root 执行，`--since` 填第三步记下的部署时间，按 HiDeck 日志里显示的时间填：

```sh
sudo sh debug/collect.sh --since "2026-11-05 10:00"
```

脚本默认从 systemd 的工作目录里找 `logs/`，找不到时加 `--logs /实际路径/logs`。最后会打印 `written: .../hideck-debug-时间.tar.gz` 和摘要。如果打印"unmasked digit runs remain"并退出，说明发现了没打码的长数字，把这条提示贴给我们，不要上传压缩包。

## 第七步：回传

1. 把第一步的两个文件和第六步的压缩包放到**私有仓库**，邀请 BFNOC 访问。`Solomeyou/hideck-issue90-assets` 现在是公开仓库；可以把它改成私有，也可以新建一个私有仓库
2. 在 #90 贴：
   - 第二步记下的 `git log -1 --oneline`
   - 每次 `transport_timeout` 的时间，以及你当时有没有做什么（重启 Wi-Fi、切卡、重启模组等）
   - 主动短信测试的记录
   - `summary.txt` 开头的计数部分

## 这一版多记了什么

| 内容 | 日志行 | 用来看什么 |
|---|---|---|
| 每 30 秒，每个 SPI 的 ESP 收发包数 | `[dev] SWu ESP per SA` | 断的时候是收方向先停，还是发方向先停，停在哪个 SA |
| CHILD_SA 建立、删除、退役 | `[dev] CHILD_SA installed` / `delete sending` / `peer delete` / `inbound retired` | 和断开是否在同一时刻、换密钥后旧 SA 是否被提前删掉 |
| 每个 IKE 消息的头部和解密后的载荷摘要 | `[dev] IKE rx/tx`、`[dev] IKE payloads rx/tx` | 换密钥、DPD、对方发来的 Delete；身份、EAP、密钥内容不记 |
| 每条 SIP 消息（打码） | `[dev] IMS SIP rx/tx` | REGISTER 刷新发出去后有没有回应 |
| 每条 IMS 连接的建立、读失败、关闭 | `[dev] IMS conn opened/read failed/closed` | 断开前各方向静默了多久，TCP 重传、RTO 超时次数、RTT |
| 外层 UDP 发送失败、未知来源的包、NAT-T 保活 | `[dev] outer UDP ...`、`[dev] NAT-T keepalive sent` | 外层路径是否出过错 |
| 每 5 分钟的 IMS 状态快照 | `[dev] IMS state snapshot` | 当时的注册、port-s、安全关联状态 |
| 内层抓包（只有包头） | `logs/dev-pcap/*.pcapng` | 隧道里的包有没有发出、有没有回来 |
| 宿主机网络事件 | 压缩包里的 `network.txt` | 当时有没有 Wi-Fi 断开、DHCP 续租等 |

打码规则：`[dev]` 日志里的号码、IMSI、IMPI、IMEI、显示名、AP 的 MAC、鉴权信息、digest nonce 都已遮掉，短信正文不记。收集脚本会把所有文本里 11 位以上的连续数字再遮一遍。**抓包和日志里保留了内层 IPv6 地址和 Call-ID**，所以请放私有仓库。
