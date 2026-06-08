// QQ空间说说一键导出（零依赖单文件版）
// 编译: go build -o qzone.exe   （或交叉编译为各平台）
// 运行: 双击即可。扫码登录 -> 抓取全部现存说说 -> 原图内嵌 -> 自动用浏览器打开。
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"

// ---------- 工具函数 ----------

func genGTK(skey string) string {
	h := 5381
	for _, c := range skey {
		h += (h << 5) + int(c)
	}
	return strconv.Itoa(h & 2147483647)
}

func ptqrToken(qrsig string) int {
	e := 0
	for _, c := range qrsig {
		e += (e << 5) + int(c)
	}
	return 2147483647 & e
}

func openInSystem(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

// ---------- 数据结构 ----------

// flexStr 兼容 g_tk 在 JSON 里既可能是字符串也可能是数字
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	*f = flexStr(strings.Trim(string(b), `"`))
	return nil
}

type config struct {
	QQ      string            `json:"qq"`
	Cookies map[string]string `json:"cookies"`
	GTK     flexStr           `json:"g_tk"`
}

type pic struct {
	Url1     string `json:"url1"`
	Url2     string `json:"url2"`
	Url3     string `json:"url3"`
	URL      string `json:"url"`
	Smallurl string `json:"smallurl"`
}

type comment struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type msg struct {
	Content     string    `json:"content"`
	CreatedTime int64     `json:"created_time"`
	Pic         []pic     `json:"pic"`
	Commentlist []comment `json:"commentlist"`
	LikeTotal   int       `json:"likeTotal"`
}

type msglistResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Total   int    `json:"total"`
	Msglist []msg  `json:"msglist"`
}

// ---------- 登录 ----------

func login(dir string) *config {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 25 * time.Second}

	get := func(u string) ([]byte, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	fmt.Println("正在获取登录二维码…")
	png, err := get("https://ssl.ptlogin2.qq.com/ptqrshow?appid=549000912&e=2&l=M&s=3&d=72&v=4&t=0.8&daid=5")
	if err != nil {
		fmt.Println("❌ 获取二维码失败，请检查网络：", err)
		waitExit()
		os.Exit(1)
	}
	pURL, _ := url.Parse("https://ssl.ptlogin2.qq.com")
	var qrsig string
	for _, c := range jar.Cookies(pURL) {
		if c.Name == "qrsig" {
			qrsig = c.Value
		}
	}
	if qrsig == "" {
		fmt.Println("❌ 未获取到二维码标识，请重试")
		waitExit()
		os.Exit(1)
	}
	qrPath := filepath.Join(dir, "login_qr.png")
	_ = os.WriteFile(qrPath, png, 0600)
	fmt.Println("✅ 二维码已弹出，请用【手机QQ】扫码并在手机上点击确认登录…")
	openInSystem(qrPath)

	token := ptqrToken(qrsig)
	var success string
	for i := 0; i < 180; i++ {
		u := fmt.Sprintf("https://ssl.ptlogin2.qq.com/ptqrlogin?u1=https%%3A%%2F%%2Fqzs.qq.com%%2Fqzone%%2Fv5%%2Floginsucc.html%%3Fpara%%3Dizone&ptqrtoken=%d&ptredirect=0&h=1&t=1&g=1&from_ui=1&ptlang=2052&action=0-0-%d&js_ver=20032614&js_type=1&login_sig=&pt_uistyle=40&aid=549000912&daid=5&", token, time.Now().UnixMilli())
		body, _ := get(u)
		txt := string(body)
		switch {
		case strings.Contains(txt, "二维码未失效"):
			fmt.Print("  …等待扫码\r")
		case strings.Contains(txt, "二维码认证中"):
			fmt.Print("  …已扫码，请在手机上点击确认\r")
		case strings.Contains(txt, "二维码已失效"):
			fmt.Println("\n❌ 二维码已失效，请重新运行")
			waitExit()
			os.Exit(1)
		case strings.Contains(txt, "登录成功"):
			fmt.Println("\n✅ 登录成功，正在换取凭证…")
			success = txt
		}
		if success != "" {
			break
		}
		time.Sleep(time.Second)
	}
	if success == "" {
		fmt.Println("\n❌ 超时未登录，请重新运行")
		waitExit()
		os.Exit(1)
	}

	sigx := regexp.MustCompile(`ptsigx=(.*?)&`).FindStringSubmatch(success)
	uinM := regexp.MustCompile(`uin=(\d+)`).FindStringSubmatch(success)
	if len(sigx) < 2 || len(uinM) < 2 {
		fmt.Println("❌ 登录响应解析失败")
		waitExit()
		os.Exit(1)
	}
	checkURL := fmt.Sprintf("https://ptlogin2.qzone.qq.com/check_sig?pttype=1&uin=%s&service=ptqrlogin&nodirect=0&ptsigx=%s&s_url=https%%3A%%2F%%2Fqzs.qq.com%%2Fqzone%%2Fv5%%2Floginsucc.html%%3Fpara%%3Dizone&f_url=&ptlang=2052&ptredirect=100&aid=549000912&daid=5&j_later=0&low_login_hour=0&regmaster=0&pt_login_type=3&pt_aid=0&pt_aaid=16&pt_light=0&pt_3rd_aid=0", uinM[1], sigx[1])
	_, _ = get(checkURL) // 跟随重定向，cookiejar 自动收集 skey/p_skey

	cookies := map[string]string{}
	qzURL, _ := url.Parse("https://user.qzone.qq.com")
	for _, c := range jar.Cookies(qzURL) {
		cookies[c.Name] = c.Value
	}
	skey := cookies["p_skey"]
	if skey == "" {
		skey = cookies["skey"]
	}
	if skey == "" {
		fmt.Println("❌ 未取得登录凭证，请重试")
		waitExit()
		os.Exit(1)
	}
	qq := regexp.MustCompile(`^o0*`).ReplaceAllString(cookies["uin"], "")
	if qq == "" {
		qq = uinM[1]
	}
	cfg := &config{QQ: qq, Cookies: cookies, GTK: flexStr(genGTK(skey))}
	saveConfig(dir, cfg)
	return cfg
}

