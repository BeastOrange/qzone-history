#!/usr/bin/env python3
# coding: utf-8
"""patch_lightbox.py — 修复已生成 HTML 的"点击大图变 about:blank"问题。
把 <a href="data:..." target="_blank"><img src="data:..."></a>（导航到 data URI 被浏览器拦截）
改成页内灯箱弹层看大图，同时去掉重复的 base64（体积减半）。
用法: python3 patch_lightbox.py viewer_all.html
"""
import re, sys, os

path = sys.argv[1] if len(sys.argv) > 1 else "viewer_all.html"
html = open(path, encoding="utf-8").read()
before = len(html)

# 1) 去掉图片外层 <a href="..." target="_blank"> ... </a>，只留 <img>
#    覆盖 data URI 与下载失败时回退的 http 链接两种
html = re.sub(r'<a href="[^"]*" target="_blank">', '', html)
html = html.replace('</a>', '')

# 2) 注入灯箱样式（在 </style> 前）
lb_css = (
    "#lb{display:none;position:fixed;inset:0;background:rgba(0,0,0,.93);z-index:9999;"
    "align-items:center;justify-content:center;cursor:zoom-out;}"
    "#lb img{max-width:96vw;max-height:96vh;object-fit:contain;border-radius:6px;}"
    ".imgs img{cursor:zoom-in;}"
)
html = html.replace("</style>", lb_css + "</style>", 1)

# 3) 注入灯箱容器 + 点击委托脚本（在 </body> 前）
lb_html = (
    '<div id="lb" onclick="this.style.display=\'none\'"><img id="lbimg" src=""></div>'
    "<script>document.addEventListener('click',function(e){"
    "if(e.target.tagName==='IMG'&&e.target.closest('.imgs')){"
    "document.getElementById('lbimg').src=e.target.src;"
    "document.getElementById('lb').style.display='flex';}});"
    "document.addEventListener('keydown',function(e){"
    "if(e.key==='Escape')document.getElementById('lb').style.display='none';});</script>"
)
html = html.replace("</body>", lb_html + "</body>", 1)

open(path, "w", encoding="utf-8").write(html)
after = len(html)
print(f"✅ 已修复 {path}：{before/1024/1024:.1f}MB → {after/1024/1024:.1f}MB（去重 base64 后体积减小）")
print("   现在点击图片会在页面内弹出大图，按 Esc 或点击空白处关闭。")
