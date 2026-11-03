# hideck#90 Vodafone DE 测试分支说明（MWI 正式方案）

本分支是 hideck#97 里维护者定下的正式方案，基于当前 main，不含之前的实验开关和抓包脚本。请用它替换之前的测试版，日常使用一段时间，确认 Vodafone DE（262/02）在默认设置下 port-s 不再被反复重置、短信能正常收到。测试完换回正式版即可。

## 一、改了什么

- 运营商预设：`vodafone_de_26202` 默认不发 SUBSCRIBE(mwi)，其他运营商不变。
- 设备页卡策略新增「语音信箱提示」：自动（默认，按运营商预设）、开启、关闭，按 SIM 保存。
- 网络明确回 405/489 时记住这张卡不支持 MWI，重启、重连都不清除；480、超时不记。选「开启」会清除记录重新检测。

Vodafone DE 在「自动」下就不会发 MWI，不用改任何设置。

## 二、构建、部署、回滚

```sh
git fetch https://github.com/BFNOC/hideck.git test-vodafone-de-mwi-policy
git checkout FETCH_HEAD
git log -1 --oneline   # 应为本说明文件所在的提交
make frontend-dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "with_utls nomsgpack" \
  -ldflags "-X 'github.com/yibaiba/hideck/internal/global.Version=v2.1.38-issue90-mwi-policy'" \
  -o hideck-issue90 ./cmd/hideck
```

部署前**先删掉之前 `no-mwi` 的 systemd 设置**，这一轮要测的是默认行为，不能再靠环境变量：

```sh
sudo rm -f /etc/systemd/system/hideck.service.d/issue90.conf && sudo systemctl daemon-reload
```

部署（需要 root，在构建目录里执行；`/root/hideck/hideck` 换成实际路径）。任何一步失败都会停下；备份文件名带时间，不会覆盖之前的备份：

```sh
BIN=/root/hideck/hideck; BAK="$BIN.before-mwi-policy-$(date +%Y%m%d-%H%M%S)"; \
test -x hideck-issue90 && cp "$BIN" "$BAK" && echo "backup: $BAK" && \
systemctl stop hideck && install -m 755 hideck-issue90 "$BIN" && systemctl start hideck
```

回滚：`systemctl stop hideck && cp <上面打印的 backup 路径> /root/hideck/hideck && systemctl start hideck`。新版本会在数据库里加一列和一张表，旧版本会忽略它们，回滚不用处理数据库。`config/carrier_overrides.yaml` 里不要给 26202 加任何 override。

## 三、运行

不用跑脚本，正常使用即可，建议至少一两天。「语音信箱提示」保持「自动」，不要改成「开启」：开启后会重新发 MWI，port-s 预计又会被重置。

启动后先确认生效：

```sh
journalctl -u hideck --since "10 min ago" | grep 'SUBSCRIBE(mwi)'
```

每次注册后应看到一条 `IMS SUBSCRIBE(mwi) skipped`，`reason` 为 `carrier policy disables MWI subscription`，`preset` 为 `vodafone_de_26202`，不应出现 `SUBSCRIBE(mwi) starting`。

## 四、回传

跑了一段时间后，把下面几条命令的输出贴出来（`--since` 换成你部署的时间）：

```sh
S="2026-11-04 00:00"
journalctl -u hideck --since "$S" | grep -c 'SUBSCRIBE(mwi) skipped'
journalctl -u hideck --since "$S" | grep -c 'SUBSCRIBE(mwi) starting'
journalctl -u hideck --since "$S" | grep 'port-s lifecycle' | grep -c '"closed"'
journalctl -u hideck --since "$S" | grep 'port-s lifecycle' | grep -c 'peer_reset'
journalctl -u hideck --since "$S" | grep -c 'IMS REGISTER succeeded'
```

再附上短信记录：每次申请验证码或别人发短信的时间、是否收到、收到时间。来电、掉线有异常也记下时间。

贴之前请检查一遍，号码、IMSI、IMEI、ICCID 都要打码。

## 五、结果怎么看

| 看到的 | 说明 |
|---|---|
| `starting` 为 0，`peer_reset` 很少或为 0，短信都能收到 | 正式方案有效，可以提 PR |
| `starting` 不为 0 | 预设没生效，请贴出 `SUBSCRIBE(mwi)` 相关的日志行 |
| `starting` 为 0，但 `peer_reset` 仍然频繁 | 还有别的原因，请贴出 RST 前后几分钟的日志 |
| port-s 不断，但短信仍有收不到的 | 记下时间贴出来，可能是 #90 里另外几个问题 |
