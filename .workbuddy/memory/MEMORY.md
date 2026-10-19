# 项目长期笔记（精简版 2026-10-19）

## 项目构成
- `短剧库/短剧库/` = 完全版（红果/黄果/黄豆三源），端口 **8998**；`果果剧库/` = 红果专用版，端口 **8999**。两仓库均无 git（仅 .gitignore），改前先备份。
- 构建（必须仓库根）：`C:/Go124/go/bin/go.exe build -o dist/juku.exe .`（Go 1.24.1；env `GOTOOLCHAIN=local GOPROXY=https://goproxy.cn GOSUMDB=off`）。
- **跨版禁止 cp** `internal/webui/index.html`、`internal/app/provider_media.go`；其余相同文件可直接 cp。下载入口也不同（完全版 `provider_huangguo.go`，专用版 `download_media.go`）。
- Git：仓库根 `D:/AI/guoguo`，remote `github.com/fengbuming/guoguo`（私有，默认 master）。账号 `fengbuming`。推送必带代理：`git -c http.proxy=http://127.0.0.1:7897 -c https.proxy=http://127.0.0.1:7897 push`（全局配的 10808 无效）。gh 可用。中文路径用 `git -c core.quotepath=false status`。

## admin 密码
- 存各自 `data/accounts.json`：pbkdf2-sha256 / 600000 轮 / 盐 16B hex / hash 32B hex。`-admin-password` 仅首次启动有效。改已有账户**必须先停服务**再改文件（否则运行中进程回写覆盖），先 .bak。
- 登录：`GET /api/ui/viewer`（无 cookie 首次无 id，带 guest cookie 再请求一次）→ `X-Juku-Viewer: <id>` + JSON POST `/api/ui/account/login`。

## 红果取流（2026-10-16 攻破：全清晰度 + 全集免登录）
- App `multi_video_model`：POST `api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/`，body `{"biz_param":{...},"mixed_video_id_map":{"1004":[vid]}}` → `data[vid].video_model.fallback_api`；再 GET（无签名只带 UA）→ video_list + key_seed。
- 六签名头 x-gorgon(RC4 变体，sdk 常量 **67503104**)/x-khronos/x-ladon/x-argus/x-helios/x-medusa(不需要)。query 必须 python 风格编码（`*!'()` 与空格转大写 %XX）；版本 71332 / 7.1.3.32。
- URL 解密 `hongguoDecryptSpadeURL`：base64 后头须 `a8 ?? 01 00`，sha512(seed)→sha512(h1||64B常量)取前 32B 作 AES-128-CBC key+IV；spade_a 走减法变体 tag="11"。
- 代码 `provider_hongguo_model.go` + `_sign.go`（置于 resolveHongguoMedia 首位）；回退 `provider_hongguo_protected.go`（play-info，仅试看集/720p）。参考 Java 在 `_ref/`，探针在 `_hgtest/`。

## 播放链路修复（2026-10-16~18）
- 续读重试**白名单** `retryableMediaStatus()`（403/404/410/408/429/5xx 重试；200/400/401/416 不重试）。勿用排除法（会破坏 TestMediaProxyRejectsUnsafeContinuation）。
- 红果 CDN 单连接只稳传 2–5MB：单节点连败 2 次切 backup_url_1/2，**换节点必须重置 ETag validator**。前端 recoverFromStall + reloadPlayback 兜底，改前端必升 `?v=`。
- 静默挂起：proxy client 无 Timeout → body.Read 永久阻塞。修复 `stallGuardConn`（Read 前 SetReadDeadline(now+30s) 空闲超时 → ErrDeadlineExceeded → resume()）。完全版挂 `safeDNSDialer.dial()`；专用版用 `network_stall_guard.go` 挂 DialContext。
- planOnly 语义不可动（预取只解析地址、0 字节传输）。

## 剧库磁盘缓存（2026-10-19，分支 perf/library-disk-cache）
- 内存只留精简索引 `dramaEntry`（376B，完整 Drama 720B）；完整记录按 ID 的 FNV-1a 哈希分 **256 片**存 `data/library/records/NNN.json`，索引 `data/library/index.json`。LRU 上限 16 片，脏分片淘汰时就地落盘。
- 热路径（列表/搜索/排序/统计/封面代理/排行榜/推荐合并）只读索引，零磁盘 IO；索引含 desc/intro（前端本地搜索要用）与单份封面。
- 保真要点：`drama()` 在有完整记录时以记录为准回填封面与集数（否则 DeepEqual 测试失败）；`applyEntry` **不要**写 Cover/EpisodeCount；`canonicalDramaCover` 无封面时保持 nil 别转成 ""。
- 旧整文件 `library.json` 首次启动自动迁移，之后不再解析（不删，留作回滚）。`readLibraryCache` 改为优先读分片剧库；CLI 刷新会同步写回分片。
- 实测（27,722 部）：索引常驻 51.5 MiB vs 全量 74.2 MiB；进程空闲 RSS **177.8 → 136.4 MB**；`/api/ui/dramas` 0.24s→0.20s（载荷 50.7→48.1MB，略快）。峰值仍高（整库 JSON 一次性编码，Go 不立刻还内存）。

## 启动方式 / 环境
- 局域网：**不带** -listen（默认 0.0.0.0）+ `-open=false`，必须后台启动。日志显示 127.0.0.1 是 publicURL() 展示逻辑。
- schtasks：`JukuFull8998`（8998，自启已禁用可手动 run）、`JukuPro8999`（8999 开机自启）；用 PowerShell `*-ScheduledTask`。LAN IP 2026-10-15 仍 192.168.1.6。
- ipconfig/netsh 输出 GBK（写文件后 python 解码）；PowerShell 的 Get-NetFirewall*/Get-NetIPAddress 本机返回空。

## 测试
- 既有失败（与改动无关）：TestManualMetadataPriorityValidation（downloader nil → panic 中断整包，**跑全量必须 -skip**）、TestEmbyExportPersistentSignedLinksAndSafeArchive、TestEmbySyncAutomaticFollowedImportRefreshAndRestart、TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata、TestHuangguoBrowserHTTP2CookiesAndNativeIsolation、TestLegacyAnonymousCatalogAndSessionReuse。
- 真实网络集成测试必须 `NewDownloader(cfg)`，不能用 `&Downloader{cfg:...}`（limiter/client nil 会 panic）。

## VPS 适配实测（2026-10-19 Windows，8998 完全版）
- 空闲 RSS 106–186MB；每路 MSE +1–3MB；CPU ≈0.4% 单核 / 每 10Mbit/s 代理吞吐。
- Native HLS：整集一个 ffmpeg（RSS ~142MB），135s 1080p ~11s CPU（`-c copy`）；占 "video" 额度（默认 1）。
- 播放额度 `data/playback-settings.json`：会话 4、媒体连接 8、remux 2、音/视频转码各 1、预备 1、封装缓存 512MB；超限 429；会话空闲 10 分钟回收。
- 1080p 竖屏 4.25Mbit/s → 1.53GB/观看小时；30Mbps ≈5–6 路；1024GB/月 ≈670 观看小时。
- 结论：2 核富余、1GiB 可用（+2GB swap，首选裸二进制 + systemd，Docker 在 1GiB 偏紧）、40GB 宽裕。
- 灌流复现：viewer GET 两次拿 id → POST `/api/ui/playback/open` → GET `/api/ui/playback/stream?session=&episode=N&start=0&quality=0&version=0&remux=0`。
