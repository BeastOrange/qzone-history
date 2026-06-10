# QQ 空间导出现状与探查参数报告

> 生成日期：2026-06-09  
> 当前重点版本：`oneclick` v1.3  
> 目的：记录当前项目现状、使用的技术、关键接口参数，以及后续深度探查动态/互动记录时需要优先验证的假设。

## 结论先行

当前真正可用、面向普通人的程序是 `oneclick/` 下的零依赖 Go 单文件工具。它通过扫码登录获取 QQ 空间 cookie，抓取现存说说，再扫描动态流，尝试重建疑似已删除的说说，最后生成单文件离线 HTML。

目前能影响“抓到多远年代”的核心不是图片下载，而是动态流接口 `feeds2_html_pav_all` 的分页与服务端返回边界。当前 `oneclick` v1.3 已经从“连续空页停止”升级为“读取 `total_number` 并按 `offset/count` 扫描”，但仍受 `total_number`、`maxPages`、服务端深 offset 行为、以及 `begin_time/end_time` 是否有效影响。

如果要继续探索 2017-09-03 之前的互动记录，建议先做实验模式，而不是直接改正式导出逻辑。

## 项目组成

仓库里有两套相互独立的程序。

### 主程序

根目录 Go module：`qzone-history`。

主程序使用 Clean Architecture：

- `cmd/main.go`：入口和依赖装配。
- `internal/domain`：实体、仓储接口、用例接口。
- `internal/usecase`：重建、导出等业务逻辑实现。
- `internal/infrastructure/qzone_api`：QQ 空间接口访问。
- `pkg/database`：SQLite/GORM 持久化。
- `pkg/utils`：令牌、HTML 处理等工具。

主程序仍保留原项目的研究性质，适合参考架构和旧动态流探查逻辑，不是当前普通用户的主要入口。

### oneclick 工具

目录：`oneclick/`。独立 Go module：`qzone`。

特点：

- 单文件主程序：`oneclick/main.go`。
- 仅使用 Go 标准库，无第三方依赖。
- 普通用户双击运行，扫码登录。
- 生成 `QQ空间_<QQ>.html`。
- 生成详细日志 `QzoneExport_log_<时间>.txt`。
- 保存 `cookies.json` 用于下次复用登录态。

当前版本常量：

```go
const version = "1.3"
```

位置：`oneclick/main.go`。

## 当前技术栈

主程序：

- Go
- GORM
- SQLite
- goquery
- Viper
- progressbar

`oneclick`：

- Go 标准库
- `net/http` 请求 QQ 接口
- `http.CookieJar` 处理扫码登录阶段 cookie
- 正则解析 JSONP/HTML 片段
- `encoding/base64` 内嵌图片
- `html`/字符串拼接生成自包含网页

## 登录与凭证

### 二维码登录

`oneclick` 使用 QQ 二维码登录流程：

1. 请求 `ptqrshow` 获取二维码。
2. 从 cookie 中读取 `qrsig`。
3. 使用 `qrsig` 生成 `ptqrtoken`。
4. 轮询 `ptqrlogin`。
5. 登录成功后访问 `check_sig` 换取 QQ 空间 cookie。

关键参数：

- `qrsig`：二维码登录阶段 cookie。
- `ptqrtoken`：由 `qrsig` 计算得出。
- `uin`：QQ 号 cookie，常见格式为 `o0...`，需要去掉前缀。
- `p_skey` / `skey`：生成 `g_tk` 的关键凭证。
- `g_tk`：QQ 空间接口认证参数。

### cookies.json 复用

登录成功后，`oneclick` 会在程序同目录写入：

```text
cookies.json
```

它包含 QQ、cookie 和 `g_tk`。v1.3 启动时会读取该文件并校验登录态；网络抖动时会重试，避免把临时失败误判为 cookie 过期。

安全边界：

- `cookies.json` 等同登录凭证。
- 不要上传、分享或提交到仓库。
- 用完可以删除，下次重新扫码。

## 现存说说抓取

