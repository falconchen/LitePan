<a name="readme-top"></a>

<div align="center">

<img src="docs/pictures/banner.png" alt="LitePan" width="100%">

<br>

<a href="https://www.litepan.top"><img src="https://img.shields.io/badge/官网文档-www.litepan.top-6C63FF?style=for-the-badge&labelColor=1B1B2F" alt="官网文档"></a>
&nbsp;
<a href="https://space.bilibili.com/1501989416"><img src="https://img.shields.io/badge/Bilibili-交流与演示-00A1D6?style=for-the-badge&logo=bilibili&logoColor=white&labelColor=1B1B2F" alt="Bilibili"></a>
&nbsp;
<a href="https://github.com/falconchen/LitePan/pkgs/container/litepan"><img src="https://img.shields.io/badge/GHCR-falconchen%2Flitepan-2496ED?style=for-the-badge&logo=docker&logoColor=white&labelColor=1B1B2F" alt="GHCR"></a>


[![build][build-shield]][build-url]
[![platforms][platforms-shield]][ghcr-url]
[![docker-pulls][docker-pulls-shield]][dockerhub-url]
[![license][license-shield]][license-url]

</div>

<br>

> [!CAUTION]
> 当前仓库是正在开发中的 **Go 版 LitePan**，首次发布可能问题较多，请谨慎测试。
> Python 旧版已归档至 [LitePan-old](https://github.com/Ponphil/LitePan-old)。

> [!NOTE]
> **本仓库是 [Ponphil/LitePan](https://github.com/Ponphil/LitePan) 的 fork**，
> 在上游基础上做了改动并自行构建发布镜像（`ghcr.io/falconchen/litepan`、
> `falconchen/litepan`），与上游发布的 `ponphil/litepan` 是两套镜像。
>
> 软件遵循 [PolyForm Noncommercial License 1.0.0](./LICENSE)，**仅限非商业用途**。
>
> Required Notice: Copyright Ponphil (2026)


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

## ▎ 快速开始

**Docker Compose 部署** · 镜像由本仓库 CI 构建，同时支持 `amd64` 与 `arm64`

```yaml
services:
  litepan:
    # latest 跟随 main 变动；生产环境建议钉到 main-<sha>，见下文
    image: ghcr.io/falconchen/litepan:latest
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
> **不要用 `ponphil/litepan:latest` 部署本仓库对应的 Go 版。**  
> `latest` 仍是 Python 旧版镜像。若你需要旧版程序与 Compose 脚本，请前往归档仓库：[LitePan-old](https://github.com/Ponphil/LitePan-old)。

### 镜像与标签

镜像由 GitHub Actions 自动构建，`main` 每次更新即触发，同时提供
**`linux/amd64` 与 `linux/arm64`**（两个架构分别在各自的原生 runner 上
构建，再合并成一个 manifest list，拉取时自动选对架构）。

推送到两个 registry，内容完全一致，任选其一：

```
ghcr.io/falconchen/litepan     # 公开，拉取无需登录
falconchen/litepan             # Docker Hub
```

标签有两种：

| 标签 | 说明 |
| --- | --- |
| `latest` | 始终跟随 `main` 最新提交，会随之变动 |
| `main-<短 sha>` | 钉在某次提交上，内容不变 |

**生产环境建议用 `main-<sha>` 而不是 `latest`**，避免下次 `docker compose pull`
时被动升级到未验证的版本。

```yaml
services:
  litepan:
    image: ghcr.io/falconchen/litepan:main-1e241ef
```

确认某个标签包含哪些架构：

```bash
docker manifest inspect ghcr.io/falconchen/litepan:latest \
  | jq -r '.manifests[].platform
           | select(.architecture != "unknown")
           | "\(.os)/\(.architecture)"'
# linux/amd64
# linux/arm64
```

> 这里过滤掉 `unknown` 是因为 buildx 会额外附带一条 attestation 记录，
> 它的 platform 显示为 `unknown/unknown`，并不是可运行的架构。

## ▎ 支持

<table>
  <tr>
    <td width="50%" valign="top">
      <h3>支持 LitePan</h3>
      <p>如果这个项目对你有帮助，欢迎点右上角 <strong>Star</strong>，也欢迎自愿赞赏。</p>
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

交流请到 <a href="https://space.bilibili.com/1501989416">B 站主页</a>。  
暂不接受公开 PR；有维护意愿请私信。
外部贡献致谢见 [ACKNOWLEDGEMENTS.md](./ACKNOWLEDGEMENTS.md)。

---

## ▎ 许可

[PolyForm Noncommercial 1.0.0](./LICENSE) — 个人学习与非商业使用，**禁止商用**。  
第三方依赖见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。请遵守各网盘服务条款与当地法规。

[build-shield]: https://img.shields.io/github/actions/workflow/status/falconchen/LitePan/docker-image.yml?branch=main&label=build&logo=githubactions&logoColor=white&style=flat-square
[build-url]: https://github.com/falconchen/LitePan/actions/workflows/docker-image.yml
[platforms-shield]: https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2496ED?style=flat-square
[ghcr-url]: https://github.com/falconchen/LitePan/pkgs/container/litepan
[docker-pulls-shield]: https://img.shields.io/docker/pulls/falconchen/litepan?logo=docker&logoColor=white&style=flat-square
[dockerhub-url]: https://hub.docker.com/r/falconchen/litepan
[license-shield]: https://img.shields.io/badge/License-PolyForm%20NC-red?style=flat-square
[license-url]: ./LICENSE
