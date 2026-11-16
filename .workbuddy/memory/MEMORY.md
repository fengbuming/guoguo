# 项目长期笔记（精简 2026-11-07）

## 构成与构建
- `短剧库/短剧库/` 完全版（红果/黄果/黄豆，8998）；`果果剧库/` 红果专用版（8999）。均无 git，改前备份。
- 构建（仓库根）：`C:/Go124/go/bin/go.exe build -o dist/juku.exe .`（Go 1.24.1，env `GOTOOLCHAIN=local GOPROXY=https://goproxy.cn GOSUMDB=off`）。
- **跨版禁止 cp**：`internal/webui/index.html`、`internal/app/provider_media.go`；下载入口不同（完全版 `provider_huangguo.go` / 专用版 `download_media.go`）。前端改动必升 `?v=`。
- Git 根 `D:/AI/guoguo`，remote `github.com/fengbuming/guoguo`（私有 master），推送必带 `-c http.proxy=http://127.0.0.1:7897`（https 同）。

## 账号 / 登录
- `data/accounts.json`：pbkdf2-sha256 / 600000 轮 / 盐16B hex / hash32B hex。改已有账户**先停服**再改 + .bak；改密码优先 `POST /api/ui/account/password`（自动吊销旧会话，免停服）。
- 登录：`GET /api/ui/viewer`（匿名首次无 id，带 guest cookie 再请求）→ `X-Juku-Viewer: <id>` + POST `/api/ui/account/login`。管理端点 POST 前须重新取 viewer（旧 id 会 409），且必须带该头。
- 必须登录/关闭注册：`POST /api/ui/admin/settings {requireLogin,allowRegistration}`，持久化；入口「用户/账号」页。遗留放行面 `/assets/**`、`/api/emby/*`。无后门，登录限速 10 次/分钟/用户名。

## 红果取流
- `multi_video_model` POST `api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/` → `data[vid].video_model.fallback_api` → GET 得 video_list+key_seed。六签名头（x-gorgon RC4 变体，sdk 常量 67503104 等），query 需 python 风格编码，版本 71332。
- URL 解密 `hongguoDecryptSpadeURL`（sha512(seed) 派生 AES-128-CBC key+IV；spade_a 减法变体）。代码 `provider_hongguo_model.go`+`_sign.go`，回退 `provider_hongguo_protected.go`；参考 `_ref/`，探针 `_hgtest/`。

## 播放链路要点
- 续读重试白名单 `retryableMediaStatus()`（403/404/410/408/429/5xx 重试；200/400/401/416 不重试），勿改排除法（破坏 TestMediaProxyRejectsUnsafeContinuation）。
- 红果 CDN 单连接只稳传 2–5MB：连败 2 次切 backup_url_1/2，**换节点必须重置 ETag validator**。前端 recoverFromStall + reloadPlayback 兜底。
- 静默挂起修复 `stallGuardConn`（读前 SetReadDeadline +30s；完全版挂 `safeDNSDialer.dial()`，专用版 `network_stall_guard.go` 挂 DialContext）。planOnly 语义不可动。
- `data/playback-settings.json` 的 `enabled` 是总开关（默认 false → legacy libx264 转码）；本机/线上均已置 true。`directSources` 实测无效，置空。
- 红果 CENC 探测不出编码 → 已改 `CENCKey` 非空跳过探测直接 remux（解密 + `-c copy`），CPU ~350→0.08 核秒/集；首帧需等整集封装。
- **新建 `hlsProxy` 后必须调 `attachPlaybackAudioTrack(&media, proxy)`**（preparePlaybackMedia / generatePlaybackWithParent / probePlaybackCodecs 三处）。红果受保护源是 DASH 双轨，漏掉就只有视频轨 → 无声且浏览器禁用音量控件（c323825 修）。
- 清单请求为首片阻塞最多 6s，别退回空 EVENT 清单。remux 首分片即返回；`jobCtx` 不能挂 plan ctx；`cacheMB` 是**磁盘**配额。

