# 一键导出 QQ 空间说说 · One-Click Qzone Export

把你 QQ 空间里**现存的全部说说**（含图片、评论）一键导出成一个**离线 HTML 网页**，
原图高清、可搜索、点击看大图，整个网页自包含（图片内嵌），断网也能看、可随意分享。

> 面向普通用户：**下载对应平台的可执行文件，双击 → 扫码 → 自动生成并打开网页。电脑上不需要安装任何东西。**

---

## 下载（推荐）

前往 [Releases](../../releases) 下载对应平台的文件：

| 平台 | 文件 | 用法 |
| --- | --- | --- |
| Windows | `QzoneExport.exe` | 双击运行。首次若弹“已保护你的电脑”→ “更多信息” → “仍要运行”。 |
| macOS（Intel/Apple Silicon 通用） | `QzoneExport-macOS` | 双击运行。首次若提示“无法验证开发者”→ 右键图标 → 打开 → 再点“打开”。 |

运行后：屏幕弹出二维码 → **手机 QQ 扫码并确认** → 自动抓取、下载图片 → **自动用浏览器打开**生成的 `QQ空间_你的QQ号.html`。

---

## 它做了什么 / 为什么需要它

QQ 空间官方没有“导出说说”的功能。本工具通过你**自己登录**后的官方接口
（`emotion_cgi_msglist_v6`）翻页拉取你**现存**的说说，并：

- 自动去掉缩略图的尺寸限制参数，下载**原图**画质；
- 带 `Referer` 下载图片并 **base64 内嵌**进 HTML，避免 QQ 图床链接过期 / 防盗链导致图片丢失；
- 生成带**搜索**和**点击看大图（灯箱）**的单文件网页。

> ⚠️ 只能导出**还没被删除**的说说。当年删掉的内容 QQ 服务器已不再保留，任何工具都无法找回原文。

---

## 隐私与安全

- 本工具**只连接 QQ 官方服务器**，不会把你的任何数据上传到作者或第三方（源码可审计，全程只有 GET 请求、无任何上报）。
- 登录后会在同目录生成 `cookies.json`（含你的登录凭证，用于下次免扫码）。
  **用完建议删除，切勿发给任何人。**
- 生成的 HTML 含你的说说与图片，请自行妥善保管。

---

## 从源码编译（开发者）

纯 Go 标准库，无第三方依赖：

```bash
cd oneclick
# 当前平台
go build -o QzoneExport .
# 交叉编译 Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o QzoneExport.exe .
# 交叉编译 macOS 通用二进制
GOOS=darwin GOARCH=arm64 go build -o qe-arm64 . && GOOS=darwin GOARCH=amd64 go build -o qe-amd64 . && lipo -create -output QzoneExport-macOS qe-arm64 qe-amd64
```

## 备选：Python 脚本（需自备 Python 3）

`qzone_export.py` 是同等功能的跨平台 Python 版；`run.bat` 是 Windows 下的启动器；
`patch_lightbox.py` 可把旧版导出的 HTML 修复成“点击看大图”的灯箱版。

```bash
python3 qzone_export.py
```