func saveConfig(dir string, cfg *config) {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "cookies.json"), b, 0600)
}

func loadConfig(dir string) *config {
	b, err := os.ReadFile(filepath.Join(dir, "cookies.json"))
	if err != nil {
		return nil
	}
	var cfg config
	if json.Unmarshal(b, &cfg) != nil || cfg.QQ == "" {
		return nil
	}
	return &cfg
}

// ---------- 抓取 ----------

func (c *config) cookieHeader() string {
	parts := make([]string, 0, len(c.Cookies))
	for k, v := range c.Cookies {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

func (c *config) httpGet(u string) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://user.qzone.qq.com/"+c.QQ)
	req.Header.Set("Cookie", c.cookieHeader())
	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

var jsonpRe = regexp.MustCompile(`(?s)^[^(]*\((.*)\)[^)]*$`)

func (c *config) fetchPage(pos, num int) (*msglistResp, error) {
	u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/taotao.qq.com/cgi-bin/emotion_cgi_msglist_v6?uin=%s&hostUin=%s&pos=%d&num=%d&replynum=100&g_tk=%s&code_version=1&format=jsonp&need_private_comment=1&inCharset=utf-8&outCharset=utf-8&callback=_cb&notice=0&sort=0&dgsort=0", c.QQ, c.QQ, pos, num, string(c.GTK))
	body, err := c.httpGet(u)
	if err != nil {
		return nil, err
	}
	m := jsonpRe.FindSubmatch(body)
	var raw []byte
	if m != nil {
		raw = m[1]
	} else {
		raw = body
	}
	var r msglistResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

var wRe = regexp.MustCompile(`&w=\d+`)
var hRe = regexp.MustCompile(`&h=\d+`)
var sizeRe = regexp.MustCompile(`!/\w+&`)

func toOriginal(u string) string {
	u = strings.ReplaceAll(u, "\\", "")
	u = wRe.ReplaceAllString(u, "")
	u = hRe.ReplaceAllString(u, "")
	u = sizeRe.ReplaceAllString(u, "!/0&")
	return u
}

func picURLs(m *msg) []string {
	var out []string
	for _, p := range m.Pic {
		for _, v := range []string{p.Url3, p.Url2, p.Url1, p.URL, p.Smallurl} {
			if strings.HasPrefix(v, "http") {
				out = append(out, toOriginal(v))
				break
			}
		}
	}
	return out
}

// ---------- HTML ----------

func fmtTime(ts int64) string {
	if ts == 0 {
		return "时间未知"
	}
	t := time.Unix(ts, 0)
	return fmt.Sprintf("%d年%d月%d日 %02d:%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute())
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return strings.ReplaceAll(s, "\n", "<br>")
}

func buildHTML(qq string, msgs []msg, datauri map[string]string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>QQ空间 · ` + qq + `</title><style>
:root{--bg:#0f1115;--card:#1a1d24;--text:#e6e8eb;--muted:#8b929e;--accent:#f2a73b;}
*{box-sizing:border-box;}
body{margin:0;background:var(--bg);color:var(--text);font-family:-apple-system,"Segoe UI","Microsoft YaHei","PingFang SC",sans-serif;line-height:1.6;}
.topbar{position:sticky;top:0;background:rgba(15,17,21,.94);backdrop-filter:blur(8px);padding:14px 20px;border-bottom:1px solid #2a2e37;z-index:10;}
.topbar h1{margin:0;font-size:17px;}
.topbar input{margin-top:10px;width:100%;padding:8px 12px;border-radius:8px;border:1px solid #2a2e37;background:#13161c;color:var(--text);font-size:14px;}
.wrap{max-width:680px;margin:0 auto;padding:20px;}
.card{background:var(--card);border:1px solid #252934;border-radius:14px;padding:16px 18px;margin-bottom:16px;}
.card header{margin-bottom:8px;} .date{color:var(--muted);font-size:13px;}
.content{white-space:pre-wrap;word-break:break-word;}
.imgs{display:grid;grid-template-columns:repeat(3,1fr);gap:6px;margin-top:12px;}
.imgs img{width:100%;aspect-ratio:1;object-fit:cover;border-radius:8px;background:#0a0c10;cursor:zoom-in;}
.cmts{margin-top:12px;padding-top:10px;border-top:1px solid #252934;font-size:13px;}
.cmt{color:var(--muted);margin:3px 0;} .cmt b{color:#bcd0ff;}
footer{display:flex;gap:16px;margin-top:12px;color:var(--muted);font-size:13px;} .like{color:var(--accent);}
.hidden{display:none;}
#lb{display:none;position:fixed;inset:0;background:rgba(0,0,0,.93);z-index:9999;align-items:center;justify-content:center;cursor:zoom-out;}
#lb img{max-width:96vw;max-height:96vh;object-fit:contain;border-radius:6px;}
</style></head><body>`)
	b.WriteString(`<div class="topbar"><h1>QQ空间 · ` + qq + ` · 共 ` + fmt.Sprint(len(msgs)) + ` 条说说</h1><input id="q" placeholder="搜索内容…"></div>`)
	b.WriteString(`<div class="wrap" id="feed">`)
	for i := range msgs {
		m := &msgs[i]
		b.WriteString(`<article class="card"><header><span class="date">` + fmtTime(m.CreatedTime) + `</span></header>`)
		b.WriteString(`<div class="content">` + esc(m.Content) + `</div>`)
		urls := picURLs(m)
		if len(urls) > 0 {
			b.WriteString(`<div class="imgs">`)
			for _, u := range urls {
				src := datauri[u]
				if src == "" {
					src = u
				}
				b.WriteString(`<img loading="lazy" src="` + src + `">`)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`<footer><span class="like">❤ ` + fmt.Sprint(m.LikeTotal) + `</span>`)
		if len(m.Commentlist) > 0 {
			b.WriteString(`<span>💬 ` + fmt.Sprint(len(m.Commentlist)) + `</span>`)
		}
		b.WriteString(`</footer>`)
		if len(m.Commentlist) > 0 {
			b.WriteString(`<div class="cmts">`)
			for _, c := range m.Commentlist {
				b.WriteString(`<div class="cmt"><b>` + esc(c.Name) + `</b>：` + esc(c.Content) + `</div>`)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`</article>`)
	}
	b.WriteString(`</div><div id="lb" onclick="this.style.display='none'"><img id="lbimg" src=""></div>`)
	b.WriteString(`<script>
var q=document.getElementById('q'),cards=[].slice.call(document.querySelectorAll('.card'));
q.addEventListener('input',function(){var v=q.value.trim().toLowerCase();
cards.forEach(function(c){c.classList.toggle('hidden',v&&c.textContent.toLowerCase().indexOf(v)<0);});});
document.addEventListener('click',function(e){if(e.target.tagName==='IMG'&&e.target.closest('.imgs')){
document.getElementById('lbimg').src=e.target.src;document.getElementById('lb').style.display='flex';}});
document.addEventListener('keydown',function(e){if(e.key==='Escape')document.getElementById('lb').style.display='none';});
</script></body></html>`)
	return b.String()
}

func waitExit() {
	if runtime.GOOS == "windows" {
		fmt.Print("\n按回车键退出…")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func main() {
	fmt.Println("================================================")
	fmt.Println("            QQ空间说说导出工具")
	fmt.Println("================================================")
	dir := exeDir()

	cfg := loadConfig(dir)
	if cfg != nil {
		if r, err := cfg.fetchPage(0, 1); err == nil && r.Code == 0 {
			fmt.Printf("✅ 检测到有效登录（QQ %s），跳过扫码\n", cfg.QQ)
		} else {
			fmt.Println("ℹ️ 旧登录已过期，需要重新扫码")
			cfg = nil
		}
	}
	if cfg == nil {
		cfg = login(dir)
	}

	fmt.Printf("\n开始抓取 QQ %s 的全部说说…\n", cfg.QQ)
	first, err := cfg.fetchPage(0, 20)
	if err != nil || first.Code != 0 {
		fmt.Println("❌ 抓取失败：", err)
		waitExit()
		os.Exit(1)
	}
	total := first.Total
	fmt.Printf("共有 %d 条说说\n", total)
	msgs := append([]msg{}, first.Msglist...)
	pos := len(msgs)
	for pos < total {
		page, err := cfg.fetchPage(pos, 20)
		if err != nil {
			time.Sleep(2 * time.Second)
			page, err = cfg.fetchPage(pos, 20)
			if err != nil {
				break
			}
		}
		if len(page.Msglist) == 0 {
			break
		}
		msgs = append(msgs, page.Msglist...)
		pos += len(page.Msglist)
		fmt.Printf("  已抓 %d/%d\r", len(msgs), total)
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("\n实际抓到 %d 条\n", len(msgs))

	// 收集图片
	seen := map[string]bool{}
	var urls []string
	for i := range msgs {
		for _, u := range picURLs(&msgs[i]) {
			if !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}

	fmt.Printf("下载 %d 张图片（原图画质）…\n", len(urls))
	datauri := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var done int
	var dmu sync.Mutex
	for _, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			raw, err := cfg.httpGet(u)
			dmu.Lock()
			done++
			fmt.Printf("  %d/%d\r", done, len(urls))
			dmu.Unlock()
			if err == nil && len(raw) > 200 {
				du := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw)
				mu.Lock()
				datauri[u] = du
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	fmt.Printf("\n图片完成：成功 %d/%d\n", len(datauri), len(urls))

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedTime > msgs[j].CreatedTime })
	doc := buildHTML(cfg.QQ, msgs, datauri)
	outPath := filepath.Join(dir, "QQ空间_"+cfg.QQ+".html")
	_ = os.WriteFile(outPath, []byte(doc), 0644)
	fi, _ := os.Stat(outPath)
	fmt.Printf("\n✅ 完成！已生成 %s（%.1f MB，%d 条说说）\n", filepath.Base(outPath), float64(fi.Size())/1024/1024, len(msgs))
	fmt.Println("   正在用默认浏览器打开…")
	openInSystem(outPath)
	fmt.Println("\n⚠️ 提示：cookies.json 含你的登录凭证，用完建议删除，切勿发给他人。")
	waitExit()
}
