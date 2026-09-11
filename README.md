<a name="readme-top"></a>

<div align="center">

<img src="docs/pictures/banner.png" alt="LitePan" width="100%">

<br>

<a href="https://www.litepan.top"><img src="https://img.shields.io/badge/官网文档-www.litepan.top-6C63FF?style=for-the-badge&labelColor=1B1B2F" alt="官网文档"></a>
&nbsp;
<a href="https://space.bilibili.com/1501989416"><img src="https://img.shields.io/badge/Bilibili-交流与演示-00A1D6?style=for-the-badge&logo=bilibili&logoColor=white&labelColor=1B1B2F" alt="Bilibili"></a>
&nbsp;
<a href="https://hub.docker.com/r/falconchen/litepan"><img src="https://img.shields.io/badge/Docker-falconchen%2Flitepan-2496ED?style=for-the-badge&logo=docker&logoColor=white&labelColor=1B1B2F" alt="Docker"></a>
&nbsp;
<a href="https://github.com/falconchen/LitePan/pkgs/container/litepan"><img src="https://img.shields.io/badge/GHCR-falconchen%2Flitepan-181717?style=for-the-badge&logo=github&logoColor=white&labelColor=1B1B2F" alt="GHCR"></a>


[![docker-pulls][docker-pulls-shield]][docker-url]
[![version][version-shield]][upstream-url]
[![license][license-shield]][license-url]

</div>

<br>

