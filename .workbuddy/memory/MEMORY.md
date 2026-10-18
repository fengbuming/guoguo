# 项目长期笔记

## 项目构成

`D:/AI/guoguo` 下有两个相关仓库：

| 目录 | 说明 | 端口 |
|---|---|---|
| `果果剧库/` | 红果**专用版**（只有 hongguo 一个源） | 8999 |
| `短剧库/短剧库/` | 多站源**完全版**（红果/黄果/黄豆三源），由 `短剧库.zip` 解压而来 | 8998 |

- 完全版与专用版 go.mod 现均为 **Go 1.24.1**，统一用 `C:/Go124/go/bin/go.exe` 构建（专用版已从 1.22 升版；`C:/Go/go/bin/go.exe` 已不适用）。
- 构建必须**从仓库根**执行：`go build -o dist/juku.exe .`（不能 build `./internal/app`，否则产出的是包归档而非可执行文件）。
- 构建环境：`GOTOOLCHAIN=local`、`GOPROXY=https://goproxy.cn`、`GOSUMDB=off`。

## 黄果 403（Cloudflare WAF）

- 新上游（2026-09-25 版）已自带修复：`network_media.go` 的 `mediaRequestHeaders()` 给黄果媒体请求打 `Sec-Fetch-Mode: cors` + `Sec-Fetch-Dest: empty`；`network_browser.go` 的 `huangguoBrowserHeaders()` 据此分支——非 navigate 时去掉 `upgrade-insecure-requests`/`sec-fetch-user` 导航标记，Cloudflare 放行。
- 为防「无 fetch-metadata 的server端请求仍走导航头」回退 403，额外在 `huangguoBrowserHeaders()` 加了 `isHuangguoResourcePath()` 路径回退（URL 命中媒体后缀/前缀时按 resource 处理）。该补丁是本地改动、无 git 跟踪，下次重新下载需再移植。
- 专用版（果果剧库）**不含** `network_browser.go`，红果专用、无黄果源，不存在此问题。

## 管理员密码（admin）

- 存储位置：各部署自己的 `data/accounts.json`（`短剧库/短剧库/data/` 与 `果果剧库/data/` 各有一份 `admin` 账户）。算法 `pbkdf2-sha256`，`iterations=600000`，盐 16 字节 hex，`passwordHash` 32 字节 hex。哈希函数即标准 PBKDF2-HMAC-SHA256（单块），可直接用 `hashlib.pbkdf2_hmac('sha256', pw, salt, 600000, 32)` 生成。
- CLI 的 `-admin-password` 只在**首次启动**写密码，对已有账户无效；改已有密码必须直接改 `accounts.json`。
- 改密码流程（关键）：**必须先停服务**（schtasks `JukuFull8998`/`JukuPro8999`），否则运行中进程会把内存里的旧密码写回文件覆盖改动 → 改 `admin` 记录的 `salt/passwordHash/iterations/algorithm` → 重启。原文件先 `.bak` 备份。
- 登录需 `X-Juku-Viewer` 头（CSRF 防护）：先 `GET /api/ui/viewer` 拿 `id`（无 cookie 首次返回 `ready:false` 不设 id，需带返回的 guest cookie 再请求一次才拿到 `id`），再以 `X-Juku-Viewer: <id>` + JSON 体 `{"username":"admin","password":"..."}` POST `/api/ui/account/login` 验证。
- `requirePasswordChange` 若为 true，登录后会被强制再改密码；如希望新密码直接可用，改为 false。

## 红果取流（2026-10-16：已攻破，全清晰度 + 全集免登录）

**结论：红果可匿名取到全部集（含付费/VIP 集）与 360p~1080p 全部清晰度，无需登录。** 此前「匿名仅试看集、上限 720p」的判断是错的，错在取址方式而非平台限制。

### 主链路（现网在用）：App 取址 `multi_video_model`

- **参考来源**：GitHub `sunnysky123/hongguo-web`（Java，纯代码六头签名；README FAQ 明确「签名不被接受 → HTTP 200 空 body」）。源码已在 `D:/AI/guoguo/_ref/`（AppSource.java / Client.java / PureSign.java / Spade.java）。用 `gh api` 拉取（**git clone 走代理会失败**，gh 可直连）。
- **两阶段**：
  1. `POST https://api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/`，body 是 `{"biz_param":{...},"mixed_video_id_map":{"1004":[vid]}}`（**不是** `video_id/content_type`）；返回 `data[vid].video_model.fallback_api`。
  2. `GET fallback_api`（**不带签名**，只带 UA）→ `video_info.data.video_list`（5 个清晰度）+ `key_seed`。