现存说说使用接口：

```text
emotion_cgi_msglist_v6
```

当前 URL 构造包含：

```text
uin=<QQ>
hostUin=<QQ>
pos=<offset>
num=<page_size>
replynum=100
g_tk=<GTK>
code_version=1
format=jsonp
need_private_comment=1
inCharset=utf-8
outCharset=utf-8
callback=_cb
notice=0
sort=0
dgsort=0
```

当前策略：

- 首页请求 `pos=0,num=20`。
- 读取响应中的 `total`。
- 按 `pos += len(page.Msglist)` 线性翻页，直到 `pos >= total`。

这个接口只返回现存说说，不返回已经删除的说说。

## 动态流与疑似已删除说说重建

动态流使用接口：

```text
feeds2_html_pav_all
```

当前 URL 构造包含：

```text
uin=<QQ>
begin_time=0
end_time=0
getappnotification=1
getnotifi=1
has_get_key=0
offset=<offset>
set=0
count=100
useutf8=1
outputhtmlfeed=1
scope=1
format=jsonp
g_tk=<GTK>
```

关键参数说明：

| 参数 | 当前值 | 当前作用判断 |
| --- | --- | --- |
| `uin` | 当前 QQ | 目标空间账号 |
| `begin_time` | `0` | 潜在时间窗口入口，当前未利用 |
| `end_time` | `0` | 潜在时间窗口入口，当前未利用 |
| `offset` | `page * 100` | 动态流分页主轴 |
| `count` | `100` | 每页请求数量 |
| `total_number` | 响应字段 | 当前 `oneclick` 翻页上限依据 |
| `scope` | `1` | 固定参数，暂无本地证据表明影响历史深度 |
| `set` | `0` | 固定参数，暂无本地证据表明影响历史深度 |
| `has_get_key` | `0` | 固定参数，暂无本地证据表明影响历史深度 |
| `getappnotification` | `1` | 固定参数，更像输出/通知相关开关 |
| `getnotifi` | `1` | 固定参数，更像输出/通知相关开关 |

当前 `oneclick` 策略：

- 每页 `count=100`。
- `pageSize=100`。
- `maxPages=200`。
- 首页读取 `total_number`。
- 如果 `total_number > 0`，按 offset 扫到覆盖总数。
- 如果 `total_number` 不可用，退回“连续 3 个空页停止”。
- 中间空页会被容忍，不再直接认为到底。

这比 v1.1 的“连续空页停止”更稳，因为动态流分页可能稀疏，中间出现空页不代表历史结束。

## HTML 解析

动态流响应包含 HTML 片段。`oneclick` 当前会提取响应中全部 `html:'...'` 段并拼接，再解析活动。

这是一个重要修复点：主程序里的 `pkg/utils.ProcessOldHTML` 只取第一个 `html:'...'` 段，理论上会漏掉同一页里的大量 feed。后续如果要把主程序作为实验基底，需要先同步 `oneclick` 的多段 HTML 提取逻辑。

当前活动解析依赖这些 HTML 结构：

- `li.f-single.f-s-s`
- `q_namecard`
- `info-detail`
- `txt-box-title`
- `img-item`

如果更早年份的动态流 HTML 结构不同，接口即使返回数据，当前解析器也可能把它误判为空。

## 图片下载

v1.3 图片下载逻辑：

- 现存说说每个图片位保留多个候选 URL。
- 优先尝试原图 URL。
- 原图失败后尝试备用地址。
- 只有响应内容被识别为 `image/*` 且长度足够时才内嵌。
- 页面只渲染成功内嵌的图片，避免 broken image。

关键参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `defaultDownloadConcurrency` | `16` | 默认图片下载并发 |
| `QZONE_DOWNLOAD_CONCURRENCY` | 空 | 可通过环境变量调整下载并发 |
| 并发上限 | `64` | 防止过高并发 |
| 图片请求重试 | `8` 次 | 用于短暂断网、切热点、图床抖动 |
| 普通接口重试 | `3` 次 | 用于说说和动态流接口 |

