// QQ空间说说一键导出（零依赖单文件版）
// 编译: go build -o qzone.exe   （或交叉编译为各平台）
// 运行: 双击即可。扫码登录 -> 抓取全部现存说说 -> 原图内嵌 -> 自动用浏览器打开。
package main

import (
	"bufio"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
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

// version 当前一键导出工具版本（写入日志与网页，便于排查）
const version = "1.3"

// ---------- 详细日志 ----------
//
// 为方便排查“在别人电脑上跑不出说说/重建不出来”这类问题，整个运行过程会写一份
// 非常详细的日志到 exe 同目录的 QzoneExport_log_<时间>.txt：记录每一步、每个网络
// 请求（凭证 g_tk 已脱敏、cookie 只记名字不记值）、每页抓到多少条、解析与重建明细
// 以及任何错误。控制台仍只显示简洁进度。
var (
	logFile *os.File
	logMu   sync.Mutex
)

func initLog(dir string) string {
	name := "QzoneExport_log_" + time.Now().Format("20060102_150405") + ".txt"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Println("⚠️ 无法创建日志文件：", err)
		return ""
	}
	logFile = f
	lg("==================== QzoneExport 运行日志 ====================")
	lg("版本: v%s", version)
	lg("系统: %s/%s  Go: %s", runtime.GOOS, runtime.GOARCH, runtime.Version())
	lg("工作目录(exe同级): %s", dir)
	return path
}

// lg 写一行带毫秒时间戳的日志到文件（不输出到控制台）
func lg(format string, a ...any) {
	if logFile == nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	line := fmt.Sprintf("[%s] ", time.Now().Format("15:04:05.000")) + fmt.Sprintf(format, a...)
	_, _ = io.WriteString(logFile, line+"\n")
	_ = logFile.Sync()
}

// say 同时输出到控制台与日志，用于关键里程碑
func say(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
	lg(format, a...)
}

func closeLog() {
	if logFile != nil {
		lg("==================== 日志结束 ====================")
		_ = logFile.Close()
	}
}

// gtkMaskRe / maskURL 写日志前把 g_tk 凭证脱敏，避免日志外发时泄露登录态
var gtkMaskRe = regexp.MustCompile(`(g_tk=)[^&]*`)

func maskURL(u string) string { return gtkMaskRe.ReplaceAllString(u, "${1}***") }

// cookieNames 只返回 cookie 的名字（不含值），用于日志
func cookieNames(c map[string]string) string {
	names := make([]string, 0, len(c))
	for k := range c {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// truncate 截断超长字符串（按 rune 边界），用于把原始响应写进日志
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…(已截断)"
}

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

	client *http.Client `json:"-"`
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

	// 以下字段不来自接口，由本工具在“重建已删除说说”时填充
	Deleted   bool     `json:"-"` // true 表示这是从动态流重建出来、现存列表里已不存在（疑似已删除）的说说
	ExtraImgs []string `json:"-"` // 重建说说的图片直链（不走 pic 结构）
	Views     int      `json:"-"` // 重建出的浏览数
}

type msglistResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Total   int    `json:"total"`
	Msglist []msg  `json:"msglist"`
}

// ---------- 重建已删除说说（来自动态流 feeds2_html_pav_all） ----------
//
// QQ 空间的“现存说说”接口（emotion_cgi_msglist_v6）只返回还没被删除的说说。
// 但你被点赞/评论/留言过的说说，会在“动态流”里留下痕迹；即使原说说已被删除，
// 这些痕迹有时仍残留在动态流中。本节复刻主程序 qzone-history 的重建逻辑：
// 抓动态流 HTML -> 解析成一条条 activity -> 按 md5(内容+收件人QQ) 聚合成 moment。
// 注意：只能找回“当年有人互动、且动态流仍残留”的那部分；纯文字无互动、或
// 服务器已彻底清除的说说，任何工具都无法恢复。

// activityType 活动类型（与主程序保持一致的判定）
type activityType int

const (
	typeMoment activityType = iota
	typeForward
	typeLike
	typeComment
	typeBoardMsg
	typeReply
	typeView
	typeOther
)

// activity 动态流里的一条互动记录
type activity struct {
	SenderQQ   string
	SenderName string
	ReceiverQQ string
	Content    string
	Timestamp  time.Time
	TimeText   string
	ImageURLs  []string
	Type       activityType
}

// reconMoment 由若干 activity 聚合而成的（疑似已删除的）说说
type reconMoment struct {
	Key       string
	Content   string
	Timestamp time.Time
	TimeText  string
	ImageURLs []string
	Likes     int
	Views     int
	Comments  []comment
}

// ---------- 登录 ----------

