@echo off
chcp 65001 >/dev/null
title QQ空间说说导出工具
echo ============================================
echo            QQ空间说说导出工具
echo ============================================
echo.

REM 优先用 py 启动器,其次 python
where py >/dev/null 2>&1
if %errorlevel%==0 (
    py qzone_export.py
    goto end
)
where python >/dev/null 2>&1
if %errorlevel%==0 (
    python qzone_export.py
    goto end
)

echo [!] 未检测到 Python,需要先安装(只需一次)。
echo     正在打开 Python 下载页,请下载安装,
echo     安装时务必勾选 "Add Python to PATH",装好后再双击本文件。
start https://www.python.org/downloads/
pause

:end
