# 动态流本地缓存 + 边界判定研究计划

> 状态：研究计划 + 实现
> 分支：`codex/research-2015-depth-probe`
> 基线：接续 `2026-06-09-2015-depth-probe.md` 的七轮探测结论
> 目标：用「抓一次存本地、之后零网络」的方式替代暴力探测，并据此**真正判定动态流重建边界**。

## 为什么要做这个

前一阶段（七轮 probe）已证明三件事，构成本计划的事实基础：

1. `feeds2_html_pav_all` 的 `begin_time/end_time` **被服务端忽略**（6 个年份窗口返回字节级相同响应）。时间窗口路线作废，唯一有效轴是 `offset`。
2. offset 空间**稀疏且非单调**：`3000` 空、`3100` 有、`3250` 空、`3400` 有。**不能二分、不能用「连续空页」当终止条件。**
3. 深 offset 会触发 **HTTP 501 短期风控**（非数据到底），冷却后恢复。今早一次猛跑把会话打进快速拒绝状态——这正是「跑太猛拖慢调试」的根因。

由此推出：要稳、要快、要能判定边界，必须把**网络抓取**和**数据分析**彻底解耦。

## 核心设计：原始响应缓存（cache-first harvester）

### 不变量

> **同一个 offset 的原始响应只从网络取一次，落盘后永久复用。**

因为 `begin_time/end_time` 无效、`count` 固定 100，一页响应**只由 offset 唯一决定**，且历史动态流内容不会再变。所以原始响应可以安全地长期缓存。

### 目录结构

```
oneclick/feed_cache/
  off_0000000.body      # offset=0 的原始 JSONP 响应（含 html:'...' 段）
  off_0000100.body      # offset=100
  off_0000200.body
  ...
  manifest.jsonl        # 每个已缓存 offset 一行：offset/body_size/parsed/min_max_time/fetched_at
```

- 文件名零填充到 7 位，保证字典序 = offset 升序，便于人工浏览。
- `.body` 存**未经处理的原始响应**（含 JSONP 包裹）。分析时现场 `processOldHTML` + `parseActivities`，这样解析器迭代时**不用重抓**。
- 空页（约 127 字节、无 `html:'`）也照常落盘——空页本身就是「这个 offset 确实没数据」的证据，是判定边界的关键信号。
- HTTP 501/错误**不落盘**，下次 harvest 自动重试该 offset。

### 抓取流程 `--harvest`

```
./QzoneExport-macOS --harvest                                  # 默认 offset 0..20000 step 100
./QzoneExport-macOS --harvest --offset-start 0 --offset-end 40000 --offset-step 100 --probe-delay-ms 5000
```

逐个 offset：
1. 若 `feed_cache/off_XXXXXXX.body` 已存在 → 跳过（命中缓存，**不发请求**）。
2. 否则发一次请求（复用 `probeFeedResultWithCooldown`，自带 501/429 冷却）。
3. 成功 → 写 `.body` + 追加 manifest 一行。失败 → 不落盘，记日志，继续下一个（冷却已在内部处理）。
4. `time.Sleep(delay)`，默认 **3000ms**（比原 probe 的 900ms 更温和）。

特性：
- **断点续传**：靠 `.body` 文件是否存在判断，随时 Ctrl-C、随时重跑，已抓的秒过。
- **细水长流**：delay 拉到 4000–6000ms，挂着跑几小时即可覆盖全网格；被风控就自动冷却。
- **不碰正式导出**：完全独立入口，正式 `QzoneExport` 流程零改动。

### 离线分析 `--analyze-cache`

```
./QzoneExport-macOS --analyze-cache
```

只读 `feed_cache/`，**零网络**：
1. 遍历全部 `.body`，`processOldHTML` + `parseActivities`，按 `activityDedupeKey` 去重。
2. 输出**边界报告**：
   - 全局最早可解析时间 `global_min_time`（**这就是动态流重建边界**）。
   - 最深「有数据」offset、最深「已缓存」offset。
   - 已缓存 offset 数、空页数、缺口（未缓存）offset 列表。
   - 尾部连续空页数。
3. 把全部去重后的 activity 导成一份 `cache_activities_<时间>.jsonl`，让现有 `--preview-year` / `--analyze-probe` 直接复用。
4. 直接调 `reconstructMoments → reconAsDeletedMsgs → buildHTML` 生成 `reconstructed_all_from_cache.html`（全年代、不限 2015）。

## 如何判定「真正的边界」

边界 = 在**完整覆盖**的 offset 网格上，最早的可解析活动时间。判定可信的充要条件：

1. **网格连续无缺口**：`0..N step 100` 每个 offset 都已成功缓存（无 501 缺口）。
2. **尾部足够多连续空页**：N 往后连续 ≥ M 页（建议 M=20，即 2000 个 offset）全是空页且无错误。

满足这两条时，`global_min_time` 就是 QQ 当前对该账号动态流暴露的**真实历史下界**，可以下定论。

> 注意：因为 offset 非单调，「中间有空页」不算到底；只有**尾部长串连续空页 + 全网格无缺口**才算。`--analyze-cache` 会显式报告这两个指标是否满足，避免我们误判。

