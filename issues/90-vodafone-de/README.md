# hideck#90 Vodafone DE 测试分支说明

本分支只用于收集 Vodafone DE（262/02，预设 `vodafone_de_26202`）刷新注册被 403 的数据，并试两处修复。它不是正式版本，测试完请换回正式版本。

## 一、相对 main 改了什么

行为改动三处，第 1、2 条对所有运营商生效：

1. **刷新收到 403 时不再换模板重试。** TS 24.229 §5.1.1.4.1 规定，重新注册收到 403 时，UE 要做初始注册，不能再发一次重新注册。之前报告里 cseq=5 的那次 403 就是这个重试产生的。
2. **刷新收到 403 后，在现有隧道里做初始注册。** 丢掉当前 SA，用新端口发初始 REGISTER，不拆 IKE 隧道。之前的做法是整条隧道重连，换新的内层 IP，然后才注册。在隧道里做是我们的选择，规范只要求做初始注册。隧道内注册也失败时，仍然走原来的整条隧道重连。
3. **只对 Vodafone DE：port-s 被 RST 后先不发恢复 REGISTER，最多等 3 分钟。**
   - 依据：TS 33.203 §7.1 NOTE 6 和 TS 24.229 §4.2A NOTE 2 允许 P-CSCF 关掉到 UE port-s 的连接，等有请求时再连回来。这只在没有 NAT、没有 TLS、没有用多注册机制时成立，所以协商了 outbound 时不等待。
   - 3 分钟内 P-CSCF 重新连上，就不发 REGISTER；没有重连，就照原来的方式发 REGISTER。等待期间如果有别的 REGISTER 先成功了，会改走原有的 port-s 验证流程，那时恢复 REGISTER 可能早于 3 分钟。
   - **等待期间，首页连接链路上的 "SMS RX" 会变红，显示"失败"；WiFi 通话健康页显示"恢复中"，中断次数会增加。** 这是预期的，不是新问题。Vodafone DE 会不会为了送短信重新连上 port-s，目前没有任何证据。如果它不会，这 3 分钟里发来的短信可能收不到，也可能等下次注册后才补送。

刷新收到 408、500、504 时，§5.1.1.4.1 同样要求做初始注册，但现有代码会保留旧注册。这个问题跟本次无关，本分支不改。

另外加了一批 `[dev]` 前缀的日志，见第四节。日志走 Info 级别，不需要打开 `server.debug`。

改动的文件：

- `third_party/vowifi-go/internal/vowifi/imscore/register.go`、`register_failure.go`：第 1、2 条
- `third_party/vowifi-go/internal/vowifi/imscore/port_s_vodafone_de.go`、`register_transport.go`、`sms_readiness.go`：第 3 条
- `third_party/vowifi-go/internal/vowifi/imscore/dev_trace.go`，以及 `port_s_session.go`、`transport_runtime.go`、`service_trigger.go`、`runtimecore/ims_config.go` 里调用它的地方：`[dev]` 日志
- `register_flow_trace.go`：200 OK 的 flow negotiation 日志增加 `authentication_info`、`nextnonce`、`flow_timer`
- 每处改动都有对应的测试，文件名以 `_test.go` 结尾

## 二、隐私

`[dev]` 日志里的 SIP 报文在写入前都经过 `devRedactSIP`（`dev_trace.go`）打码：

- 折行先展开，再把 Authorization、Proxy-Authorization 整个头遮掉（冒号前有空格也能识别）
- 所有 `sip:`、`sips:`、`tel:` URI 的用户部分遮掉（IMPI、IMPU、号码，不管带不带分隔符）
- 地址头里的显示名遮掉（带不带引号都遮），reginfo 里的 `<display-name>` 也遮掉
- Security-Client、Security-Server、Security-Verify 不打码：里面只有 SPI、端口和算法，没有身份信息，保留下来才能看出 SA 有没有变
- 8 位以上的连续数字遮掉
- `imei:` 后面的整串遮掉（`+sip.instance`、Path）
- `i-wlan-node-id`（AP 的 MAC）遮掉
- MESSAGE 只记头部，不记正文，短信内容不会进日志
- IKE 和 Contact 里的 IMEI 只记"是否一致"，不记数值

日志里仍会出现：内层 IPv6 地址、P-CSCF 地址、Call-ID、WWW-Authenticate 里的 nonce（AKA 挑战，不是身份信息）。贴到 issue 之前请再人工看一遍。不需要抓包。