- **六个签名头**：`x-gorgon`（RC4 变体，sdk 常量 **67503104**，与老版 `0,6,11,28` 不同）、`x-khronos`、`x-ladon`=b64(be32(ts))、`x-argus`=b64(le32(ts))、`x-helios`（md5 派生 35 项表 + 34 轮 Feistel）、`x-medusa`（未实现，实测不需要）。另需 `x-ss-stub`(大写 md5)、`x-tt-request-tag`、`passport-sdk-version` 等。
- **query 必须 python 风格编码**：只保留 `A-Za-z0-9-_.~`，`*!'()` 与空格都转大写 `%XX`（`resolution=1280%2A2772`）。编码不一致 = 签名无效。版本用 **71332 / 7.1.3.32**（非 73532）。
- **片源 URL 用 key_seed 加密**：`hongguoDecryptSpadeURL` —— base64 解出后头须为 `a8 ?? 01 00`，`sha512(seed)` 再 `sha512(h1||64字节常量)` 取前 32 字节作 AES-128-CBC 的 key+IV，去 PKCS7 填充与尾随 0。CDN 域名 `*.qznovelvod.com`。
- **密钥**：`spade_a` 走 **App 减法变体**（tag="11"），复用既有 `hongguoContentKey`。
- **代码**：`internal/app/provider_hongguo_model.go`（取址+解密+选流）、`provider_hongguo_model_sign.go`（签名）。`resolveHongguoMedia` 中**置于首位**，play-info 降级为回退。
- **实测**（series 7687485258702261310）：ep1 / ep10 / ep56（末集）均取到 1080p，5 个清晰度候选，ffmpeg 解密播放成功。

### 回退链路：网页 play-info（仅试看集、上限 720p）

- `POST hongguoduanju.com/api/video/protected/play-info`，body `{"series_id","item_id","definition"}`，匿名无需签名；DASH 双轨 + `player_access_key`，密钥用**加法变体**（tag="22"）。
- 付费集是 **HTTP 403 + body `{"error":"protected_playback_unavailable"}`**（不是 200）。
- 代码 `internal/app/provider_hongguo_protected.go`。该路径 CDN 为 `v11-hgweb.qznovelvod.com`，**不校验 Referer**（各 Referer 均 206）。

### 已废弃

- 老 App `video_model/v1`（返回 200 空 body）已确认不可复活，保留仅为兜底。
- `markHongguoLockedChapters` / 「仅前 N 集可播」提示（基于 `accessible_episode_cnt`）**已全部删除**——付费集现在可播，不该再标锁定。`AccessibleEpisodes` 字段保留但前端不再展示。

### 续读 403（2026-10-16 修复）

- 症状：播放中途「媒体续读未返回所需字节范围（HTTP 403）」，关闭重开可恢复。
- 根因：`media_resume.go` 的 `resume()` 对 `status < 500` 直接放弃重试，403 只试 1 次即判死。而实测 CDN 的 403 是**按节点的偶发防盗链拒绝**（同一 URL 直连 206；带 `Referer: hongguoduanju.com` 时 `v96-reading-videocdn.qznovelvod.com` 返回 403），并发 8 次/串行 6 次 Range 均正常，非限流也非 URL 过期。
- 修复：新增 `retryableMediaStatus()` **白名单**（403/404/410/408/429/5xx 重试，200 与 400/401/416 不重试）；退避 250ms → 600ms。注意必须用白名单，用「排除法」会把 200 纳入重试从而破坏 `TestMediaProxyRejectsUnsafeContinuation`。

### 续读换 CDN 节点（2026-10-18，根治卡顿核心）

- 实测红果 CDN 单连接平均只稳定传 2–5MB 就被服务端关闭，且续读易粘在同一坏节点（实测单节点续读耗时中位 27s、最坏 173s）。根治：单节点连续失败 2 次（`mediaResumeAttemptsPerHost`）即切同清晰度备用节点（`providerMedia.Backups`，来自 `backup_url_1/2`），**换节点必须重置 ETag validator**（各节点 ETag 不同，不重置误报「资源已变化」）。备用节点优先于降清晰度。
- 前端兜底：`recoverFromStall` 用 `starved`（readyState<3 或 buffered<1s）判据，暂停→播放一次生效；播放页有刷新按钮（`reloadPlaybackBtn`/`reloadPlaybackVideoBtn` → `reloadPlayback()` 重建会话）。改前端必须升 `player.js`/`player-buffer.js` 的 `?v=` 版本号（裸引用会被浏览器缓存）。

### 播放静默挂起（2026-10-17 修复）