func login(dir string) *config {
	lg("login: 开始扫码登录流程")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 25 * time.Second}

	get := func(u string) ([]byte, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			lg("login GET 失败 url=%s err=%v", maskURL(u), err)
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	fmt.Println("正在获取登录二维码…")
	png, err := get("https://ssl.ptlogin2.qq.com/ptqrshow?appid=549000912&e=2&l=M&s=3&d=72&v=4&t=0.8&daid=5")
	if err != nil {
		say("❌ 获取二维码失败，请检查网络：%v", err)
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
	cfg.initHTTPClient()
	lg("login: 成功 QQ=%s cookies=[%s]", qq, cookieNames(cookies))
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
	cfg.initHTTPClient()
	return &cfg
}

// ---------- 抓取 ----------

func (c *config) initHTTPClient() {
	if c.client != nil {
		return
	}
	c.client = &http.Client{Timeout: 45 * time.Second}
}

func (c *config) cookieHeader() string {
	parts := make([]string, 0, len(c.Cookies))
	for k, v := range c.Cookies {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

func (c *config) httpGet(u string) ([]byte, error) {
	return c.httpGetWithRetry(u, 3, time.Second)
}

func (c *config) httpGetImage(u string) ([]byte, error) {
	return c.httpGetWithRetry(u, 8, 2*time.Second)
}

func (c *config) httpGetWithRetry(u string, attempts int, baseDelay time.Duration) ([]byte, error) {
	if attempts < 1 {
		attempts = 1
	}
	c.initHTTPClient()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Referer", "https://user.qzone.qq.com/"+c.QQ)
		req.Header.Set("Cookie", c.cookieHeader())
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			lg("httpGet 第%d/%d次失败 url=%s err=%v", attempt, attempts, maskURL(u), err)
			sleepBeforeRetry(attempt, attempts, baseDelay)
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			lg("httpGet 第%d/%d次读取失败 url=%s err=%v", attempt, attempts, maskURL(u), rerr)
			sleepBeforeRetry(attempt, attempts, baseDelay)
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			lg("httpGet 第%d/%d次服务端错误 url=%s status=%d", attempt, attempts, maskURL(u), resp.StatusCode)
			sleepBeforeRetry(attempt, attempts, baseDelay)
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

func sleepBeforeRetry(attempt, attempts int, baseDelay time.Duration) {
	if attempt >= attempts {
		return
	}
	delay := time.Duration(attempt) * baseDelay
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	time.Sleep(delay)
}

var jsonpRe = regexp.MustCompile(`(?s)^[^(]*\((.*)\)[^)]*$`)

func (c *config) fetchPage(pos, num int) (*msglistResp, error) {
	u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/taotao.qq.com/cgi-bin/emotion_cgi_msglist_v6?uin=%s&hostUin=%s&pos=%d&num=%d&replynum=100&g_tk=%s&code_version=1&format=jsonp&need_private_comment=1&inCharset=utf-8&outCharset=utf-8&callback=_cb&notice=0&sort=0&dgsort=0", c.QQ, c.QQ, pos, num, string(c.GTK))
	body, err := c.httpGet(u)
	if err != nil {
		lg("fetchPage(pos=%d,num=%d) 请求失败: %v", pos, num, err)
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
		lg("fetchPage(pos=%d,num=%d) JSON解析失败: %v 原始响应(截断): %s", pos, num, err, truncate(string(body), 800))
		return nil, err
	}
	lg("fetchPage(pos=%d,num=%d) -> code=%d total=%d 本页=%d", pos, num, r.Code, r.Total, len(r.Msglist))
	return &r, nil
}

var wRe = regexp.MustCompile(`([?&])w=\d+&?`)
var hRe = regexp.MustCompile(`([?&])h=\d+&?`)
var sizeRe = regexp.MustCompile(`!/\w+&`)

func toOriginal(u string) string {
	u = strings.ReplaceAll(u, "\\", "")
	u = stripQueryNumberParam(u, wRe)
	u = stripQueryNumberParam(u, hRe)
	u = sizeRe.ReplaceAllString(u, "!/0&")
	return u
}

func stripQueryNumberParam(u string, re *regexp.Regexp) string {
	out := re.ReplaceAllString(u, "$1")
	out = strings.ReplaceAll(out, "?&", "?")
	return strings.TrimSuffix(out, "?")
}

type imageCandidate struct {
	Key  string
	URLs []string
}

func isImageSource(u string) bool {
	return u != "" && (strings.HasPrefix(u, "http") || strings.HasPrefix(u, "data:"))
}

func appendUniqueURL(urls []string, u string) []string {
	if !isImageSource(u) {
		return urls
	}
	for _, existing := range urls {
		if existing == u {
			return urls
		}
	}
	return append(urls, u)
}

func picCandidates(p pic) []string {
	var urls []string
	for _, v := range []string{p.Url3, p.Url2, p.Url1, p.URL, p.Smallurl} {
		if !isImageSource(v) {
			continue
		}
		raw := strings.ReplaceAll(v, "\\", "")
		urls = appendUniqueURL(urls, toOriginal(raw))
		urls = appendUniqueURL(urls, raw)
	}
	return urls
}

func imageCandidates(m *msg) []imageCandidate {
	if m.Deleted {
		out := make([]imageCandidate, 0, len(m.ExtraImgs))
		for _, u := range m.ExtraImgs {
			urls := appendUniqueURL(nil, u)
			if len(urls) > 0 {
				out = append(out, imageCandidate{Key: urls[0], URLs: urls})
			}
		}
		return out
	}
	out := make([]imageCandidate, 0, len(m.Pic))
	for _, p := range m.Pic {
		urls := picCandidates(p)
		if len(urls) > 0 {
			out = append(out, imageCandidate{Key: urls[0], URLs: urls})
		}
	}
	return out
}

func picURLs(m *msg) []string {
	candidates := imageCandidates(m)
	out := make([]string, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, item.Key)
	}
	return out
}

// ---------- 动态流抓取 + 解析 + 重建 ----------

// processOldHTML 从 JSONP 响应里取出**全部** html:'...' 段，并正确还原 JS 字符串转义。
//
// 关键修复 1（条数）：动态流一页响应是 data:[{…html:'…',opuin:…},{…html:'…',opuin:…},…]，
// 每个 feed 各有一个 html 段。旧实现只取第一个 `html:'...',opuin` 段，导致一页里 86 条
// 动态只解析出 1 条（其余 85 条连同它们的图片被整段丢弃）。这里改为提取并拼接所有 html 段。
//
// 关键修复 2（乱码）：QQ 模板里的缩进是 JS 转义 `\t`、换行是 `\n`（反斜杠+字母，不是真正的
// 制表符/换行）。旧实现“先折叠空白、再粗暴删掉所有反斜杠”，导致 `\t` 残留成裸字母 t，
// 输出一堆 `ttttt…`。这里改为：先取 html 段 -> 正确还原 \xHH/\uHHHH/\t/\n/\r/\"/\\/\/
// -> 最后再折叠空白。
func processOldHTML(message string) string {
	const startString = "html:'"
	const endString = "',opuin"
	var sb strings.Builder
	rest := message
	for {
		si := strings.Index(rest, startString)
		if si < 0 {
			break
		}
		si += len(startString)
		ei := strings.Index(rest[si:], endString)
		if ei < 0 {
			break
		}
		sb.WriteString(rest[si : si+ei])
		rest = rest[si+ei+len(endString):]
	}
	if sb.Len() == 0 {
		return ""
	}
	seg := unescapeJS(sb.String())
	seg = regexp.MustCompile(`\s+`).ReplaceAllString(seg, " ")
	return seg
}

// unescapeJS 还原 JS 字符串里的转义序列。\xHH / \uHHHH 按字节/码点还原（中文常以 \xHH
// 字节序列出现，逐字节写出后整体仍是合法 UTF-8）。
func unescapeJS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		n := s[i+1]
		switch n {
		case 'x':
			if i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 4
					continue
				}
			}
			b.WriteByte(n)
			i += 2
		case 'u':
			if i+5 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 6
					continue
				}
			}
			b.WriteByte(n)
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 'b':
			b.WriteByte('\b')
			i += 2
		case 'f':
			b.WriteByte('\f')
			i += 2
		case '0':
			b.WriteByte(0)
			i += 2
		default:
			// \" \' \\ \/ 以及其它：保留转义后的那个字符本身
			b.WriteByte(n)
			i += 2
		}
	}
	return b.String()
}

