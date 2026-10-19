# 项目长期笔记（2026-10-19 精简重写）

## 项目构成
- `短剧库/短剧库/` = 完全版（红果/黄果/黄豆三源），端口 **8998**；`果果剧库/` = 红果专用版，端口 **8999**。两仓库均无 git（仅 .gitignore），改前先备份。
- 构建：`C:/Go124/go/bin/go.exe build -o dist/juku.exe .`（必须从仓库根；Go 1.24.1；env `GOTOOLCHAIN=local GOPROXY=https://goproxy.cn GOSUMDB=off`）。

## admin 密码
- 存各自 `data/accounts.json`：pbkdf2-sha256，600000 轮，盐 16B hex，hash 32B hex（`hashlib.pbkdf2_hmac('sha256',pw,salt,600000,32)` 可直接生成）。`-admin-password` 仅首次启动有效；改已有账户必须**先停服务**再改文件（否则运行中进程把内存旧值回写覆盖），先 .bak。
- 登录：`GET /api/ui/viewer`（无 cookie 首次不返回 id，带返回的 guest cookie 再请求一次）→ `X-Juku-Viewer: <id>` + JSON POST `/api/ui/account/login`。

## 红果取流（2026-10-16 攻破：全清晰度 + 全集免登录）
- 主链路 App `multi_video_model`：POST `api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/`，body `{"biz_param":{...},"mixed_video_id_map":{"1004":[vid]}}` → `data[vid].video_model.fallback_api`；再 GET fallback_api（无签名只带 UA）→ video_list（5 清晰度）+ key_seed。
- 六签名头：x-gorgon（RC4 变体，sdk 常量 **67503104**）/x-khronos/x-ladon/x-argus/x-helios/x-medusa（实测不需要）。query 必须 python 风格编码（`*!'()` 与空格都转大写 %XX，如 `resolution=1280%2A2772`）；版本 71332 / 7.1.3.32。
- URL 解密 `hongguoDecryptSpadeURL`：base64 解出头须 `a8 ?? 01 00`，sha512(seed) 再 sha512(h1||64B 常量) 取前 32B 作 AES-128-CBC key+IV；密钥 spade_a 走减法变体 tag="11"。
- 代码：`provider_hongguo_model.go` + `provider_hongguo_model_sign.go`（置于 resolveHongguoMedia 首位）；回退 `provider_hongguo_protected.go`（网页 play-info，仅试看集/720p，付费集 403 protected_playback_unavailable）。参考 Java 实现在 `_ref/`，探针在 `_hgtest/`。
- 已废弃：老 App video_model/v1（200 空 body）；「仅前 N 集可播」锁定提示已全删。

## 播放链路修复（2026-10-16~18）
- 续读重试**白名单** `retryableMediaStatus()`（403/404/410/408/429/5xx 重试；200/400/401/416 不重试），退避 250→600ms。勿用排除法（会把 200 纳入重试，破坏 TestMediaProxyRejectsUnsafeContinuation）。
- 红果 CDN 单连接只稳传 2–5MB：单节点连败 2 次（mediaResumeAttemptsPerHost）切 backup_url_1/2 备用节点，**换节点必须重置 ETag validator**（各节点 ETag 不同）。前端 recoverFromStall + reloadPlayback 兜底；改前端必须升 player.js/player-buffer.js 的 `?v=`。
- 静默挂起根因：proxy client 无 http.Client.Timeout，body.Read 永久阻塞。修复 stallGuardConn：每次 Read 前 SetReadDeadline(now+30s)（connReadStallTimeout）空闲超时 → os.ErrDeadlineExceeded → 落入 resume()。完全版挂 `safeDNSDialer.dial()`；专用版无 safeDNSDialer，用 network_stall_guard.go 挂 transport.DialContext。两版各有 network_stall_guard_test.go。

## 启动方式
- 局域网：**不带** -listen（默认 0.0.0.0:8998）+ `-open=false`，必须后台启动（否则随会话回收）。日志显示 127.0.0.1 是 publicURL() 展示逻辑，非实际监听。
- schtasks：`JukuFull8998`（8998，自启已禁用、可手动 run）、`JukuPro8999`（8999，开机自启）。停止/启用以 PowerShell `*-ScheduledTask`（Git Bash 调 schtasks /end 会把 /end 当路径）。
- LAN IP 2026-10-15 仍为 192.168.1.6；部署后重新 ipconfig 确认。