## 阶段与验收

### 阶段 A：缓存基础设施（本次实现）
- [x] `feedCacheDir` / `cachePath(offset)` / `writeCache` / `readCache` / `cachedOffsets`。
- [x] `--harvest`：cache-first、断点续传、复用 cooldown、可配 delay/范围。
- [x] `--analyze-cache`：离线解析、去重、边界报告、导出 activities JSONL + 全量重建 HTML。
- [x] 单元测试：缓存读写、命中跳过、空页 vs 错误区分、边界报告（连续空页判定、缺口检测）。
- 验收：`go test ./...` 全绿；`go build` 通过；`--harvest`/`--analyze-cache` 入口不影响正式导出。✅ 已达成（gofmt/vet 干净）。

#### 实际命令（阶段 B 用户本机执行）

```bash
cd .worktrees/research-2015-depth-probe/oneclick
GOCACHE=/private/tmp/qzone-go-cache go build -o QzoneExport-macOS .

# 慢速抓取（细水长流，建议 delay 5s，挂着跑；可随时 Ctrl-C 续传）
./QzoneExport-macOS --harvest --offset-start 0 --offset-end 20000 --offset-step 100 --probe-delay-ms 5000

# 纯离线分析，零网络，出边界报告 + 全量重建 HTML
./QzoneExport-macOS --analyze-cache
```

要点：
- 已缓存的 offset **秒过不发请求**——调试解析器/重建逻辑时反复跑 `--analyze-cache` 不消耗任何配额。
- 被 501 风控时 harvest 自动冷却（5→10→20→30 分钟），无需守着。
- `--analyze-cache` 会明确告诉你边界是否「可判定」：**网格无缺口 + 尾部连续空页≥20** 才下结论；否则提示继续 `--harvest`。

### 阶段 B：用户本机跑一轮完整 harvest（用户执行）
- 用户扫码登录后挂 `--harvest --probe-delay-ms 5000` 慢跑，直到 manifest 覆盖 `0..20000` 无缺口、尾部连续空页足够。
- `--analyze-cache` 出边界报告。

### 阶段 C：定论或换接口
- 若边界稳定停在 2017-09 附近且尾部连续空页充分 → **判定 2017-09 为 feeds2 真实边界**，写入结论。
- 若仍有疑点（缺口未补全、或怀疑更深处有岛状数据）→ 扩大 `--offset-end` 继续 harvest，或进入下一研究：H5 `/mqzone/profile` 时间游标接口（见上一计划「下一步」）。

## 阶段 D：多数据源回忆找回（2026-06-10 实现，`recover.go`）

> 关键认知：说说动态流只是一口井。照片(相册)、留言板、日志是**独立存储**的内容仓库，
> 说说被删它们仍在，且大多带 2010–2015 真实时间戳——很可能直接绕过 2017 边界。

新增隐藏入口（缓存优先落盘 `recover_cache/`，复用 genGTK/JSONP/下原图/buildHTML，正式导出零改动）：

| 入口 | 接口 | 价值 |
| --- | --- | --- |
| `--diagnose` | `main_page_cgi?param=16` → SS/XC/RZ 计数 | **先判断 2017 是删除边界还是漏抓**：SS 计数 vs 现存说说数 |
| `--harvest-photos` | `fcg_list_album_v3` + `cgi_list_photo` | 最高价值：照片带 uploadtime/desc/原图，独立于说说 |
| `--harvest-board` | `m.qzone.qq.com/.../new/get_msgb` | 留言板，别人留的话+时间 |
| `--harvest-blogs` | `blognew/get_abs` | 日志标题+时间索引 |
| `--resolve-tid <tid>` | `emotion_cgi_msgdetail_v6` | 按 tid 还原单条说说全文（重建里捞到的老 tid 补全器） |
| `--recover-all` | 上面 diagnose+photos+board+blogs 依次 | 一键全跑 |

产物：`相册回忆_<QQ>.html`、`留言板_<QQ>.html`、`日志_<QQ>.html`（均已 `.gitignore`）。
测试：`recover_test.go` 覆盖 JSONP 剥离、上传时间解析、照片→卡片映射、计数解析（含 QQ 的 string/number 混用）。

执行（用户本机，harvest 不受影响）：
```bash
./QzoneExport-macOS --diagnose         # 先看账号到底有多少说说/相册/日志
./QzoneExport-macOS --recover-all      # 一键找回照片+留言板+日志
```
邮箱通道（无需工具，用户自行）：`mail.qq.com` 全文搜 `说说`/`评论`/`留言`/`赞`，按发件域 `qq.com` 筛——开过通知的话，邮件正文常引用说说原文。


## 风险与边界
- harvest 仍是真实网络请求，必须限速；默认 delay 提到 3s，深扫建议 5s+。
- 缓存 `.body` 含空间内容摘要与图片 URL，按隐私日志保管，已在 `.gitignore` 覆盖（`oneclick/feed_cache/` 需补充）。
- 真实抓取须用户本机执行（沙盒连不上 `ptlogin2.qq.com`）；Codex/本助手只负责实现与离线分析。