## 剧库磁盘缓存 / 内存
- 索引 `dramaEntry` 常驻 ~19.7MB；完整记录按 FNV-1a 分 256 片存 `data/library/records/NNN.json`，索引 `index.json`，LRU 16 片。旧 `library.json` 首启自动迁移（不删，回滚用）。
- 保真：`drama()` 以完整记录回填封面集数；`applyEntry` 不写 Cover/EpisodeCount；`canonicalDramaCover` 无封面保持 nil；intro==desc 时索引不重复存。
- 列表**必须流式写**（复用 Encoder + 8KB 分块），已磁盘快照化 `data/library/snapshot/<key>.json.gz`（ETag/304，保留 6 份）。整库 27.4MB→7.09MB，命中 0.024s。回落：非 GET/HEAD、不收 gzip、entries nil、IO 异常。封面缓存 8MiB/256 条。
- `configureProcessMemory()`：GOMEMLIMIT=可用45%、≤4GiB 时 GOGC=50；Linux 必须**进程启动前**设 `GODEBUG=madvdontneed=1`（systemd unit 已加），否则 RSS 不降。Windows 只看 private。
- 测量脚本 `.workbuddy/tmp/mem_probe.py`、`single_probe.py`、`gzip_check.py`、`vps_*.py`（用 `envs/default/Scripts/python.exe` 才有 paramiko）。

## 启动 / 环境 / 测试
- 局域网：不带 `-listen`（默认 0.0.0.0）+ `-open=false`，必须后台启动。schtasks：`JukuFull8998`、`JukuPro8999`。ipconfig/netsh 输出 GBK。
- 全量测试必须 `-skip` 既有失败：TestManualMetadataPriorityValidation（panic 中断整包）、TestEmbyExportPersistentSignedLinksAndSafeArchive、TestEmbySyncAutomaticFollowedImportRefreshAndRestart、TestHongguoSuggestionsPreserveNamesWithoutFetchingDramaMetadata、TestHuangguoBrowserHTTP2CookiesAndNativeIsolation、TestLegacyAnonymousCatalogAndSessionReuse。真实网络集成测试必须 `NewDownloader(cfg)`。

## VPS（111.91.25.138 / Debian12 / 2C 800MB / 40GB）
- 裸二进制 + systemd：`/opt/juku/bin/juku`、`/opt/juku/data`、`/etc/systemd/system/juku.service`（ExecStart `-listen 0.0.0.0:8998 -open=false -data-dir /opt/juku/data -ffmpeg /usr/bin/ffmpeg`，Environment=GODEBUG=madvdontneed=1）。**公网未开放**，SSH 隧道 `ssh -L 8998:127.0.0.1:8998 root@...`。
- 编译改 ALWAYS 用 `git worktree` 检干净副本 + `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 -trimpath -ldflags="-s -w"`。admin 仅首启 `timeout 40 ./bin/juku -admin-password xxx`，别写进 unit。
- 迁移只搬 `libraryCache` 对齐的数据；**不迁移** accounts.json / viewer-key / juku.json / ui-state.json / huangguo-session.json。备份 `/opt/juku/backup/<ts>/`，指针 LAST。
- 容量：播放期 RSS 可达 477MB，800MB 机器建议 cacheMB 128–192、maxSessions 2、maxRemuxJobs 1；`vm.swappiness` 设 10（0 会让 swap 失效）。
- 播放额度默认：会话4 / 媒体连接8 / remux2 / 音、视频转码各1 / 预备1 / cacheMB 512；超限 429；会话空闲 10 分钟回收。线上现 enabled:true、maxRemuxJobs=2、cacheMB=128、directSources 空。
- 检索参数是 `q` 不是 keyword，响应 `{data,limited,query,saved,source,total,warning}`。灌流复现：viewer GET×2 拿 id → POST `/api/ui/playback/open` → GET `/api/ui/playback/stream?...`。
- 同机还有 nginx 1.22.1 + Hysteria2（443/80 default_server）、LE IP 证书；无 ufw/iptables。paramiko `sftp.put` 偶发 ENOENT，改系统文件用 heredoc；`ss` 显示 `*:8998`。