## 本机环境要点
- ipconfig/netsh 输出为 GBK（写文件后 python 解码）；PowerShell 的 Get-NetFirewall*/Get-NetIPAddress 在本机返回空。

## 测试
- 既有失败（与取流改动无关）：TestManualMetadataPriorityValidation（downloader nil → panic 中断整包，跑全量必须 -skip）、TestEmbyExportPersistentSignedLinksAndSafeArchive、TestEmbySyncAutomaticFollowedImportRefreshAndRestart、TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata、TestHuangguoBrowserHTTP2CookiesAndNativeIsolation、TestLegacyAnonymousCatalogAndSessionReuse（后两者 NTFS ACL/环境语义）。
- 跑全量：`go test ./internal/app/ -skip 'TestManualMetadataPriorityValidation|TestEmbyExportPersistentSignedLinksAndSafeArchive|TestEmbySyncAutomaticFollowedImportRefreshAndRestart|TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata'`
- 真实网络集成测试必须 `NewDownloader(cfg)`，不能用 `&Downloader{cfg:...}`（limiter/client 均 nil 会 panic）。

## Git（仓库根 D:/AI/guoguo，remote github.com/fengbuming/guoguo 私有，默认 master）
- 推送必带代理（git 全局代理 10808 无效、直连不通）：`git -c http.proxy=http://127.0.0.1:7897 -c https.proxy=http://127.0.0.1:7897 push`。gh CLI 可用；账号是 fengbuming（不是 mingbingfeng）。
- 根 .gitignore 排除备份目录、zip/ipa、ipa_out/、尘·外/、guoapp_ios/、_hgtest 时效直链；两版各自 .gitignore 排除 data/（含 admin 哈希）、dist/、短剧下载/、config.json。中文路径查看用 `git -c core.quotepath=false status`。

## VPS 适配实测（2026-10-19，8998 完全版 Windows 实测）

- 空闲 RSS 106–186MB（library.json 50MB/27,722 部全量在内存）；每路 MSE 播放 +1–3MB；CPU ≈0.4% 单核 / 每 10Mbit/s 代理吞吐（单流 78.5MB/34.7s，CPU +0.23s）。
- Native HLS（手机）：整集一个 ffmpeg 进程连续封装（RSS ~142MB），135s 1080p ~11s CPU（`-c copy`，非真转码）；占 "video" 额度（默认 1）。分片命中缓存后 ms 级。
- 播放额度（`data/playback-settings.json`，管理员可改 1–64）：会话 4、媒体连接 8、remux 2、音/视频转码各 1、预备 1、封装缓存 512MB。超限 429「同时播放数量已达上限」；会话空闲 10 分钟回收。
- 1080p 竖屏 4.25Mbit/s（147.6s/78.5MB）→ 1.53GB/观看小时；30Mbps ≈ 5–6 路；1024GB/月 ≈ 670 观看小时。个人/家庭宽裕；多人分享需压画质、关预缓存（预备设 0）或开 302 直连。
- 部署结论：2 核富余、1GiB 可用（建议 +2GB swap；Docker 在 1GiB 偏紧，首选裸二进制 + systemd + `apt install ffmpeg`，或程序自动下载静态 ffmpeg）、40GB 宽裕。仓库自带 Dockerfile/compose（bookworm-slim，UID 1000）。
- 灌流复现：viewer GET 两次拿 id → POST `/api/ui/playback/open` {dramaId} 拿 session → GET `/api/ui/playback/stream?session=&episode=N&start=0&quality=0&version=0&remux=0`（服务端忽略 Range 上限直接给整集）；native：POST `/api/ui/playback/hls/open` 再取 index.m3u8 + segment.ts。

## 跨版同步纪律
- **禁止跨版 cp**：`internal/webui/index.html`、`internal/app/provider_media.go`（两版有专属差异，2026-10-18 踩坑），其余相同文件可直接 cp。
- planOnly 语义不可动：MSE/原始直连预取只解析地址、0 字节传输（TestPlaybackPlanRefusalAndMetadataOnlyPrefetch 钉死）。
- 下载入口不同：完全版 `provider_huangguo.go:downloadHuangguoProviderMediaWithProgress`；专用版 `download_media.go:downloadMediaWithProgress`。