> [!IMPORTANT]
> **这是 [Ponphil/LitePan](https://github.com/Ponphil/LitePan) 的个人 Fork，不是上游仓库。**
>
> 为适配慢速且不稳定的上行链路，115 上传相关代码改动较大，并且**使用自行构建的
> 镜像**（`falconchen/litepan` 与 `ghcr.io/falconchen/litepan`），与上游镜像
> `ponphil/litepan` 不通用。具体差异见下方[「与上游的差异」](#-与上游的差异)。
>
> 遇到问题请先在本仓库提 issue，**不要拿本 Fork 的问题去打扰上游作者**。
> 功能介绍、官网文档与赞赏渠道仍指向上游，版权与许可同样归属上游作者。

> [!CAUTION]
> 上游仍是开发中的 **Go 版 LitePan**，本 Fork 在其之上继续修改，问题可能更多，
> 请谨慎测试、先备份 `data/`。
> Python 旧版已归档至 [LitePan-old](https://github.com/Ponphil/LitePan-old)。


<br>

## ▎ 功能简述

<table>
  <tr>
    <td width="50%" valign="top" align="center">
      <h3>多网盘聚合</h3>
      <p align="left">多账号统一管理，一个界面看完。</p>
      <img src="docs/pictures/feature-browser.png" alt="多网盘聚合" height="220">
    </td>
    <td width="50%" valign="top" align="center">
      <h3>跨盘秒传</h3>
      <p align="left">能秒传就秒传，否则自动上传。</p>
      <img src="docs/pictures/feature-crosstransfer.png" alt="跨盘秒传" height="220">
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top" align="center">
      <h3>STRM 直连播放</h3>
      <p align="left">生成 <code>.strm</code>，对接 Emby / Jellyfin。</p>
      <img src="docs/pictures/feature-strm.png" alt="STRM 直连播放" height="220">
    </td>
    <td width="50%" valign="top" align="center">
      <h3>STRM 刮削</h3>
      <p align="left">写 nfo / 海报，海报墙可追更。</p>
      <img src="docs/pictures/feature-strm-scrape.png" alt="STRM 刮削" height="220">
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top" align="center">
      <h3>目录整理</h3>
      <p align="left">TMDB 识别，预览后再归档。</p>
      <img src="docs/pictures/feature-organize.png" alt="目录整理" height="220">
    </td>
    <td width="50%" valign="top" align="center">
      <h3>自动联动</h3>
      <p align="left">整理、STRM、刮削、刷库串起来。</p>
      <img src="docs/pictures/feature-automation.png" alt="自动联动" height="220">
    </td>
  </tr>
</table>

## ▎ 挂载与更多功能

支持 WebDAV 与 FUSE 本地挂载，另有 302 直链、缓存保持、命名对齐、离线下载等能力。

---

## ▎ 与上游的差异

以上功能介绍来自上游。本 Fork 在其基础上做了下列改动，全部围绕
**115 网盘在慢速、易断链路上的上传可靠性**。

### 1. OSS 数据传输不再受 30 秒总超时约束

上游给 115 驱动的所有 HTTP 请求共用一个 `Timeout: 30s` 的 `http.Client`，
而 Go 的 `http.Client.Timeout` **覆盖整个请求、包括发送请求体**。上行约
1.2 MB/s 时，超过约 40 MB 的文件必然在传输途中被掐断，报错是：

```
Put "https://<bucket>.oss-cn-shenzhen.aliyuncs.com/...":
  context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

本 Fork 把 115 驱动拆成两个客户端：JSON API 仍用 30 秒总超时（请求小、本就该
快速返回），**OSS 数据传输改用不设总超时的客户端**，僵死连接由
`ResponseHeaderTimeout` 兜底，取消交给 `context`。传输时长本来就取决于文件
大小和链路速度，给它设总超时在慢链路上必然误杀。

### 2. 分片阈值与分片大小改为按账号可配置

上游是编译期常量（单片上限 512 MiB、分片 20 MiB）。本 Fork 在 115_Open 的账号
配置里新增两个字段，表单由 struct tag 自动生成：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `single_part_limit_mb` | 10 | 超过此大小走分片上传 |
| `upload_part_size_mb` | 5 | 单个分片大小 |

**断点粒度就等于分片大小**：链路一断，最多只丢掉当前这一片。慢速或不稳定的
上行适合调小，快速链路可以调大以减少往返。

### 3. 移除 cookie 版 115 驱动

只保留 `115_Open`（开放平台 OAuth）。后台驱动列表里不会再出现 cookie 版 115，
需要它的请用上游镜像。

### 4. 自动构建并推送镜像

`main` 分支更新时由 GitHub Actions 跑 `go vet` / `go test`，然后构建
**linux/amd64 与 linux/arm64** 双架构镜像并推送。PR 只构建不推送。

### 关于 115 分片上传的一个坑

分片发起时**必须带 OSS 顺序模式参数 `?sequential&uploads`**。115 的回调
`callbackBody` 里写死了 `sha1=${sha1}`，而普通分片合并出来的对象 OSS 算不出
整对象 SHA1（会回 `Sha1CheckNotSupport`），`${sha1}` 填不出来，115 就固定返回
`{"state":false,"message":"校验文件失败","code":10002}`。顺序模式下 OSS 会
计算整对象 SHA1，回调才能通过。

上游的 `ossInitiateMultipart` 本来就带了这个参数，本 Fork 未改动此处；
记在这里是因为它极易被忽略——实测客户端自己把 `${sha1}` 替换成正确的值**也没用**，
说明 115 并不信任回调里传来的值，而是自己去 OSS 核对。

---

## ▎ 快速开始

**Docker Compose 部署** · 本 Fork 的镜像由 CI 自动构建推送，两个仓库内容一致：

| 仓库 | 拉取地址 |
| --- | --- |
| Docker Hub | `falconchen/litepan` |
| GHCR | `ghcr.io/falconchen/litepan` |

可用标签：

| 标签 | 含义 |
| --- | --- |
| `latest` | `main` 分支最新构建 |
| `main-<sha>` | 对应某次提交，用于固定版本或回滚 |

生产环境建议钉在 `main-<sha>` 上，`latest` 会随 `main` 变动。

```yaml
services:
  litepan:
    image: falconchen/litepan:latest
    container_name: litepan
    restart: unless-stopped
    ports:
      - "5211:5211"
      # 内置 Magnet 的 TCP/uTP/DHT 监听端口；若在后台修改，需同步调整映射
      - "42069:42069/tcp"
      - "42069:42069/udp"
    environment:
      - TZ=Asia/Shanghai
    volumes:
      - ./data:/app/data
      - ./strm:/app/strm
      - ./mounts:/app/mounts:shared

      # 可选：将 FUSE 读缓存单独映射，建议放到更快的磁盘
      # - ./fuse_read_cache:/app/data/fuse_read_cache
    devices:
      - /dev/fuse:/dev/fuse
    pid: "host"
    privileged: true
    # 没有代理环境的，可以在下方配置tmdb的hosts
    # extra_hosts:
      # - "api.themoviedb.org:这里填写对应的ip"
      # - "image.tmdb.org:这里填写对应的ip"
    # 注意：也可以在程序内「目录整理 → TMDB 设置」填写反代主域名（自动补 /3 与 /t/p），与 hosts 二选一即可
```

打开 `http://你的IP:5211`，默认管理员密码均为admin。  
需要 FUSE 时请确保宿主机具备 `/dev/fuse` 权限。

> [!WARNING]
> **注意区分三个 `latest`：**
> - `falconchen/litepan:latest` —— 本 Fork 的 Go 版，就是上面 compose 用的。
> - `ponphil/litepan:latest` —— 上游的 **Python 旧版**，不要拿来部署 Go 版。
> - 上游 Go 版对应的是 `ponphil/litepan:beta`。
>
> 本 Fork 与上游镜像**不要混用**：数据目录格式虽然兼容，但驱动集合不同
> （本 Fork 去掉了 cookie 版 115），切回上游前请先确认没有账号依赖差异部分。
> 若你需要 Python 旧版程序与 Compose 脚本，请前往归档仓库：[LitePan-old](https://github.com/Ponphil/LitePan-old)。

## ▎ 支持

<table>
  <tr>
    <td width="50%" valign="top">
      <h3>支持 LitePan</h3>
      <p>LitePan 由 <a href="https://github.com/Ponphil/LitePan">Ponphil</a> 开发，本仓库只是个人 Fork。
         如果这个项目对你有帮助，请去<strong>上游仓库</strong>点 Star；下方赞赏码同样属于上游作者。</p>
      <img src="docs/pictures/wechat-tip.png" alt="微信赞赏" width="260">
    </td>
    <td width="50%" valign="top">
      <h3>赞助致谢</h3>
      <p>感谢每一位支持 LitePan 的朋友。</p>
      <p>完整致谢名单见官方网站：</p>
      <p>
        <a href="https://www.litepan.top/sponsor.html">https://www.litepan.top/sponsor.html</a>
      </p>
    </td>
  </tr>
</table>

## ▎ 反馈

**先判断问题属于哪一边：**

- **本 Fork 特有的问题**（115 上传超时、分片阈值/分片大小配置、镜像构建、
  缺少 cookie 版 115 驱动）→ 在 [falconchen/LitePan](https://github.com/falconchen/LitePan/issues) 提 issue。
- **上游本身的功能问题**（STRM、刮削、目录整理、其它网盘驱动等）→ 请到上游
  <a href="https://space.bilibili.com/1501989416">B 站主页</a> 交流。上游暂不接受公开 PR。

复现 Fork 的问题时请附上 `docker logs litepan` 中 `module=file_op` 的原文，
并注明用的是哪个镜像标签。

外部贡献致谢见 [ACKNOWLEDGEMENTS.md](./ACKNOWLEDGEMENTS.md)。

---

## ▎ 许可

[PolyForm Noncommercial 1.0.0](./LICENSE) — 个人学习与非商业使用，**禁止商用**。  
著作权归上游作者所有（`Required Notice: Copyright Ponphil (2026)`），本 Fork
及其发布的镜像沿用完全相同的许可与限制，**不因分发形式改变而放宽**。  
第三方依赖见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。请遵守各网盘服务条款与当地法规。

[docker-pulls-shield]: https://img.shields.io/docker/pulls/falconchen/litepan?logo=docker&logoColor=white&style=flat-square
[version-shield]: https://img.shields.io/badge/基于上游-v0.5.4--Beta-6C63FF?style=flat-square
[license-shield]: https://img.shields.io/badge/License-PolyForm%20NC-red?style=flat-square
[docker-url]: https://hub.docker.com/r/falconchen/litepan
[upstream-url]: https://github.com/Ponphil/LitePan
[license-url]: ./LICENSE
