# hideck#90 Vodafone DE 测试分支说明（延迟 MWI）

本分支只用于找出 Vodafone DE（262/02，预设 `vodafone_de_26202`）下 port-s 被对端 RST 的原因。它不是正式版本，测试完请换回正式版本。

## 一、上一轮看到了什么

上一轮（`test-vodafone-de-notify-dialog`，6 轮 × 10 分钟）：

- `no-mwi` 两轮 port-s 零 RST，活到轮末；其余 4 轮共 RST 54 次。SUBSCRIBE(mwi) 每次注册后都收到 480，`no-mwi` 两轮为 0。
- 但两轮各只开过 1 条 port-s，P-CSCF 也不同，只能说相关。
- `notify-dialog` 生效后 RST 照旧，这一项放下。

本轮要分清两种情况：RST 是 SUBSCRIBE(mwi) 触发的，还是注册后不论发什么都会出现。

## 二、相对上一版改了什么

新实验开关 `delay-mwi`（仍用 `HIDECK_DEV90_EXPERIMENT`）：每次注册后先等 120 秒再发 SUBSCRIBE(mwi)。等待期间注册变了（例如 RST 后重新注册），这次就不发，由新注册重新计时。日志：

- `[dev] IMS experiment delays SUBSCRIBE(mwi)`：开始等
- `[dev] IMS experiment sends the delayed SUBSCRIBE(mwi)`：等完照发
- `[dev] IMS experiment drops the delayed SUBSCRIBE(mwi)`：注册变了或服务停了，没发

上一版的开关仍然可用。改动的文件：`third_party/vowifi-go/internal/vowifi/imscore/` 下的 `dev_experiment.go`、`subscribe_mwi.go`，以及本目录的 `run-experiments.sh`。

## 三、构建、部署、回滚

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-delay-mwi
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-mwi'" \
  -o hideck-issue90 ./cmd/hideck
```

部署（需要 root，在构建目录里执行；`/root/hideck/hideck` 换成实际路径）。任何一步失败都会停下；备份文件名带时间，不会覆盖之前的备份：

```sh
BIN=/root/hideck/hideck; BAK="$BIN.before-mwi-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-issue90 && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-issue90 "$BIN" && systemctl start hideck
```

回滚：`systemctl stop hideck && cp <上面打印的 backup 路径> /root/hideck/hideck && systemctl start hideck`。`config/carrier_overrides.yaml` 里不要给 26202 加任何 override。

## 四、运行

**启动一次即可**：

```sh
sudo DEVICE=wwan0 sh issues/90-vodafone-de/run-experiments.sh
```

默认 6 轮，每轮 15 分钟，共约 1.5 小时：

| 轮 | 实验 | 要回答的问题 |
|---|---|---|
| 01 | `baseline` | 对照 |
| 02 | `delay-mwi` | RST 是跟着推迟后的 SUBSCRIBE(mwi) 出现，还是照旧在 port-s 打开后一分钟内出现（上一轮有 MWI 的 4 轮为 17–58 秒） |
| 03 | `no-mwi` | 复现上一轮的零 RST |
| 04–06 | 同 01–03 | 重复一遍，排除换节点的影响 |

- `DEVICE`、`EXPERIMENTS`、`MINUTES`、`OUTER_IFACE` 的用法和上一版相同。
- **短信**：请在 02、03、05、06 轮各申请一次验证码或发一条短信，在每轮开始 5 分钟后发（前 2 分钟是检查，`delay-mwi` 还要等 2 分钟），记下发送时间、是否收到、收到时间。port-s 不断时短信能不能进来，是这个 issue 最终要的答案。

## 五、回传

`summary.txt` 在上一版的字段之后新增：

- `mwi_sent`：SUBSCRIBE(mwi) 事务结束（收到最终响应或超时）的次数
- `mwi_to_reset_s`：`delay-mwi` 轮里每次发出推迟的 SUBSCRIBE(mwi) 到同一条隧道里下一次 RST 的秒数
- `mwi_delayed`、`mwi_delayed_sent`、`mwi_delayed_dropped`：`delay-mwi` 开始等、等完照发、因注册变了没发的次数（每轮结束停服务时丢掉的不算）。`warn` 里的 `mwi-sent-<a>-of-<b>` 表示照发了 b 次、实际只有 a 次 SUBSCRIBE(mwi)
- `sms_in`、`sms_in_at`：收到的 MESSAGE 条数和时间，用来对照你的短信记录

`delay-mwi` 轮里一次都没开始等，`valid` 为 `no`。

请把 **`summary.txt` 和短信记录直接贴出来**，整个输出目录打包作为附件。外层抓包里有你的公网 IP 和 ePDG 地址，不想公开就不要放进附件。打包前请再人工看一遍。

## 六、结果怎么看

| 看到的 | 说明 |
|---|---|
| `delay-mwi` 轮 `reset_lifetimes` 都在 120 秒以上，`mwi_to_reset_s` 稳定在几十秒内；`no-mwi` 轮再次零 RST | 支持"SUBSCRIBE(mwi) 触发 RST"，再看 RST 前后的报文找原因 |
| `delay-mwi` 轮 `mwi_delayed_sent=0`，RST 照旧在 port-s 打开后一分钟内出现 | 削弱"MWI 触发 RST"。若 `no-mwi` 轮仍零 RST，差别要从别处找，请贴出这两轮的日志 |
| `delay-mwi` 轮第一个 `reset_lifetimes` 在 120 秒以上，之后的都在一分钟内 | 第一次 MWI 之后的影响延续到了后面的注册，同样支持"MWI 触发 RST" |
| `delay-mwi` 轮发出了推迟的 MWI（`mwi_delayed_sent>0`）后 port-s 仍不断 | 只有紧跟注册的 MWI 会触发；推迟发送本身可能就是绕过办法，看这一轮短信是否收到 |
| `no-mwi` 轮这次也有 RST | 上一轮零 RST 是偶然，MWI 这条线放下 |
| `no-mwi` 轮零 RST 且短信收到 | 记下时间贴出来，这是最想要的结果 |

## 七、跑完后可选：测试版继续不订阅 MWI

脚本结束时会删掉自己的设置，测试版恢复成照常订阅 MWI。如果 `no-mwi` 轮零 RST、短信也能收到，可以让测试版继续不订阅 MWI，日常用一两天：

```sh
sudo mkdir -p /etc/systemd/system/hideck.service.d
printf '[Service]\nEnvironment="HIDECK_DEV90_EXPERIMENT=no-mwi"\n' | sudo tee /etc/systemd/system/hideck.service.d/issue90.conf
sudo systemctl daemon-reload && sudo systemctl restart hideck
```

- 只多打一些 `[dev]` 日志，SIP 内容已打码；不设 `HIDECK_DEV90_PCAP_DIR` 就不抓内层包。
- 期间收没收到短信、来电通不通、有没有掉线，记下时间和现象贴出来。
- 撤销：`sudo rm /etc/systemd/system/hideck.service.d/issue90.conf && sudo systemctl daemon-reload && sudo systemctl restart hideck`。
- 再跑 `run-experiments.sh` 会覆盖并在结束时删掉这个文件，之后需要的话重新执行上面三行。
- 这只是临时办法，正式修复会进正式版，届时换回正式版即可。
