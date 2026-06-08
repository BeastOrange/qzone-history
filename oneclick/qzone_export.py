#!/usr/bin/env python3
# coding: utf-8
"""
qzone_export.py — QQ空间说说一键导出（跨平台：Windows / macOS / Linux）

功能：扫码登录 → 抓取你全部现存说说（含评论）→ 下载原图并内嵌 →
生成自包含 HTML → 自动用默认浏览器打开。

只用 Python 标准库，无需 pip 安装任何东西。
用法：  python qzone_export.py
        （Windows 用户可直接双击 启动.bat）
"""
import urllib.request, urllib.error, http.cookiejar, ssl, time, re, json, sys, os, html, base64, webbrowser, subprocess
import concurrent.futures

ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"
HERE = os.path.dirname(os.path.abspath(__file__))
COOKIE_FILE = os.path.join(HERE, "cookies.json")


def open_path(p):
    """用系统默认程序打开文件（跨平台）"""
    p = os.path.abspath(p)
    try:
        if sys.platform.startswith("win"):
            os.startfile(p)            # type: ignore[attr-defined]
        elif sys.platform == "darwin":
            subprocess.run(["open", p])
        else:
            subprocess.run(["xdg-open", p])
    except Exception:
        webbrowser.open("file://" + p)


def gen_gtk(skey):
    h = 5381
    for ch in skey:
        h += (h << 5) + ord(ch)
    return str(h & 2147483647)


def ptqr_token(qrsig):
    e = 0
    for ch in qrsig:
        e += (e << 5) + ord(ch)
    return str(2147483647 & e)


# ---------------- 扫码登录 ----------------
def login():
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(
        urllib.request.HTTPSHandler(context=ctx),
        urllib.request.HTTPCookieProcessor(jar),
    )

    def get(url, redirect=True):
        op = opener
        if not redirect:
            class NoRedirect(urllib.request.HTTPRedirectHandler):
                def redirect_request(self, *a, **k):
                    return None
            op = urllib.request.build_opener(
                urllib.request.HTTPSHandler(context=ctx),
                urllib.request.HTTPCookieProcessor(jar),
                NoRedirect,
            )
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        try:
            r = op.open(req, timeout=20)
            return r.read()
        except urllib.error.HTTPError as e:
            return e.read()

    print("正在获取登录二维码…")
    png = get("https://ssl.ptlogin2.qq.com/ptqrshow?appid=549000912&e=2&l=M&s=3&d=72&v=4&t=0.8&daid=5")
    qrsig = next((c.value for c in jar if c.name == "qrsig"), None)
    if not qrsig:
        print("❌ 获取二维码失败，请检查网络后重试"); sys.exit(1)
    qr_path = os.path.join(HERE, "login_qr.png")
    open(qr_path, "wb").write(png)
    print("✅ 二维码已弹出，请用【手机QQ】扫码并在手机上确认登录…")
    open_path(qr_path)

    token = ptqr_token(qrsig)
    success = None
    for _ in range(180):
        u = ("https://ssl.ptlogin2.qq.com/ptqrlogin?u1=https%3A%2F%2Fqzs.qq.com%2Fqzone%2Fv5%2Floginsucc.html%3Fpara%3Dizone"
             f"&ptqrtoken={token}&ptredirect=0&h=1&t=1&g=1&from_ui=1&ptlang=2052&action=0-0-{int(time.time()*1000)}"
             "&js_ver=20032614&js_type=1&login_sig=&pt_uistyle=40&aid=549000912&daid=5&")
        txt = get(u).decode("utf-8", "replace")
        if "二维码未失效" in txt:
            print("  …等待扫码", end="\r")
        elif "二维码认证中" in txt:
            print("  …已扫码，请在手机上点击确认", end="\r")
        elif "二维码已失效" in txt:
            print("\n❌ 二维码已失效，请重新运行"); sys.exit(1)
        elif "登录成功" in txt:
            print("\n✅ 登录成功，正在换取凭证…"); success = txt; break
        time.sleep(1)
    if not success:
        print("\n❌ 超时未登录，请重新运行"); sys.exit(1)

    m_sig = re.search(r"ptsigx=(.*?)&", success)
    m_uin = re.search(r"uin=(\d+)", success)
    if not (m_sig and m_uin):
        print("❌ 登录响应解析失败"); sys.exit(1)
    sigx, uin = m_sig.group(1), m_uin.group(1)
    check = (f"https://ptlogin2.qzone.qq.com/check_sig?pttype=1&uin={uin}&service=ptqrlogin&nodirect=0"
             f"&ptsigx={sigx}&s_url=https%3A%2F%2Fqzs.qq.com%2Fqzone%2Fv5%2Floginsucc.html%3Fpara%3Dizone"
             "&f_url=&ptlang=2052&ptredirect=100&aid=549000912&daid=5&j_later=0&low_login_hour=0"
             "&regmaster=0&pt_login_type=3&pt_aid=0&pt_aaid=16&pt_light=0&pt_3rd_aid=0")
    get(check, redirect=False)

    cookies = {c.name: c.value for c in jar}
    if "p_skey" not in cookies and "skey" not in cookies:
        print("❌ 未取得登录凭证，请重试"); sys.exit(1)
    skey = cookies.get("p_skey") or cookies.get("skey")
    cfg = {
        "qq": re.sub(r"^o0*", "", cookies.get("uin", uin)) or uin,
        "cookies": cookies,
        "g_tk": gen_gtk(skey),
    }
    json.dump(cfg, open(COOKIE_FILE, "w"), ensure_ascii=False, indent=2)
    return cfg


