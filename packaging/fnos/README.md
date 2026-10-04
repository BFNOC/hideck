# 飞牛 fnOS 应用包

本目录生成供飞牛应用中心手动安装测试的 `.fpk`，内部复用 `yibaiba/hideck:<版本>` Docker 镜像，不是第二套电话/VoWiFi 实现。尚未完成 fnOS 真机安装、升级、USB 热插拔和通话验收，也不代表已上架飞牛应用中心。

实现依据：[官方 Docker 应用案例](https://developer.fnnas.com/docs/examples/docker/)、[环境变量](https://developer.fnnas.com/docs/core-concepts/environment-variables/)、[应用入口](https://developer.fnnas.com/docs/core-concepts/app-entry/)、[fnpack](https://developer.fnnas.com/docs/cli/fnpack/)。

## 安装条件

- 飞牛 Docker 应用已安装、已启动，能够拉取对应版本镜像；FPK 不包含离线镜像。
- 镜像发布支持 Linux amd64、arm64；FPK 本身不包含平台二进制，声明 `platform=all`。不据此声明支持 32 位 ARM 或其他架构。
- USB 模组接入 NAS；虚拟机中的 fnOS 还需要把物理 USB 设备直通给虚拟机。
- 宿主机需要对应的 USB 串口/QMI/MBIM 驱动。模组直拨另需 `snd_usb_audio`；ADB、ALSA 用户态工具和已适配模组语音资源需要由包含这些改动的镜像提供，FPK 不会为旧镜像补装。
- 不要同时运行另一套 HiDeck 或其他程序控制同一模组。安装不会停止、替换原有部署或迁移原有数据。

容器沿用主项目的 `network_mode: host`、`privileged: true`、`/dev:/dev`，这意味着广泛的宿主机设备访问。安装向导要求明确确认。安装脚本不安装宿主机软件包、不改内核或设备权限，不自动开启 ADB、切换 RF/通话模式或重启模组。

宿主机生命周期脚本使用飞牛包用户；容器内仍是特权运行，两者不是同一层权限。若飞牛调用生命周期脚本时无法访问 Docker，状态检查会明确报错，不把权限错误伪装成“未运行”，也不自动授予 Docker 用户组权限。

## 打包与安装

先从[官方 fnpack 下载页](https://developer.fnnas.com/docs/cli/fnpack/)取得适合开发机的工具。使用 Python 3.9+，不需要额外 Python 依赖：

```sh
python3 packaging/fnos/build.py --version 2.1.23 \
  --fnpack /absolute/path/to/fnpack --output /tmp/hideck-fnos-output
```

输出 `hideck_2.1.23_fnos.fpk` 和对应 SHA256 校验文件。版本必须对应已发布镜像；本地打包不隐式拉取镜像，不能把“打包通过”当成镜像或真机验收。需要调整管理端口可增加 `--http-port 17575`，会同步更新 manifest 和桌面入口；容器读取飞牛注入的 `TRIM_SERVICE_PORT`，不使用 bridge 端口映射。

**`2.1.23` 仅是已发布镜像的打包示例，不包含最近的模组直拨及依赖完善。** 验收这些新功能前，需要先发布包含当前改动的新版本 Docker 镜像，再以该版本生成 FPK；生成 FPK 不会编译或发布当前工作区的应用代码。

也可手动运行 GitHub Actions 的 **Build fnOS package**：先确认目标镜像包含 amd64/arm64，再打包并上传工作流产物，不会发布 Release 或部署设备。

1. 在与包相同的目录核对 `sha256sum -c hideck_2.1.23_fnos.fpk.sha256`。
2. 在飞牛应用中心选择手动安装此包，选择应用存储位置并确认权限提示。
3. 从桌面打开 HiDeck；默认管理地址为 `http://NAS_IP:7575`，初始账号为 `admin / admin`，首次登录立即修改密码。
4. 在 HiDeck 中检查设备后，再明确选择需要的通话模式。安装包本身不改变原有运营商兼容策略。

桌面入口使用独立标签页，默认仅管理员可见。这只是飞牛入口可见性，不是 HiDeck 鉴权；知道地址的客户端仍能访问 HiDeck 登录页。不要将管理端口无保护地暴露到互联网。

## 数据与升级

| 内容 | 飞牛目录 | 容器目录 |
| --- | --- | --- |
| 配置、通知渠道凭证和绑定 | `${TRIM_PKGETC}` | `/app/config` |
| 短信数据库、录音、证书、ADB 状态 | `${TRIM_PKGVAR}/data` | `/app/data` |
| 应用日志 | `${TRIM_PKGVAR}/logs` | `/app/logs` |

使用飞牛提供的实际存储路径，不假定 `/vol1`，也不写进升级时替换的 `TRIM_APPDEST`。不声明公开共享目录，避免把验证码和 Bot 凭证暴露给其他 NAS 用户。

首次安装仅在配置不存在时复制主项目模板。重新启动、安装回调和升级回调不会覆盖已有配置、通知绑定或数据库；卸载钩子仅交还平台处理，不主动删除数据。**这不保证飞牛卸载流程一定保留应用目录**：卸载、迁移或回退前，先停止应用并备份上述三个目录，按飞牛卸载界面的实际选项处理。回退镜像不能替代数据库备份。

每个 FPK 固定引用指定版本镜像，不跟随 `latest` 自动更新。通过新版 FPK 升级；不要同时让外部容器自动更新工具改动此项目。容器名为 `hideck-fnos`，由飞牛 `docker-project` 负责启停。

## HTTPS、IPv6 与 WebRTC

- 管理 HTTP 默认 `7575/TCP`；内置 HTTPS 默认 `7576/TCP`，只有配置启用后才监听；WebRTC 媒体默认 `7580/UDP`。
- 使用 host 网络，不额外关闭 IPv6。实际 IPv4/IPv6 可达性仍取决于 NAS、路由器和防火墙；不是“有 HTTPS 就有音频”。
- 飞牛桌面或 FN Connect 的 HTTPS 不会自动使另一个 HTTP 端口成为受信任页面，也不能保证转发 HiDeck 的 UDP 媒体。
- 双向麦克风使用受信任的 HiDeck HTTPS 页面。配置方法见 [HTTPS 与 WebRTC](../../docs/https-webrtc.md)；使用本地 CA 时客户端仍需正确安装信任。
- 飞牛可能占用或重定向 80/443；不要让 Caddy/Nginx 抢占系统端口。先查看[官方端口说明](https://help.fnnas.com/articles/v1/settings/port-customization)，可选独立 HTTPS 端口，并同步配置媒体可达性。

PC/SC 读卡器还需要宿主机 pcscd 与 socket 共享，见 [Docker PC/SC 说明](../../DOCKERHUB.md#pcsc-smart-card-readers)。基础 FPK 不替宿主机安装 pcscd，也不默认挂载不存在的 socket。

## 验证范围

本地测试覆盖包结构、端口一致性、显式权限确认、首次配置初始化、升级保留数据，以及运行/未运行/同名冲突/Docker 失败状态。遵循飞牛生命周期约定，`status=0` 只表示本项目容器正在运行，不宣称业务就绪；`/ping` 健康检查独立保留在 Docker 中，避免首次健康探测尚未执行时把冷启动误判成失败。容器启动和停止交给飞牛管理。

真机验收仍需验证：应用中心安装与存储目录、包用户 Docker 查询权限、启停和升级、USB 重枚举、多设备、真实短信，以及 HTTPS 下的双向通话。宿主机缺少驱动时应先解决驱动问题，不能靠换包、强装其他内核模块或自动重启模组掩盖。
