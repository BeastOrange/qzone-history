# QQ 空间说说一键导出

> 本项目从 [Yinyaommmm/qzone-history](https://github.com/Yinyaommmm/qzone-history) fork 而来。感谢原作者提供的 QQ 空间历史还原思路与工程基础；本 fork 主要面向普通用户的一键导出体验。

一个零依赖的 QQ 空间说说导出工具：双击运行，手机 QQ 扫码登录，自动导出你的说说、评论和图片，生成一个可以离线浏览、搜索、点击看大图的单文件 HTML。

## 下载

前往 [Releases](../../releases) 下载对应平台文件：

| 平台 | 文件 | 用法 |
| --- | --- | --- |
| Windows | `QzoneExport.exe` | 双击运行。若弹出系统保护提示，点“更多信息”后选择“仍要运行”。 |
| macOS | `QzoneExport-macOS` | 首次运行若提示无法验证开发者，右键文件选择“打开”。 |

运行流程：

1. 打开程序。
2. 使用手机 QQ 扫码并确认登录。
3. 等待抓取说说、扫描动态流、下载图片。
4. 程序会在同目录生成 `QQ空间_你的QQ号.html` 并自动打开。

## 主要功能

- 导出现存说说、评论和图片。
- 下载图片并 base64 内嵌到 HTML，离线也能查看。
- 支持搜索、深色/浅色主题、点击图片查看大图。
- 扫描动态流，尽量重建曾被点赞、评论或浏览过的疑似已删除说说。
- 生成详细日志 `QzoneExport_log_<时间>.txt`，便于排查网络和解析问题。
- 复用 `cookies.json`，有效期内再次运行可跳过扫码。
- v1.3 起增强图片下载：备用 URL fallback、多线程下载、断网/切热点时更耐心重试。

## 重要边界

重建已删除说说依赖 QQ 空间动态流里残留的互动痕迹。它只能尽量找回“当年有人互动、且动态流仍有记录”的内容；纯文字、无人互动、服务器已彻底清除或图片链接已失效的内容无法保证恢复。

## 隐私与安全

- 工具只请求 QQ 官方接口，不上传你的数据到作者或第三方。
- `cookies.json` 含登录凭证，用完建议删除，不要发给任何人。
- 生成的 HTML 含你的说说和图片，请自行妥善保存。

## 从源码编译

```bash
cd oneclick

# 当前平台
go build -o QzoneExport .

# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o QzoneExport.exe .

# macOS 当前架构
go build -o QzoneExport-macOS .
```

更多细节见 [oneclick/README.md](oneclick/README.md)。

## 声明

本项目仅供学习、研究与个人娱乐使用。请只导出和保存你自己的 QQ 空间内容，不要用于侵犯他人隐私、批量抓取、商业用途或任何违反平台规则的行为。