- 症状：手机局域网播放「看几集就卡住当前集、无任何提示」，返回重进恢复；日志里卡住时段**无任何 media 事件**。
- 根因：播放链路 `proxy.client`（`playback_gateway.go` / `download_hls.go`）复用 downloader Transport 但**无 `http.Client.Timeout`**，`ResponseHeaderTimeout=20s` 只管 header；CDN 连接 TCP 半开时 `body.Read()` 永久阻塞，续读机制无从触发。
- 修复：`stallGuardConn` —— 每次 `Read` 前 `SetReadDeadline(now+30s)`（`connReadStallTimeout`），stall 超阈值返回 `os.ErrDeadlineExceeded`，落入既有 `resume()` 续读。是**空闲超时**（有数据即顺延），不误伤慢速/长播放；对 H2 是连接级空闲检测。完全版挂在 `safeDNSDialer.dial()`（`network_dns_cdn.go`）；专用版无 safeDNSDialer，用 `network_stall_guard.go` 的 `newStallGuardDialContext()` 挂 `transport.DialContext`。
- 测试：两版各有 `network_stall_guard_test.go`（3 用例）。

## 红果探针

`D:/AI/guoguo/_hgtest/`：`main.go`（取址+解密+ffmpeg 验证，支持 `FIXTURE=1` 输出夹具、`VERIFY=1` 端到端）、`sign.go`（六头签名）、`crypto.go`（URL 解密）、`key.go`（密钥派生+ffmpeg）。可复用于后续复测。

## 本机局域网地址（动态）

- 2026-09-25 实测局域网 IPv4 = **192.168.1.6**（适配器「以太网 2」；此前为 .5，DHCP 可能变化）。手机访问用 `http://192.168.1.6:8998`（完全版）/ `:8999`（专用版）。部署后务必重新 `ipconfig` 确认当前 IP。
- 2026-10-15 19:54 复核：8998/8999 均监听 0.0.0.0，`http://192.168.1.6:8998/`、`http://192.168.1.6:8999/` 返回 200，IP 未变。

## 启动方式

- 本地单机的：加 `-listen 127.0.0.1:8998`。
- 局域网（手机/其他设备访问）：**不带** `-listen`，默认即 `0.0.0.0:8998`；加 `-open=false` 避免自动开浏览器。
- 必须以**后台任务方式**启动，否则进程随工具会话结束被回收。
- 日志会把 0.0.0.0 显示成 `http://127.0.0.1:8998`（`publicURL()` 的展示逻辑），不代表只监听本地。
- 自启任务（schtasks，登录时触发，动作 `"<exe>" -open=false`）：`JukuFull8998`（8998）、`JukuPro8999`（8999）。
  - **2026-09-27 起 `JukuFull8998`(8998) 自启已禁用**（任务保留，仅关闭自动触发）；`JukuPro8999`(8999) 仍开机自启。
  - 手动启动（禁用状态仍可手动运行）：`Start-ScheduledTask -TaskName JukuFull8998` 或 `schtasks /run /tn JukuFull8998`。
  - 直接启 exe：`"D:\AI\guoguo\短剧库\短剧库\dist\juku.exe" -open=false`。
  - 停止：`Stop-ScheduledTask -TaskName JukuFull8998`；重新启用自启：`Enable-ScheduledTask -TaskName JukuFull8998`。
  - 注意：Git Bash 直接调 `schtasks /end` 会把 `/end` 当路径报错，须用 PowerShell 的 `*-ScheduledTask` 或在 cmd 中执行。

## 本机环境要点

- `ipconfig` / `netsh` 输出为 **GBK**，需写文件后按 gbk 解码；Git Bash 的 `/tmp` 实际是 `E:/TEMP/AppData/Local/Temp`。
- PowerShell 的 `Get-NetFirewall*` / `Get-NetIPAddress` 在本环境返回空，改用 `netsh` + 文件 + python 解码。

## 既有测试失败（与改动无关）

- `TestEmbyExportPersistentSignedLinksAndSafeArchive`：断言 `Mode().Perm()==0o600`，NTFS ACL 不映射 POSIX 权限位。
- `TestEmbySyncAutomaticFollowedImportRefreshAndRestart`：Windows 下「sync settings not saved privately」（同类 ACL 语义）。
- `TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata`：上游断言全角冒号，但 `hongguoSearchKeyword` 用 `norm.NFKC` 把 `：` 归一化成 `:`，断言不可能成立。
- `TestManualMetadataPriorityValidation`：用 `&UIApp{}`（downloader 为 nil）调 `handleDramas`，`more=1&source=invalid` 未被拒（返回 200）→ 启动后台 `fetchAllDramas` → `d.cfg` 空指针 **panic**，会让整个 `internal/app` 测试包中断（无法跑完整套件）。跑全量时用 `-skip` 排除它。
- `TestHuangguoBrowserHTTP2CookiesAndNativeIsolation`：「cookie session reuse/reset failed at request 3」，环境相关。
- `TestLegacyAnonymousCatalogAndSessionReuse`：「anonymous session must be private」，同类 NTFS ACL 语义。
- `TestPlaybackAndDownloadResumeInterruptedMedia/native_HLS`：依赖外网，偶发 502 / 分片拉取失败。
- 以上均**不引用**红果取址相关符号，确认与取流改动无关。跑全量建议：
  `go test ./internal/app/ -skip 'TestManualMetadataPriorityValidation|TestEmbyExportPersistentSignedLinksAndSafeArchive|TestEmbySyncAutomaticFollowedImportRefreshAndRestart|TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata'`