示例：

```bash
QZONE_DOWNLOAD_CONCURRENCY=32 ./QzoneExport-macOS
```

## 当前边界与风险

目前观察到的最远动态时间是 2017-09-03。这个边界可能来自三类原因。

### 代码策略边界

可能因素：

- `maxPages=200` 限制最多扫描约 20,000 个 offset。
- `total_number` 可能是服务端裁剪后的总数。
- 当前逻辑不会主动探索 `total_number` 之外的 offset。
- 更早年份 HTML 结构可能无法被现有解析器识别。

### 服务端边界

可能因素：

- QQ 动态流服务端只保留到某个历史窗口。
- 深 offset 返回空页、错误页或 5xx。
- 老数据冷热分层，不再通过 `feeds2_html_pav_all` 暴露。

### 风控边界

可能因素：

- 过高并发。
- 高频失败重试。
- 同一 cookie 同时运行多个工具。
- 长时间无人值守扫描。

## 更深年代探查建议

不建议直接把正式导出逻辑改得更激进。建议新增实验模式，例如：

```bash
./QzoneExport-macOS --probe-depth
```

实验模式只记录指标，不生成最终导出结果，也不改变正式导出路径。

### 第一阶段：验证 begin_time/end_time 是否有效

只探首页 `offset=0`，比较不同时间窗口返回是否变化：

- `2017-09`
- `2017-08`
- `2016-01`

需要记录：

- 原始响应大小
- 是否包含 `total_number`
- `total_number` 数值
- 解析活动条数
- 首条和末条 `timeText`
- 最早时间

如果不同窗口返回内容明显不同，说明时间窗口参数可能有效。

### 第二阶段：指数 offset 探查

在 `begin_time=0,end_time=0` 下探：

```text
0, 100, 200, 400, 800, 1600, ...
```

目标不是抓全量，而是判断：

- `total_number` 外是否还有数据。
- 是否存在“中间空页，后面又非空”的稀疏分页。
- 深 offset 是否稳定返回错误。

### 第三阶段：低并发时间窗口 fan-out

如果时间窗口有效，可以并发探几个邻近月份：

- `2017-09`
- `2017-08`
- `2017-07`

建议并发：

- 起步 2。
- 最高 3 到 4。
- 每个窗口内部仍然限速。

不要高并发横扫所有 offset。

## 建议记录的实验指标

请求级：

- `window_begin`
- `window_end`
- `offset`
- `count`
- `http_status`
- `latency_ms`
- `body_size`
- `contains_total_number`
- `parsed_activity_count`

窗口级：

- `window_raw_count`
- `window_dedup_count`
- `window_min_time`
- `window_max_time`
- `window_empty_pages`
- `window_sparse_gap_count`
- `window_unique_senders`

全局级：

- `global_raw_count`
- `global_dedup_count`
- `global_min_time`
- `breakthrough_before_2017_09_03`
- `ban_signal_count`
- `auth_refresh_count`

## 建议停止条件

实验模式应在以下情况停止或降速：

- 连续 2 次登录态异常。
- 响应体突然不含预期模板片段。
- 多个窗口同时返回空且 `total_number` 缺失。
- 深 offset 持续 5xx。
- 请求耗时显著升高。
- cookie 失效或被踢下线。

## 后续实现建议

优先顺序：

1. 把 `oneclick` 的多段 HTML 提取逻辑沉淀为可测试函数，并避免主程序继续使用只取首段的 `ProcessOldHTML`。
2. 新增只读实验模式，先验证 `begin_time/end_time`。
3. 新增 offset 指数探查和空页容忍指标。
4. 仅当实验数据显示有效，再考虑把窗口化扫描并入正式导出。
5. 正式导出仍应保守限速，避免风控。

## 使用声明

本项目仅供学习、研究与个人娱乐使用。请只导出和保存你自己的 QQ 空间内容，不要用于侵犯他人隐私、批量抓取、商业用途或任何违反平台规则的行为。