# ---------------- 抓取说说 ----------------
def http_get(url, cfg):
    cookie = "; ".join(f"{k}={v}" for k, v in cfg["cookies"].items())
    req = urllib.request.Request(url, headers={
        "User-Agent": UA,
        "Referer": f"https://user.qzone.qq.com/{cfg['qq']}",
        "Cookie": cookie,
    })
    return urllib.request.urlopen(req, timeout=30, context=ctx).read()


def fetch_page(cfg, pos, num=20):
    url = ("https://user.qzone.qq.com/proxy/domain/taotao.qq.com/cgi-bin/emotion_cgi_msglist_v6"
           f"?uin={cfg['qq']}&hostUin={cfg['qq']}&pos={pos}&num={num}&replynum=100&g_tk={cfg['g_tk']}"
           "&code_version=1&format=jsonp&need_private_comment=1&inCharset=utf-8&outCharset=utf-8"
           "&callback=_cb&notice=0&sort=0&dgsort=0")
    raw = http_get(url, cfg).decode("utf-8", "replace")
    m = re.search(r"_cb\((.*)\)", raw, re.S)
    return json.loads(m.group(1) if m else raw)


def valid(cfg):
    try:
        return fetch_page(cfg, 0, 1).get("code", -1) == 0
    except Exception:
        return False


def to_original(u):
    u = u.replace("\\", "")
    u = re.sub(r"&w=\d+", "", u); u = re.sub(r"&h=\d+", "", u)
    u = re.sub(r"!/\w+&", "!/0&", u)
    return u


def pic_urls(msg):
    out = []
    for p in (msg.get("pic") or []):
        for k in ("url3", "url2", "url1", "url", "smallurl"):
            v = p.get(k)
            if isinstance(v, str) and v.startswith("http"):
                out.append(to_original(v)); break
    return out


def fmt_time(ts):
    if not ts:
        return "时间未知"
    t = time.localtime(ts)
    # 手动格式化，避免 Windows 不支持 %-m / %-d
    return f"{t.tm_year}年{t.tm_mon}月{t.tm_mday}日 {t.tm_hour:02d}:{t.tm_min:02d}"


def esc(s):
    return html.escape(s or "").replace("\n", "<br>")


