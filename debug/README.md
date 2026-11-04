# HiDeck debug-field 调试版说明

这是给报告问题的用户用的调试版，适用于"问题只在你的卡上出现、我们手上没有这张卡"的情况。它基于当前 main，功能不变，只是把能记的日志都打开了。你只要正常使用，等问题出现以后跑一条收集命令，把生成的压缩包发给我们，不用一轮一轮地按我们的要求去找日志。

## 一、比正式版多记了什么

| 内容 | 位置 | 说明 |
|---|---|---|
| 每条 IMS SIP 消息，收发两个方向 | `logs/app-日期.log`，以 `[dev] IMS SIP rx` / `tx` 开头 | 完整头部，打码后保留；带连接编号、本端和对端地址 |
| 每条 IMS TCP 连接的建立、读失败（EOF、RST、超时）、关闭 | `[dev] IMS conn opened` / `read failed` / `closed` | 带收发字节数，以及关闭前各方向静默了多久 |
| 每 5 分钟一次 IMS 状态快照 | `[dev] IMS state snapshot` | 注册状态、P-CSCF、port-s、安全关联等，打码后保留 |
| 每个 IKE 包的头部，收发两个方向 | `[dev] IKE rx` / `tx` | 交换类型、请求/响应、消息 ID、SPI、长度 |
| 每个 IKE 消息解密后的载荷摘要 | `[dev] IKE payloads rx` / `tx` | 载荷类型、notify 类型、Delete 的 SPI、SA 提议的 SPI、流量选择器；身份、EAP、密钥、nonce、配置内容不记 |
| CHILD_SA 的建立、删除和退役 | `[dev] CHILD_SA installed` / `delete sending` / `peer delete` / `inbound retired` | 新旧 SPI、当前收发 SPI |
| 每 30 秒一次，每个 SPI 的 ESP 收发包数 | `[dev] SWu ESP per SA` | 累计和本次新增的包数、字节数、错误数、最后一个包距今多久 |
| IMS TCP 连接断开时 gVisor 的 TCP 状态 | 随 `[dev] IMS conn read failed` / `closed` 一起打 | 收发段数、重传、RTO 超时次数、RTT、RTO、拥塞窗口 |
| 每条经 AT 口排队执行的 AT 命令 | `[dev] AT command` | 命令、响应、耗时、错误；11 位以上数字打码 |
| 每次 QMI 设置射频模式 | `[dev] QMI SetOperatingMode` | 请求的模式、结果、耗时 |
| 内层 IMS 抓包（只有包头） | `logs/dev-pcap/*.pcapng` | 只保留 IP 和 TCP/UDP 头，不含 SIP 正文；可以用 Wireshark 打开 |
| 正式版的 Debug 级日志（含每 30 秒一次的 SWu 数据面统计） | `logs/app-日期.log` | 不用打开 `server.debug`；时间精确到毫秒 |
| 宿主机网络事件 | 收集包里的 `network.txt` | NetworkManager、wpa_supplicant 等服务和内核的 Wi-Fi、链路、DHCP 记录；MAC 和 SSID 打码 |

## 二、隐私

- `[dev]` 日志里的 SIP 报文都先打了码：号码、IMSI、IMPI、IMEI、显示名、AP 的 MAC、鉴权信息、digest 的 nonce 全部遮掉；短信正文一律不记，只记类型和长度。
- 收集脚本会对所有文本再做一遍处理：11 位以上的连续数字都换成 `<masked>`。处理完如果还能找到像身份号码的内容，脚本就报错，不生成压缩包。
- `config.yaml` 不会被收集，里面有密码和 token。
- **抓包里保留了内层 IPv6 地址**，日志里也保留了 Call-ID。我们要靠这些区分是哪个 P-CSCF/SBC 节点。如果不想公开，请把压缩包放到私有仓库，邀请我们访问，不要直接贴在 issue 里。

## 三、构建

```sh
git fetch https://github.com/BFNOC/hideck.git debug-field
git checkout FETCH_HEAD
git log -1 --oneline        # 记下这一行，回传时一起贴
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=debug-field-$(git rev-parse --short HEAD)'" \
  -o hideck-debug ./cmd/hideck
```

arm64 设备把 `GOARCH=amd64` 换成 `GOARCH=arm64`。

## 四、部署和回滚

需要 root，在构建目录里执行。`/root/hideck/hideck` 换成你实际的二进制路径。任何一步失败都会停下来；备份文件名带时间，不会覆盖之前的备份：

```sh
BIN=/root/hideck/hideck; BAK="$BIN.before-debug-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-debug && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-debug "$BIN" && systemctl start hideck
```

回滚：`systemctl stop hideck && cp <上面打印的 backup 路径> /root/hideck/hideck && systemctl start hideck`。调试版不改数据库结构，回滚时不用处理数据库。

启动后确认一下调试版已经生效：

```sh
grep -h 'debug-field build' /root/hideck/logs/app-*.log | tail -1
```

## 五、运行

正常使用，**跑到问题出现为止**。问题出现时，记下大概时间和你看到的现象，比如"14:05 验证码没收到""14:20 界面显示 SMS RX 红叉"。

磁盘占用：日志按天轮转；抓包每建立一次 VoWiFi 隧道生成一个文件，每个最多 64 MB。只跑到问题出现的话一般不大。如果不想抓包，部署前在 systemd 里加 `Environment=HIDECK_DEV_PCAP_DIR=off`。

## 六、收集和回传

在 HiDeck 所在的机器上用 root 执行（抓包目录只有 root 能读）。`--since` 填部署调试版的时间，或者问题出现前半小时，**按 HiDeck 日志里显示的时间填**（Docker 里的时区可能和宿主机不同）：

```sh
sh debug/collect.sh --since "2026-11-04 08:00"
```

脚本默认从 systemd 的工作目录里找 `logs/`；找不到时用 `--logs /实际路径/logs` 指定。跑完会在当前目录生成 `hideck-debug-时间.tar.gz`，并打印摘要的前几行。

回传时请附上：

1. 压缩包（建议放私有仓库）
2. `git log -1 --oneline` 的输出
3. 问题出现的时间和现象

压缩包里的 `summary.txt` 有计数和时间线，你自己也可以先看看。

## 七、给开发者：加一轮实验

需要对照实验时，在这个分支上加一个小提交：实验开关读环境变量 `HIDECK_DEV_EXPERIMENT`，只对指定的运营商预设生效，启动时用 `logging.DevLog` 打出当前生效的是哪个实验。所有调试日志都走 `logging.DevLog`（带 `[dev]` 前缀），SIP 文本都过一遍 `logging.DevRedactSIP`。这个分支不提 PR；要上游的修复单独基于 main 做。