## 三、构建、部署、回滚

构建（需要 Go 和 npm，跟正式版本的构建方式一样，只是不压缩）：

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-port-s-await
git checkout FETCH_HEAD
git log -1 --oneline   # 应为 "docs(issues): add the hideck#90 Vodafone DE test branch guide"
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90'" \
  -o hideck-issue90 ./cmd/hideck
```

部署：`deploy-binary.sh` 会把二进制装到项目目录下的 `hideck`（OpenWrt 上是 `/usr/bin/hideck`），以实际路径为准。下面用 `/root/hideck/hideck` 举例：

```sh
systemctl stop hideck
cp /root/hideck/hideck /root/hideck/hideck.v2.1.38.bak
install -m 755 hideck-issue90 /root/hideck/hideck
systemctl start hideck
```

回滚：把 `.bak` 拷回去，再重启服务。

配置：

- 不需要 carrier override。如果 `config/carrier_overrides.yaml` 里还留着上次给 26202 加的 `refresh_offers_new_security_client`，请删掉，这样只有本分支的改动这一个变量。
- `server.debug` 保持原样即可。

## 四、运行和回传

正常使用即可，**至少连续运行 2.5 小时**。不需要专门发短信或打电话。期间正好收到短信或来电、或者发现有短信没收到，记下大概时间就行。如果这段时间一条短信都没进来，"P-CSCF 会不会自己连回 port-s"这一项就没有结论，这没关系，其他几项照样能看。

回传时请附上构建用的 commit（`git rev-parse --short HEAD`）、测试的起止时间和时区，以及这段时间内的这些日志：

```sh
grep -E '\[dev\]|IMS (REGISTER|port-s|refresh|re-REGISTER|reset|registered|registration|protected|peer|P-CSCF|SMS readiness|immediate)|ims_reconnect' <日志文件>
```

日志不确定在哪的话，用之前取日志的方式就行（例如 `journalctl -u hideck --since ...`）。

## 五、结果怎么看

`[dev] IMS REGISTER attempt context` 里的 `trigger` 字段表示这次注册是谁触发的。同一次注册内的每个尝试（例如换传输方式重试）都带同一个 trigger：

- `port-s flow failed`：port-s 恢复
- `re-REGISTER rejected: forbidden`：刷新被 403 之后的隧道内初始注册
- `scheduled`：到期的周期刷新或首次注册

下表里的字段只是示意。实际日志中字段顺序不同、值可能带引号，用 grep 匹配时只搜字段值即可。

| 日志里看到的 | 说明 |
|---|---|
| `[dev] IMS port-s reset; deferring the port-s recovery REGISTER` 之后 3 分钟内出现 `IMS port-s lifecycle event=opened` | P-CSCF 会自己重连 port-s。紧接着的 `[dev] IMS inbound request` 会显示它送来了什么 |
| 3 分钟后 `trigger=port-s flow failed kind=refresh` 的响应是 200 | 支持"403 只在 RST 后马上刷新时出现"这个判断 |
| 同上，响应仍然是 403 | 说明刷新延后 3 分钟也会被拒。但不能就此断定与时机完全无关，要再看下面的周期刷新 |
| `trigger=scheduled kind=refresh` 的结果 | 到期的周期刷新是 200 还是 403。不一定会出现：如果 port-s 恢复的 REGISTER 一直成功，周期刷新的时间会被不断往后推 |
| `IMS registered with new protected ports cause=forbidden` | 隧道内的初始注册成功了，没有拆隧道 |
| `IMS registration with new protected ports failed` 之后出现 `ims_reconnect` | 隧道内的初始注册也失败了，退回到整条隧道重连 |
| `trigger=port-s flow failed` 出现在 RST 后不到 3 分钟 | 等待期间有别的 REGISTER 先成功了，但 port-s 30 秒内没有回来（会有 `IMS port-s did not reopen after REGISTER`） |
| `IMS REGISTER listed multiple Contacts` | S-CSCF 上留下了之前注册的 Contact |
| `[dev] IMS REGISTER outbound` 的 `changed_headers` | 同一个 Call-ID 里，这次 REGISTER 和上一次相比改了哪些头。比较用的是打码前的原文，只输出头名，所以 Authorization 变了也能看出来 |
| `[dev] IMS device identity` | IKE 和 Contact 里的 IMEI、TAC 是否一致，以及 User-Agent |