def main():
    print("=" * 48)
    print("  QQ空间说说导出工具")
    print("=" * 48)

    cfg = None
    if os.path.exists(COOKIE_FILE):
        try:
            cfg = json.load(open(COOKIE_FILE, encoding="utf-8"))
            if valid(cfg):
                print(f"✅ 检测到有效登录（QQ {cfg['qq']}），跳过扫码")
            else:
                print("ℹ️ 旧登录已过期，需要重新扫码")
                cfg = None
        except Exception:
            cfg = None
    if cfg is None:
        cfg = login()

    QQ = cfg["qq"]
    print(f"\n开始抓取 QQ {QQ} 的全部说说…")
    first = fetch_page(cfg, 0, 20)
    if first.get("code", 0) != 0:
        print("❌ 接口返回错误：", first.get("message")); sys.exit(1)
    total = first.get("total", 0)
    print(f"共有 {total} 条说说")
    msgs = list(first.get("msglist") or [])
    pos = len(msgs)
    while pos < total:
        try:
            page = fetch_page(cfg, pos, 20)
        except Exception:
            time.sleep(2)
            try:
                page = fetch_page(cfg, pos, 20)
            except Exception:
                break
        cur = page.get("msglist") or []
        if not cur:
            break
        msgs.extend(cur); pos += len(cur)
        print(f"  已抓 {len(msgs)}/{total}", end="\r")
        time.sleep(0.4)
    print(f"\n实际抓到 {len(msgs)} 条")

    # 收集图片
    seen = set(); urls = []
    for m in msgs:
        for u in pic_urls(m):
            if u not in seen:
                seen.add(u); urls.append(u)

    def fetch_img(u):
        try:
            raw = http_get(u, cfg)
            return u, "data:image/jpeg;base64," + base64.b64encode(raw).decode()
        except Exception:
            return u, None

    print(f"下载 {len(urls)} 张图片（原图画质）…")
    datauri = {}; ok = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
        for i, (u, du) in enumerate(ex.map(fetch_img, urls), 1):
            if du:
                datauri[u] = du; ok += 1
            print(f"  {i}/{len(urls)}", end="\r")
    print(f"\n图片完成：成功 {ok}/{len(urls)}")

    # 生成 HTML
    msgs.sort(key=lambda m: m.get("created_time", 0), reverse=True)
    cards = []
    for m in msgs:
        imgs = "".join(f'<img loading="lazy" src="{html.escape(datauri.get(u, u))}">' for u in pic_urls(m))
        img_block = f'<div class="imgs">{imgs}</div>' if imgs else ""
        cmts = m.get("commentlist") or []
        cmt_block = ""
        if cmts:
            rows = "".join(f'<div class="cmt"><b>{esc(c.get("name",""))}</b>：{esc(c.get("content",""))}</div>' for c in cmts)
            cmt_block = f'<div class="cmts">{rows}</div>'
        cards.append(
            f'<article class="card"><header><span class="date">{fmt_time(m.get("created_time"))}</span></header>'
            f'<div class="content">{esc(m.get("content",""))}</div>{img_block}'
            f'<footer><span class="like">❤ {m.get("likeTotal",0) or 0}</span>'
            f'{f"<span>💬 {len(cmts)}</span>" if cmts else ""}</footer>{cmt_block}</article>'
        )

    doc = f"""<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>QQ空间 · {QQ} · {len(msgs)} 条说说</title><style>
:root{{--bg:#0f1115;--card:#1a1d24;--text:#e6e8eb;--muted:#8b929e;--accent:#f2a73b;}}
*{{box-sizing:border-box;}}
body{{margin:0;background:var(--bg);color:var(--text);font-family:-apple-system,"Segoe UI","Microsoft YaHei","PingFang SC",sans-serif;line-height:1.6;}}
.topbar{{position:sticky;top:0;background:rgba(15,17,21,.94);backdrop-filter:blur(8px);padding:14px 20px;border-bottom:1px solid #2a2e37;z-index:10;}}
.topbar h1{{margin:0;font-size:17px;}}
.topbar input{{margin-top:10px;width:100%;padding:8px 12px;border-radius:8px;border:1px solid #2a2e37;background:#13161c;color:var(--text);font-size:14px;}}
.wrap{{max-width:680px;margin:0 auto;padding:20px;}}
.card{{background:var(--card);border:1px solid #252934;border-radius:14px;padding:16px 18px;margin-bottom:16px;}}
.card header{{margin-bottom:8px;}} .date{{color:var(--muted);font-size:13px;}}
.content{{white-space:pre-wrap;word-break:break-word;}}
.imgs{{display:grid;grid-template-columns:repeat(3,1fr);gap:6px;margin-top:12px;}}
.imgs img{{width:100%;aspect-ratio:1;object-fit:cover;border-radius:8px;background:#0a0c10;cursor:zoom-in;}}
.cmts{{margin-top:12px;padding-top:10px;border-top:1px solid #252934;font-size:13px;}}
.cmt{{color:var(--muted);margin:3px 0;}} .cmt b{{color:#bcd0ff;}}
footer{{display:flex;gap:16px;margin-top:12px;color:var(--muted);font-size:13px;}} .like{{color:var(--accent);}}
.hidden{{display:none;}}
#lb{{display:none;position:fixed;inset:0;background:rgba(0,0,0,.93);z-index:9999;align-items:center;justify-content:center;cursor:zoom-out;}}
#lb img{{max-width:96vw;max-height:96vh;object-fit:contain;border-radius:6px;}}
</style></head><body>
<div class="topbar"><h1>QQ空间 · {QQ} · 共 {len(msgs)} 条说说</h1><input id="q" placeholder="搜索内容…"></div>
<div class="wrap" id="feed">{"".join(cards)}</div>
<div id="lb" onclick="this.style.display='none'"><img id="lbimg" src=""></div>
<script>
var q=document.getElementById('q'),cards=[].slice.call(document.querySelectorAll('.card'));
q.addEventListener('input',function(){{var v=q.value.trim().toLowerCase();
cards.forEach(function(c){{c.classList.toggle('hidden',v&&c.textContent.toLowerCase().indexOf(v)<0);}});}});
document.addEventListener('click',function(e){{if(e.target.tagName==='IMG'&&e.target.closest('.imgs')){{
document.getElementById('lbimg').src=e.target.src;document.getElementById('lb').style.display='flex';}}}});
document.addEventListener('keydown',function(e){{if(e.key==='Escape')document.getElementById('lb').style.display='none';}});
</script></body></html>"""
    out = os.path.join(HERE, f"qzone_{QQ}.html")
    open(out, "w", encoding="utf-8").write(doc)
    size = os.path.getsize(out) / 1024 / 1024
    print(f"\n✅ 完成！已生成 {os.path.basename(out)}（{size:.1f} MB，{len(msgs)} 条说说）")
    print("   正在用默认浏览器打开…")
    webbrowser.open("file://" + os.path.abspath(out))
    print("\n⚠️ 提示：cookies.json 含你的登录凭证，用完建议删除，切勿发给他人。")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\n已取消")
    if sys.platform.startswith("win"):
        input("\n按回车键退出…")