## 播放会话限制（2026-11-07 已修复并上线）
- 旧版 `MaxSessions=4` 全局计数 + 后台标签心跳每 20s 无限续期 → 单用户被自己的残留会话锁死（429），VPS 连续运行 17 天不重启会持续累积。
- 修复：`maxSessions=0` = 不限制；新开播放自动回收本 viewer 的闲置/最旧会话；管理员 `POST /api/ui/admin/playback/cleanup` 一键清空；设置面板与播放器 429 错误处均有清理按钮。日志事件 `playback.session_reclaimed` / `sessions_cleared`。
- 源码备份 `短剧库/短剧库/_backup_playback_20261107/`；线上备份 `/opt/juku/backup/20261107-173933/`（LAST）；本机 `dist/juku.exe` 已换（旧版 `dist/juku.exe.bak-20261107`）。线上仍保留 maxSessions=4，彻底关闭需在面板填 0。
- 提交 `8b52011`（perf/library-disk-cache，已推送）；线上二进制为从该提交 `git worktree` 干净检出的产物，sha `53c38d08…`（`dist/juku-linux-8b52011`）。

## 红果搜索（2026-11-16，7e553c6）
- 根因①：搜索页 2026-11-13 改 CSR（X-Modernjs-Render: client），HTML 无 routerData → 综合搜索必失败。改走匿名 JSON `GET hongguoduanju.com/api/search/result?aid=8662&query=&page=`（≤3 页），旧 HTML 解析兜底。
- 根因②：联想新形态——部分剧在 `word_type=title`、video_data 空、编号在 `keyword`。名称匹配改白名单（short_play_name/title/album）+ 编号（video_data.series_id 或 keyword，≥6 位纯数字）；common_query 哈希/actor_name 拒收。
- **新 JSON 解码必须 UseNumber**：19 位编号 float64 舍入 → 相邻季 ID 撞值被去重吞掉（丢 25/32 季的坑）。线上现返回字符串属防御。
- 两版同步 `provider_hongguo_search.go`；线上二进制 sha `e2815b17…`（`dist/juku-linux-7e553c6`），备份 `/opt/juku/backup/20261116-191505/`。VPS 匿名搜索 401（requireLogin）→ 用 Linux 探针二进制直测通过。
- 教训：勿对整个 internal/app 跑 gofmt -w（重排 + autocrlf 幻影 M）；幻影 M 用 `git add` 内容相同归一。8999 专用版仅本机更新，未上 VPS。

## 黄果AI 取址（2026-11-16，e0eaa83）
- 站点 2026-11-13 改版：`videoInitialData` 的 videoSrc/epPlaySrcs 变空，旧 `GET /api/videos/{id}/play` 返回空 `video_url`；现行 `POST /api/videos/{id}/episodes/{ep}/playback`、body `{}` → `data.streams.main` m3u8（AES-128，key/ts 为绝对签名地址，无需 CDN 头）。明文匿名即可，`dx150-edge-v1` 加密通道不必实现；每次播放都要重新 POST。
- 完全版 `provider_huangguoai_playback.go` 优先新取址、失败回退旧页面解析；集号取 PageURL `/ep-N/` 或 CurrentEpisode；域名候选 配置→providerHosts→huangguoai.com→huangguoai.ai→章节页 host。**专用版无黄果AI provider，勿改**。
- 本机 `data/juku.json` 已配 Clash `http://127.0.0.1:7897/`（本机直连 huangguoai.com 超时）；VPS 出口直连可用。
- 全量测试需额外 skip `TestEmbeddedInterfaceServesModulesWithoutExternalBuild`（前端 import 带 `?v=` 与测试正则不匹配，既有失败）。
- 线上二进制 sha `676941af…`（`dist/juku-linux-e0eaa83`），备份 `/opt/juku/backup/20261116-100158/`。