## 测试构造要点

- 集成测试若走真实网络，必须用 `NewDownloader(cfg)` 而非 `&Downloader{cfg:...}`——后者 `limiter` 与 `client` 均为 nil，`doPreparedCatalogRequest` 会 panic。

## Git 仓库与推送（2026-10-16 建立）

- **仓库根：`D:/AI/guoguo`**（`git init` 于 2026-10-16 建立）。注意外层 `D:/AI` 也是一个 git 仓库（2 个提交、HEAD tree 为空、无 remote），但它**从未跟踪** `guoguo/`，两者互不干扰。
- 远程：`https://github.com/fengbuming/guoguo.git`，**私有**，默认分支 `master`（gh 建仓时按本地分支名）。一次仓库同时容纳 `短剧库/短剧库`（完全版）与 `果果剧库`（专用版）。
- **GitHub 账号是 `fengbuming`**（不是 mingbingfeng）；`gh` CLI 已认证，`gh repo create <name> --private --source=. --remote=origin --push` 可直接建仓。
- **推送必踩的坑**：git 全局配了 `http.proxy`/`https.proxy` = `http://127.0.0.1:10808`，该端口**没有服务监听**，git 一律连接失败；直连 GitHub 也不通。实际可用的代理是 **`127.0.0.1:7897`**（Windows 系统代理 `ProxyServer`，常驻监听）。推送命令：
  ```
  git -c http.proxy=http://127.0.0.1:7897 -c https.proxy=http://127.0.0.1:7897 push
  ```
  若嫌麻烦可改全局：`git config --global http.proxy http://127.0.0.1:7897`（同设 https.proxy）。`gh` 不受 git 代理影响，一直可用。
- **`.gitignore`**：仓库根一份，排除备份目录、所有 zip/ipa、`ipa_out/`、`尘·外/`、`guoapp_ios/`（自带独立 .git）、`__MACOSX/`、`_ipconfig.txt`、`_hgtest/*.{txt,json,bak}`（含时效 CDN 直链）。
  - 项目自带的 `短剧库/短剧库/.gitignore` 已排除 `/data/`、`/dist/`、`/短剧下载/`、`config.json`、`accounts.json` 所在目录与 ffmpeg 二进制。**专用版原本缺 `.gitignore`，已从完全版复制一份**——否则 `data/`（12M，含 admin 密码哈希）会被提交。
  - 源码内的 `internal/app/data/juku.json` 只是默认配置（下载目录/并发/代理），非敏感，正常入库。
- 首次提交：`fe086c8`，641 个文件。中文路径在 `git status` 中默认被转义成八进制，查看时用 `git -c core.quotepath=false status`。

## 路径与运行注意

- 两个仓库**无 git**（只有 `.gitignore`），改动无法用 git 回滚；改前先备份文件。`_backup_full_20260925/`、`_backup_pro_20260925/` 是 09-25 换源码时的整目录备份。
- 两仓库同源但不完全相同：专用版删掉了多源逻辑与 legacy 取流路径（`provider_media.go`、`playback_stream.go` 有差异，其余 provider 文件一致）。同步改动时**相同文件可直接 `cp`**，有差异的文件要定点改。
- **禁止跨版 cp 清单**（有专属差异，cp 会互相覆盖，2026-10-18 踩坑）：`internal/webui/index.html`（标题/品牌/vip-filter/端口文案）、`internal/app/provider_media.go`。改这两类文件必须两版各自定点改。
- **planOnly 语义不可动**：MSE/原始直连模式预取 = 只解析地址（`TestPlaybackPlanRefusalAndMetadataOnlyPrefetch` 钉死：resolved、0 字节传输）；native（手机）真缓存由 native 分支承担。勿改全模式真缓存。
- 完全版下载红果走 `provider_huangguo.go:downloadHuangguoProviderMediaWithProgress`；专用版走 `download_media.go:downloadMediaWithProgress`。