// 动态流字段提取正则。条目切分与字段匹配都做到“类名顺序无关”，避免 QQ 调整 class
// 顺序时解析不到内容（这正是“在别人账号上重建出 0 条”的一类隐患）。
var (
	feedLiOpenRe  = regexp.MustCompile(`<li\b[^>]*>`)
	aInnerRe      = regexp.MustCompile(`(?s)<a\b([^>]*)>(.*?)</a>`)
	divInnerRe    = regexp.MustCompile(`(?s)<div\b([^>]*)>(.*?)</div>`)
	pInnerRe      = regexp.MustCompile(`(?s)<p\b([^>]*)>(.*?)</p>`)
	spanInnerRe   = regexp.MustCompile(`(?s)<span\b([^>]*)>(.*?)</span>`)
	feedSenderLnk = regexp.MustCompile(`link="nameCard_(\d+)"`)
	feedImgRe     = regexp.MustCompile(`<a[^>]*class="[^"]*img-item[^"]*"[^>]*>.*?<img[^>]*src="([^"]+)"`)
	feedImgRe2    = regexp.MustCompile(`<img[^>]*class="[^"]*img-item[^"]*"[^>]*src="([^"]+)"`)
	tagStripRe    = regexp.MustCompile(`<[^>]+>`)
)

// isItemLiTag 判断一个 <li ...> 开标签是否是“一条动态”的容器（两个类名都在、顺序无关）
func isItemLiTag(tag string) bool {
	return strings.Contains(tag, "f-single") && strings.Contains(tag, "f-s-s")
}

// findByClass 在 s 中找第一个“属性里包含全部指定 class 片段”的元素，
// 返回它的属性串和内部内容（类名顺序无关）。
func findByClass(s string, re *regexp.Regexp, classes ...string) (attrs, inner string, ok bool) {
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		good := true
		for _, c := range classes {
			if !strings.Contains(m[1], c) {
				good = false
				break
			}
		}
		if good {
			return m[1], m[2], true
		}
	}
	return "", "", false
}

func stripTags(s string) string {
	s = tagStripRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ")
	return strings.TrimSpace(s)
}

// parseActivities 用正则把一页动态流 HTML 解析成多条 activity（零依赖，不用 goquery）
func (c *config) parseActivities(processedHTML string) []activity {
	if !strings.Contains(processedHTML, "f-single") {
		return nil
	}
	// 找到所有 <li> 开标签，只保留同时含 f-single 和 f-s-s 的（顺序无关）作为条目起点；
	// 每个起点到下一个条目起点（或文末）之间是一条动态。
	liLocs := feedLiOpenRe.FindAllStringIndex(processedHTML, -1)
	var starts []int
	for _, loc := range liLocs {
		if isItemLiTag(processedHTML[loc[0]:loc[1]]) {
			starts = append(starts, loc[0])
		}
	}
	if len(starts) == 0 {
		return nil
	}
	var acts []activity
	for i, start := range starts {
		end := len(processedHTML)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		chunk := processedHTML[start:end]

		act := activity{ReceiverQQ: c.QQ}
		if attrs, inner, ok := findByClass(chunk, aInnerRe, "f-name", "q_namecard"); ok {
			act.SenderName = stripTags(inner)
			if m := feedSenderLnk.FindStringSubmatch(attrs); len(m) >= 2 {
				act.SenderQQ = m[1]
			}
		}
		if _, inner, ok := findByClass(chunk, divInnerRe, "info-detail"); ok {
			act.TimeText = stripTags(inner)
			act.Timestamp = parseFeedTime(act.TimeText)
		}
		if _, inner, ok := findByClass(chunk, pInnerRe, "txt-box-title", "ellipsis-one"); ok {
			act.Content = stripTags(inner)
		}

		for _, mm := range feedImgRe.FindAllStringSubmatch(chunk, -1) {
			act.ImageURLs = append(act.ImageURLs, mm[1])
		}
		for _, mm := range feedImgRe2.FindAllStringSubmatch(chunk, -1) {
			act.ImageURLs = append(act.ImageURLs, mm[1])
		}

		var stateText string
		if _, inner, ok := findByClass(chunk, spanInnerRe, "state"); ok {
			stateText = stripTags(inner)
		}
		switch {
		case strings.Contains(stateText, "赞了我的说说"):
			act.Type = typeLike
		case strings.Contains(stateText, "查看了我的说说"):
			act.Type = typeView
		case strings.Contains(stateText, "评论"):
			act.Type = typeComment
		case strings.Contains(stateText, "留言"):
			act.Type = typeBoardMsg
		case strings.Contains(stateText, "回复"):
			act.Type = typeReply
		default:
			act.Type = typeOther
		}
		acts = append(acts, act)
		lg("  解析活动 #%d: type=%d sender=%s(%s) time=%q content=%q imgs=%d state=%q",
			i, act.Type, act.SenderName, act.SenderQQ, act.TimeText, truncate(act.Content, 60), len(act.ImageURLs), stateText)
	}
	return acts
}

// parseFeedTime 复刻主程序的中文时间解析
func parseFeedTime(s string) time.Time {
	now := time.Now()
	layouts := []string{"2006年1月2日 15:04", "2006年01月02日 15:04", "1月2日 15:04", "01月02日 15:04", "昨天 15:04", "15:04"}
	for _, layout := range layouts {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err == nil {
			switch layout {
			case "2006年1月2日 15:04", "2006年01月02日 15:04":
				return t
			case "1月2日 15:04", "01月02日 15:04":
				return time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
			case "昨天 15:04":
				y := now.AddDate(0, 0, -1)
				return time.Date(y.Year(), y.Month(), y.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
			case "15:04":
				return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
			}
		}
	}
	return time.Time{}
}

// totalNumRe 从动态流响应里读取 total_number（QQ 告知的互动总条数），用于驱动翻页。
var totalNumRe = regexp.MustCompile(`total_number:(\d+)`)

// fetchAllActivities 翻页抓取整条动态流。
//
// QQ 的 feeds2_html_pav_all 在一页（count=100）里会把多条动态放进 data:[] 数组，
// processOldHTML 现在会提取其中**全部** html 段。翻页用响应里的 total_number 驱动：
// offset 从 0 每次步进 count，直到覆盖 total_number。不再用“连续空页就停”——因为
// 动态流数据可能稀疏，中间的空页会让旧逻辑误判到底而漏掉后面的大量数据。
func (c *config) fetchAllActivities() []activity {
	var all []activity
	const pageSize = 100
	const maxPages = 200 // 安全上限，避免异常时无限翻页

	fetch := func(offset int) (string, []byte, error) {
		u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/ic2.qzone.qq.com/cgi-bin/feeds/feeds2_html_pav_all?uin=%s&begin_time=0&end_time=0&getappnotification=1&getnotifi=1&has_get_key=0&offset=%d&set=0&count=%d&useutf8=1&outputhtmlfeed=1&scope=1&format=jsonp&g_tk=%s",
			c.QQ, offset, pageSize, string(c.GTK))
		body, err := c.httpGet(u)
		if err != nil {
			return "", nil, err
		}
		return processOldHTML(string(body)), body, nil
	}

	// 先抓首页，读出 total_number 作为翻页上限
	firstHTML, firstBody, err := fetch(0)
	if err != nil {
		lg("fetchAllActivities offset=0 请求失败: %v", err)
		return all
	}
	total := 0
	if m := totalNumRe.FindSubmatch(firstBody); m != nil {
		total, _ = strconv.Atoi(string(m[1]))
	}
	lg("动态流首页 原始响应(截断2000字): %s", truncate(string(firstBody), 2000))
	lg("动态流首页 处理后HTML(截断2000字): %s", truncate(firstHTML, 2000))
	lg("动态流 total_number=%d", total)

	empty := 0
	for page := 0; page < maxPages; page++ {
		offset := page * pageSize
		var processed string
		if page == 0 {
			processed = firstHTML
		} else {
			processed, _, err = fetch(offset)
			if err != nil {
				lg("fetchAllActivities offset=%d 请求失败: %v", offset, err)
				break
			}
		}

		if processed == "" || !strings.Contains(processed, "f-single") {
			empty++
			lg("fetchAllActivities offset=%d 无活动条目(empty=%d)", offset, empty)
			// 已知总数时，只要还没翻完就继续（容忍中间空页）；未知总数时退回“连续空页”判断
			if total > 0 {
				if offset >= total {
					break
				}
			} else if empty >= 3 {
				break
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		empty = 0
		acts := c.parseActivities(processed)
		lg("fetchAllActivities offset=%d 解析到 %d 条活动", offset, len(acts))
		all = append(all, acts...)
		fmt.Printf("  动态流已扫 %d 条互动\r", len(all))

		// 已抓到的条数已覆盖总数则收工
		if total > 0 && len(all) >= total {
			break
		}
		if total > 0 && offset >= total {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return all
}

// reconstructMoments 复刻主程序：按 md5(内容+收件人QQ) 把活动聚合成说说。
// 仅基于“赞了我的说说 / 查看了我的说说 / 评论(我的说说)”这三类对【我的说说】的互动来
// 重建，避免把留言、回复、转发等非说说的动态误判成“已删除的说说”。
func reconstructMoments(acts []activity) []reconMoment {
	order := []string{}
	m := map[string]*reconMoment{}
	for _, a := range acts {
		if a.Type != typeLike && a.Type != typeView && a.Type != typeComment {
			continue // 只有对“我的说说”的互动才能反推出说说
		}
		if strings.TrimSpace(a.Content) == "" {
			continue // 没有正文的互动无法还原说说内容
		}
		key := momentKey(a.Content, a.ReceiverQQ)
		rm, ok := m[key]
		if !ok {
			rm = &reconMoment{
				Key:       key,
				Content:   a.Content,
				Timestamp: a.Timestamp,
				TimeText:  a.TimeText,
				ImageURLs: a.ImageURLs,
			}
			m[key] = rm
			order = append(order, key)
		}
		if !a.Timestamp.IsZero() && (rm.Timestamp.IsZero() || a.Timestamp.Before(rm.Timestamp)) {
			rm.Timestamp = a.Timestamp
			rm.TimeText = a.TimeText
		}
		if len(a.ImageURLs) > len(rm.ImageURLs) {
			rm.ImageURLs = a.ImageURLs
		}
		switch a.Type {
		case typeLike:
			rm.Likes++
		case typeView:
			rm.Views++
		case typeComment:
			// 注意：动态流里这条 activity 的“内容”实际是被评论的说说正文，而非评论原文，
			// 评论原文在该接口无法可靠取得。因此只保留可靠的“评论者昵称”，正文留空。
			rm.Comments = append(rm.Comments, comment{Name: a.SenderName})
		}
	}
	out := make([]reconMoment, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	return out
}

func momentKey(content, receiverQQ string) string {
	h := md5.Sum([]byte(content + receiverQQ))
	return hex.EncodeToString(h[:])
}

// normContent 归一化文本，用于把重建说说和现存说说去重比对
func normContent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".。… ")
	return strings.Join(strings.Fields(s), "")
}

// reconAsDeletedMsgs 把重建说说里“现存列表里找不到”的那部分转成 msg（标记为已删除）
func reconAsDeletedMsgs(recons []reconMoment, existing []msg) []msg {
	exist := map[string]bool{}
	var existKeys []string
	for i := range existing {
		n := normContent(existing[i].Content)
		if n == "" {
			continue
		}
		exist[n] = true
		existKeys = append(existKeys, n)
	}
	var out []msg
	for _, r := range recons {
		n := normContent(r.Content)
		if n == "" {
			continue // 没有正文内容的重建结果（纯点赞/浏览），无法展示
		}
		if exist[n] {
			lg("重建去重: 命中现存说说，跳过 content=%q", truncate(r.Content, 50))
			continue
		}
		// 动态流里正文常被截断成前缀，用前缀匹配再排除一次现存说说
		matched := false
		for _, ek := range existKeys {
			if strings.HasPrefix(ek, n) && len([]rune(n)) >= 6 {
				matched = true
				break
			}
		}
		if matched {
			lg("重建去重(前缀): 命中现存说说，跳过 content=%q", truncate(r.Content, 50))
			continue
		}
		var imgs []string
		for _, u := range r.ImageURLs {
			imgs = append(imgs, toOriginal(u))
		}
		out = append(out, msg{
			Content:     r.Content,
			CreatedTime: tsToUnix(r.Timestamp),
			Commentlist: r.Comments,
			LikeTotal:   r.Likes,
			Deleted:     true,
			ExtraImgs:   imgs,
			Views:       r.Views,
		})
		lg("重建保留(疑似已删): content=%q 赞=%d 评论=%d 图=%d", truncate(r.Content, 60), r.Likes, len(r.Comments), len(imgs))
	}
	return out
}

func tsToUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
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

func displayImages(m *msg, datauri map[string]string) []string {
	urls := picURLs(m)
	imgs := make([]string, 0, len(urls))
	for _, u := range urls {
		if src := datauri[u]; src != "" {
			imgs = append(imgs, src)
		}
	}
	return imgs
}

func imageDataURI(raw []byte) (string, bool) {
	if len(raw) <= 200 {
		return "", false
	}
	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), true
}

func (c *config) downloadImageCandidate(item imageCandidate) (string, string, error) {
	return downloadImageCandidateWith(item, c.httpGetImage)
}

func downloadImageCandidateWith(item imageCandidate, get func(string) ([]byte, error)) (string, string, error) {
	var lastErr error
	for i, u := range item.URLs {
		if strings.HasPrefix(u, "data:image/") {
			return u, u, nil
		}
		raw, err := get(u)
		if du, ok := imageDataURI(raw); err == nil && ok {
			if i > 0 {
				lg("图片候选回退成功 key=%s fallback=%s", maskURL(item.Key), maskURL(u))
			}
			return du, u, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("响应不是有效图片或过小 len=%d mime=%s", len(raw), http.DetectContentType(raw))
		}
		lg("图片候选失败 key=%s candidate=%s err=%v", maskURL(item.Key), maskURL(u), lastErr)
	}
	return "", "", lastErr
}

const defaultDownloadConcurrency = 16

func downloadConcurrency() int {
	raw := strings.TrimSpace(os.Getenv("QZONE_DOWNLOAD_CONCURRENCY"))
	if raw == "" {
		return defaultDownloadConcurrency
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultDownloadConcurrency
	}
	if n > 64 {
		return 64
	}
	return n
}

func validateConfig(cfg *config) bool {
	for attempt := 1; attempt <= 5; attempt++ {
		r, err := cfg.fetchPage(0, 1)
		if err == nil && r.Code == 0 {
			return true
		}
		if err == nil {
			lg("cookies 校验返回非0 code=%d message=%q", r.Code, r.Message)
			return false
		}
		lg("cookies 校验第%d/5次网络失败: %v", attempt, err)
		sleepBeforeRetry(attempt, 5, 2*time.Second)
	}
	return false
}

// pageCSS 极简卡片流样式。浅色为默认，深色通过 [data-theme=dark] 或（auto 时）
// prefers-color-scheme 覆盖 CSS 变量实现。
const pageCSS = `
:root{
  --bg:#f6f7f9; --surface:#ffffff; --text:#1c2024; --muted:#8a9099;
  --line:#e7e9ee; --accent:#e8833a; --like:#e8833a;
  --recon:#c9792f; --recon-bar:#e8a866; --recon-tint:#fbf6ef;
  --shadow:0 1px 2px rgba(20,24,30,.04);
}
:root[data-theme=dark]{
  --bg:#0f1115; --surface:#1a1d24; --text:#e6e8eb; --muted:#878e99;
  --line:#262b34; --accent:#f2a73b; --like:#f2a73b;
  --recon:#ffb784; --recon-bar:#7a4f2c; --recon-tint:#1f1813;
  --shadow:none;
}
@media (prefers-color-scheme:dark){
  :root:not([data-theme=light]){
    --bg:#0f1115; --surface:#1a1d24; --text:#e6e8eb; --muted:#878e99;
    --line:#262b34; --accent:#f2a73b; --like:#f2a73b;
    --recon:#ffb784; --recon-bar:#7a4f2c; --recon-tint:#1f1813;
    --shadow:none;
  }
}
*{box-sizing:border-box;}
body{margin:0;background:var(--bg);color:var(--text);line-height:1.62;
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
  -webkit-font-smoothing:antialiased;transition:background .2s,color .2s;}
.topbar{position:sticky;top:0;z-index:20;padding:14px 20px 12px;
  background:color-mix(in srgb,var(--bg) 85%,transparent);backdrop-filter:saturate(180%) blur(12px);
  border-bottom:1px solid var(--line);}
.bar-inner{max-width:640px;margin:0 auto;display:flex;align-items:center;justify-content:space-between;gap:12px;}
.titles h1{margin:0;font-size:17px;font-weight:650;letter-spacing:.2px;}
.titles .sub{margin:2px 0 0;font-size:12.5px;color:var(--muted);}
.theme-btn{width:38px;height:38px;flex:none;border:1px solid var(--line);border-radius:10px;
  background:var(--surface);color:var(--text);font-size:17px;cursor:pointer;line-height:1;
  display:flex;align-items:center;justify-content:center;transition:border-color .15s,transform .1s;}
.theme-btn:hover{border-color:var(--accent);} .theme-btn:active{transform:scale(.94);}
.search{display:block;max-width:640px;margin:12px auto 0;width:100%;padding:9px 14px;font-size:14px;
  border:1px solid var(--line);border-radius:11px;background:var(--surface);color:var(--text);outline:none;}
.search:focus{border-color:var(--accent);}
.wrap{max-width:640px;margin:0 auto;padding:22px 20px 60px;}
.zone{margin:0;} .zone+.zone{margin-top:10px;}
.zone-head{margin:26px 0 14px;padding-top:22px;border-top:1px dashed var(--line);}
.zone-head h2{margin:0;font-size:15px;font-weight:650;color:var(--recon);}
.zone-note{margin:6px 0 0;font-size:12.5px;color:var(--muted);}
.card{background:var(--surface);border:1px solid var(--line);border-radius:16px;
  padding:16px 18px;margin-bottom:14px;box-shadow:var(--shadow);}
.card.deleted{border-left:3px solid var(--recon-bar);background:var(--recon-tint);}
.card-head{display:flex;align-items:center;gap:8px;margin-bottom:8px;}
.date{color:var(--muted);font-size:12.5px;}
.badge{font-size:11.5px;color:var(--recon);border:1px solid var(--recon-bar);border-radius:6px;padding:1px 7px;}
.content{white-space:pre-wrap;word-break:break-word;font-size:15px;}
.content.empty{color:var(--muted);font-style:italic;}
.imgs{display:grid;gap:6px;margin-top:12px;}
.imgs.n1{grid-template-columns:minmax(0,72%);} .imgs.n2,.imgs.n4{grid-template-columns:repeat(2,1fr);}
.imgs.n3,.imgs.n-multi{grid-template-columns:repeat(3,1fr);}
.imgs img{width:100%;aspect-ratio:1;object-fit:cover;border-radius:10px;background:var(--line);cursor:zoom-in;}
.imgs.n1 img{aspect-ratio:auto;max-height:420px;width:auto;max-width:100%;object-fit:contain;}
.meta{display:flex;gap:16px;margin-top:12px;color:var(--muted);font-size:13px;}
.m-like{color:var(--like);}
.cmts{margin-top:12px;padding-top:10px;border-top:1px solid var(--line);font-size:13px;}
.cmt{color:var(--muted);margin:3px 0;} .cmt b{color:var(--text);font-weight:600;}
.cmt-faded{opacity:.7;}
.noresult{text-align:center;color:var(--muted);padding:40px 0;}
.hidden{display:none!important;}
#lb{display:none;position:fixed;inset:0;background:rgba(0,0,0,.92);z-index:9999;
  align-items:center;justify-content:center;cursor:zoom-out;padding:3vw;}
#lb img{max-width:96vw;max-height:96vh;object-fit:contain;border-radius:8px;}
`

// pageJS 主题三态切换（auto→light→dark→auto，记忆到 localStorage）+ 搜索过滤 + 图片灯箱
const pageJS = `
(function(){
  var root=document.documentElement,btn=document.getElementById('theme');
  var icons={auto:'🌓',light:'☀️',dark:'🌙'},order=['auto','light','dark'];
  function cur(){return root.getAttribute('data-theme')||'auto';}
  function render(){btn.textContent=icons[cur()]||'🌓';}
  render();
  btn.addEventListener('click',function(){
    var i=order.indexOf(cur()),next=order[(i+1)%order.length];
    root.setAttribute('data-theme',next);
    try{localStorage.setItem('qz-theme',next);}catch(e){}
    render();
  });
  var q=document.getElementById('q'),no=document.getElementById('noresult');
  var cards=[].slice.call(document.querySelectorAll('.card'));
  var zones=[].slice.call(document.querySelectorAll('.zone'));
  q.addEventListener('input',function(){
    var v=q.value.trim().toLowerCase(),hits=0;
    cards.forEach(function(c){
      var ok=!v||c.textContent.toLowerCase().indexOf(v)>=0;
      c.classList.toggle('hidden',!ok); if(ok)hits++;
    });
    zones.forEach(function(z){
      z.classList.toggle('hidden',!z.querySelector('.card:not(.hidden)'));
    });
    no.classList.toggle('hidden',hits>0);
  });
  document.addEventListener('click',function(e){
    if(e.target.tagName==='IMG'&&e.target.closest('.imgs')){
      document.getElementById('lbimg').src=e.target.src;
      document.getElementById('lb').style.display='flex';
    }
  });
  document.addEventListener('keydown',function(e){
    if(e.key==='Escape')document.getElementById('lb').style.display='none';
  });
})();
`

// buildHTML 生成自包含网页：现存说说 + 重建（疑似已删除）说说分两区展示，极简卡片流，
// 支持 深/浅/自动 主题切换、搜索、点击看大图。
func buildHTML(qq string, existing, deleted []msg, datauri map[string]string) string {
	eqq := esc(qq)
	var b strings.Builder
	// 头部：内联一段“尽早应用主题”的脚本，避免浅/深切换时的闪烁
	b.WriteString(`<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>QQ空间 · ` + eqq + `</title>`)
	b.WriteString(`<script>(function(){try{var t=localStorage.getItem('qz-theme')||'auto';document.documentElement.setAttribute('data-theme',t);}catch(e){}})();</script>`)
	b.WriteString(`<style>` + pageCSS + `</style></head><body>`)
	// 顶栏：标题 + 计数 + 搜索 + 主题切换按钮
	b.WriteString(`<header class="topbar"><div class="bar-inner">`)
	b.WriteString(`<div class="titles"><h1>QQ空间 · ` + eqq + `</h1>`)
	sub := fmt.Sprint(len(existing)) + ` 条现存说说`
	if len(deleted) > 0 {
		sub += ` · ` + fmt.Sprint(len(deleted)) + ` 条重建`
	}
	b.WriteString(`<p class="sub">` + sub + `</p></div>`)
	b.WriteString(`<button id="theme" class="theme-btn" title="切换 深色 / 浅色 / 跟随系统" aria-label="切换主题"></button>`)
	b.WriteString(`</div><input id="q" class="search" placeholder="搜索说说内容…"></header>`)

	b.WriteString(`<main class="wrap">`)

	// 现存说说区
	b.WriteString(`<section class="zone"><div class="feed">`)
	for i := range existing {
		renderCard(&b, &existing[i], datauri)
	}
	b.WriteString(`</div></section>`)

	// 重建（疑似已删除）区
	if len(deleted) > 0 {
		b.WriteString(`<section class="zone recon-zone"><div class="zone-head">`)
		b.WriteString(`<h2>🕯 互动痕迹还原 · 疑似已删除</h2>`)
		b.WriteString(`<p class="zone-note">以下 ` + fmt.Sprint(len(deleted)) + ` 条由你“动态”里的点赞/评论/浏览痕迹反推而来，可能不完整，也可能混入你对他人说说的互动，请自行甄别。</p>`)
		b.WriteString(`</div><div class="feed">`)
		for i := range deleted {
			renderCard(&b, &deleted[i], datauri)
		}
		b.WriteString(`</div></section>`)
	}

	b.WriteString(`<p id="noresult" class="noresult hidden">没有匹配的说说</p>`)
	b.WriteString(`</main>`)
	b.WriteString(`<div id="lb" onclick="this.style.display='none'"><img id="lbimg" src="" alt=""></div>`)
	b.WriteString(`<script>` + pageJS + `</script></body></html>`)
	return b.String()
}

// renderCard 渲染一张说说卡片（现存或重建）
func renderCard(b *strings.Builder, m *msg, datauri map[string]string) {
	cls := "card"
	if m.Deleted {
		cls = "card deleted"
	}
	b.WriteString(`<article class="` + cls + `"><div class="card-head"><span class="date">` + fmtTime(m.CreatedTime) + `</span>`)
	if m.Deleted {
		b.WriteString(`<span class="badge">🕯 疑似已删除</span>`)
	}
	b.WriteString(`</div>`)
	if c := esc(m.Content); c != "" {
		b.WriteString(`<div class="content">` + c + `</div>`)
	} else if m.Deleted {
		b.WriteString(`<div class="content empty">（无文字内容）</div>`)
	}
	imgs := displayImages(m, datauri)
	if len(imgs) > 0 {
		n := "n" + fmt.Sprint(len(imgs))
		if len(imgs) > 4 {
			n = "n-multi"
		}
		b.WriteString(`<div class="imgs ` + n + `">`)
		for _, src := range imgs {
			b.WriteString(`<img loading="lazy" src="` + src + `" alt="">`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`<div class="meta">`)
	if m.LikeTotal > 0 {
		b.WriteString(`<span class="m-like">♥ ` + fmt.Sprint(m.LikeTotal) + `</span>`)
	}
	if len(m.Commentlist) > 0 {
		b.WriteString(`<span>💬 ` + fmt.Sprint(len(m.Commentlist)) + `</span>`)
	}
	if m.Deleted && m.Views > 0 {
		b.WriteString(`<span>👁 ` + fmt.Sprint(m.Views) + `</span>`)
	}
	b.WriteString(`</div>`)
	if len(m.Commentlist) > 0 {
		b.WriteString(`<div class="cmts">`)
		for _, c := range m.Commentlist {
			if c.Content == "" {
				b.WriteString(`<div class="cmt"><b>` + esc(c.Name) + `</b> <span class="cmt-faded">评论过（原文无法找回）</span></div>`)
			} else {
				b.WriteString(`<div class="cmt"><b>` + esc(c.Name) + `</b>：` + esc(c.Content) + `</div>`)
			}
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</article>`)
}

func waitExit() {
	if runtime.GOOS == "windows" {
		fmt.Print("\n按回车键退出…")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

// runDemo 用假数据生成一份演示网页（仅供预览 UI / 主题切换，不联网、不登录）。
// 用法：QzoneExport --demo  -> 生成 QQ空间_demo.html 并打开。
func runDemo() {
	t := func(y int, mo time.Month, d, h, mi int) int64 {
		return time.Date(y, mo, d, h, mi, 0, 0, time.Local).Unix()
	}
	// 1x1 透明/彩色像素 data URI，免联网即可看图片网格效果
	px := func(hex string) string {
		return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="120" height="120"><rect width="120" height="120" fill="#`+hex+`"/></svg>`))
	}
	existing := []msg{
		{Content: "终场哨响的那一刻，我脑海里闪过了太多画面。这一年我们一起熬过的夜、跑过的操场，都值了。", CreatedTime: t(2022, 10, 5, 14, 30), LikeTotal: 24,
			Commentlist: []comment{{Name: "小吴", Content: "包是你的"}, {Name: "懒大王", Content: "加油"}},
			Pic:         []pic{{URL: px("e8833a")}, {URL: px("4a90d9")}, {URL: px("6fae6f")}}},
		{Content: "qq也更新一下，新头像求赞 😎", CreatedTime: t(2022, 9, 12, 9, 5), LikeTotal: 12},
		{Content: "回顾经典", CreatedTime: t(2022, 6, 1, 20, 0), LikeTotal: 7, Pic: []pic{{URL: px("d96b6b")}}},
	}
	deleted := []msg{
		{Content: "好久不见", CreatedTime: t(2021, 12, 5, 18, 0), LikeTotal: 5, Views: 33, Deleted: true,
			Commentlist: []comment{{Name: "小莉"}, {Name: "男子汉"}}},
		{Content: "", CreatedTime: t(2021, 8, 20, 11, 0), LikeTotal: 2, Views: 10, Deleted: true,
			ExtraImgs: []string{px("9b8cd9")}},
		{Content: "青春才几年，你仨占七年", CreatedTime: 0, LikeTotal: 9, Deleted: true,
			Commentlist: []comment{{Name: "fantastic"}}},
	}
	// datauri：现存走 Pic.URL（经 toOriginal 后即原串），重建走 ExtraImgs，均已是 data URI
	datauri := map[string]string{}
	all := append(append([]msg{}, existing...), deleted...)
	for i := range all {
		for _, u := range picURLs(&all[i]) {
			datauri[u] = u
		}
	}
	doc := buildHTML("演示账号", existing, deleted, datauri)
	out := filepath.Join(exeDir(), "QQ空间_demo.html")
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		fmt.Println("写入失败:", err)
		return
	}
	fmt.Println("已生成演示网页:", out)
	openInSystem(out)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--demo" {
		runDemo()
		return
	}
	fmt.Println("================================================")
	fmt.Printf("        QQ空间说说导出工具  v%s\n", version)
	fmt.Println("================================================")
	dir := exeDir()
	logPath := initLog(dir)
	if logPath != "" {
		fmt.Printf("📝 详细日志: %s\n", logPath)
	}
	defer closeLog()

	cfg := loadConfig(dir)
	if cfg != nil {
		lg("检测到本地 cookies.json，校验登录态…")
		fmt.Println("检测到本地 cookies.json，正在校验登录态…")
		if validateConfig(cfg) {
			say("✅ 检测到有效登录（QQ %s），跳过扫码", cfg.QQ)
		} else {
			fmt.Println("ℹ️ 旧登录已过期，需要重新扫码")
			cfg = nil
		}
	}
	if cfg == nil {
		cfg = login(dir)
	}

	say("\n开始抓取 QQ %s 的全部说说…", cfg.QQ)
	firstPage, err := cfg.fetchPage(0, 20)
	if err != nil || firstPage.Code != 0 {
		code := -1
		if firstPage != nil {
			code = firstPage.Code
		}
		say("❌ 抓取失败：err=%v code=%d（详见日志）", err, code)
		waitExit()
		os.Exit(1)
	}
	total := firstPage.Total
	say("共有 %d 条现存说说", total)
	msgs := append([]msg{}, firstPage.Msglist...)
	pos := len(msgs)
	for pos < total {
		page, err := cfg.fetchPage(pos, 20)
		if err != nil {
			time.Sleep(2 * time.Second)
			page, err = cfg.fetchPage(pos, 20)
			if err != nil {
				lg("现存说说翻页在 pos=%d 中断: %v", pos, err)
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
	existingCount := len(msgs)
	say("\n实际抓到现存说说 %d 条", existingCount)

	// ---------- 重建已删除说说 ----------
	fmt.Println("\n开始扫描动态流，尝试重建已删除的说说…")
	lg("==== 进入重建阶段 ====")
	acts := cfg.fetchAllActivities()
	say("\n动态流共扫描到 %d 条互动记录", len(acts))
	recons := reconstructMoments(acts)
	lg("聚合成 %d 条候选说说", len(recons))
	deleted := reconAsDeletedMsgs(recons, msgs)
	say("重建出 %d 条疑似已删除的说说（已排除现存）", len(deleted))

	// 收集图片（现存 + 重建）
	seen := map[string]bool{}
	var images []imageCandidate
	collect := func(list []msg) {
		for i := range list {
			for _, item := range imageCandidates(&list[i]) {
				if !seen[item.Key] {
					seen[item.Key] = true
					images = append(images, item)
				}
			}
		}
	}
	collect(msgs)
	collect(deleted)

	concurrency := downloadConcurrency()
	fmt.Printf("下载 %d 张图片（原图画质，并发 %d，失败自动尝试备用地址）…\n", len(images), concurrency)
	lg("待下载图片 %d 张，并发 %d", len(images), concurrency)
	datauri := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	var done int
	var dmu sync.Mutex
	for _, item := range images {
		wg.Add(1)
		sem <- struct{}{}
		go func(item imageCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			du, used, err := cfg.downloadImageCandidate(item)
			dmu.Lock()
			done++
			fmt.Printf("  %d/%d\r", done, len(images))
			dmu.Unlock()
			if err == nil {
				mu.Lock()
				datauri[item.Key] = du
				mu.Unlock()
				if used != item.Key {
					lg("图片使用备用地址 key=%s used=%s", maskURL(item.Key), maskURL(used))
				}
			} else {
				lg("图片下载失败 key=%s candidates=%d err=%v", maskURL(item.Key), len(item.URLs), err)
			}
		}(item)
	}
	wg.Wait()
	say("\n图片完成：成功 %d/%d", len(datauri), len(images))

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedTime > msgs[j].CreatedTime })
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].CreatedTime > deleted[j].CreatedTime })
	doc := buildHTML(cfg.QQ, msgs, deleted, datauri)
	outPath := filepath.Join(dir, "QQ空间_"+cfg.QQ+".html")
	if err := os.WriteFile(outPath, []byte(doc), 0600); err != nil {
		say("❌ 写入网页失败: %v", err)
		waitExit()
		os.Exit(1)
	}
	fi, _ := os.Stat(outPath)
	say("\n✅ 完成！已生成 %s（%.1f MB）", outPath, float64(fi.Size())/1024/1024)
	say("   现存说说 %d 条 + 重建疑似已删除 %d 条 = 共 %d 条", existingCount, len(deleted), existingCount+len(deleted))
	fmt.Println("   正在用默认浏览器打开…")
	openInSystem(outPath)
	if len(deleted) == 0 {
		fmt.Println("\nℹ️ 本次没有重建出已删除的说说。原因可能是：没有被互动过的已删说说，或动态流已不再保留相关痕迹（详见日志）。")
	}
	fmt.Println("\n⚠️ 提示：cookies.json 含你的登录凭证，用完建议删除，切勿发给他人。")
	fmt.Println("如仍有问题，请把同目录的日志文件发给开发者排查。")
	waitExit()
}
