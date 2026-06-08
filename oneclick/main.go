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
const version = "1.1"

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
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Referer", "https://user.qzone.qq.com/"+c.QQ)
		req.Header.Set("Cookie", c.cookieHeader())
		client := &http.Client{Timeout: 35 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			lg("httpGet 第%d次失败 url=%s err=%v", attempt, maskURL(u), err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			lg("httpGet 第%d次读取失败 url=%s err=%v", attempt, maskURL(u), rerr)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		return body, nil
	}
	return nil, lastErr
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
	// 重建出来的说说图片走 ExtraImgs（已是直链），现存说说走 pic 结构
	if m.Deleted {
		return m.ExtraImgs
	}
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

// ---------- 动态流抓取 + 解析 + 重建 ----------

// processOldHTML 复刻主程序 utils.ProcessOldHTML：从 JSONP 响应里取出 html:'...' 段，
// 解码 \xHH，去掉转义反斜杠。
func processOldHTML(message string) string {
	re := regexp.MustCompile(`\\x[0-9a-fA-F]{2}`)
	newText := re.ReplaceAllStringFunc(message, func(h string) string {
		b, err := strconv.ParseUint(h[2:], 16, 8)
		if err != nil {
			return h
		}
		return string(rune(b))
	})
	const startString = "html:'"
	const endString = "',opuin"
	si := strings.Index(newText, startString)
	if si < 0 {
		return ""
	}
	si += len(startString)
	ei := strings.Index(newText[si:], endString)
	if ei < 0 {
		return ""
	}
	newText = newText[si : si+ei]
	newText = regexp.MustCompile(`\s+`).ReplaceAllString(newText, " ")
	newText = strings.ReplaceAll(newText, "\\", "")
	return newText
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

// fetchAllActivities 翻页抓取整条动态流
func (c *config) fetchAllActivities() []activity {
	var all []activity
	const pageSize = 100
	const maxPages = 200 // 安全上限，避免异常时无限翻页
	empty := 0
	for page := 0; page < maxPages; page++ {
		offset := page * pageSize
		u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/ic2.qzone.qq.com/cgi-bin/feeds/feeds2_html_pav_all?uin=%s&begin_time=0&end_time=0&getappnotification=1&getnotifi=1&has_get_key=0&offset=%d&set=0&count=%d&useutf8=1&outputhtmlfeed=1&scope=1&format=jsonp&g_tk=%s",
			c.QQ, offset, pageSize, string(c.GTK))
		body, err := c.httpGet(u)
		if err != nil {
			lg("fetchAllActivities offset=%d 请求失败: %v", offset, err)
			break
		}
		processed := processOldHTML(string(body))
		if page == 0 {
			lg("动态流首页 原始响应(截断2000字): %s", truncate(string(body), 2000))
			lg("动态流首页 处理后HTML(截断2000字): %s", truncate(processed, 2000))
		}
		if processed == "" || !strings.Contains(processed, "f-single") {
			empty++
			lg("fetchAllActivities offset=%d 无活动条目(empty=%d)", offset, empty)
			if empty >= 2 {
				break
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		empty = 0
		acts := c.parseActivities(processed)
		lg("fetchAllActivities offset=%d 解析到 %d 条活动", offset, len(acts))
		if len(acts) == 0 {
			break
		}
		all = append(all, acts...)
		fmt.Printf("  动态流已扫 %d 条互动\r", len(all))
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

func buildHTML(qq string, msgs []msg, datauri map[string]string) string {
	eqq := esc(qq)
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>QQ空间 · ` + eqq + `</title><style>
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
.card.deleted{border-color:#5a3a2a;background:#221a17;}
.badge{display:inline-block;font-size:12px;color:#ffb784;background:#3a2a1f;border:1px solid #5a3a2a;border-radius:6px;padding:1px 7px;margin-left:8px;vertical-align:middle;}
.recon-note{color:var(--muted);font-size:12px;margin-top:6px;font-style:italic;}
#lb{display:none;position:fixed;inset:0;background:rgba(0,0,0,.93);z-index:9999;align-items:center;justify-content:center;cursor:zoom-out;}
#lb img{max-width:96vw;max-height:96vh;object-fit:contain;border-radius:6px;}
</style></head><body>`)
	delCount := 0
	for i := range msgs {
		if msgs[i].Deleted {
			delCount++
		}
	}
	title := `共 ` + fmt.Sprint(len(msgs)) + ` 条`
	if delCount > 0 {
		title += `（含 ` + fmt.Sprint(delCount) + ` 条🕯重建·疑似已删除）`
	}
	b.WriteString(`<div class="topbar"><h1>QQ空间 · ` + eqq + ` · ` + title + `</h1><input id="q" placeholder="搜索内容…"></div>`)
	b.WriteString(`<div class="wrap" id="feed">`)
	for i := range msgs {
		m := &msgs[i]
		cls := "card"
		if m.Deleted {
			cls = "card deleted"
		}
		b.WriteString(`<article class="` + cls + `"><header><span class="date">` + fmtTime(m.CreatedTime) + `</span>`)
		if m.Deleted {
			b.WriteString(`<span class="badge">🕯 重建 · 疑似已删除</span>`)
		}
		b.WriteString(`</header>`)
		b.WriteString(`<div class="content">` + esc(m.Content) + `</div>`)
		if m.Deleted {
			b.WriteString(`<div class="recon-note">此条由互动痕迹还原，内容可能不完整</div>`)
		}
		urls := picURLs(m)
		if len(urls) > 0 {
			b.WriteString(`<div class="imgs">`)
			for _, u := range urls {
				src := datauri[u]
				if src == "" {
					src = esc(u) // 下载失败时回退到原链接（转义，避免属性注入）
				}
				b.WriteString(`<img loading="lazy" src="` + src + `">`)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`<footer><span class="like">❤ ` + fmt.Sprint(m.LikeTotal) + `</span>`)
		if len(m.Commentlist) > 0 {
			b.WriteString(`<span>💬 ` + fmt.Sprint(len(m.Commentlist)) + `</span>`)
		}
		if m.Deleted && m.Views > 0 {
			b.WriteString(`<span>👁 ` + fmt.Sprint(m.Views) + `</span>`)
		}
		b.WriteString(`</footer>`)
		if len(m.Commentlist) > 0 {
			b.WriteString(`<div class="cmts">`)
			for _, c := range m.Commentlist {
				if c.Content == "" {
					// 重建评论：只有评论者昵称，没有评论原文
					b.WriteString(`<div class="cmt"><b>` + esc(c.Name) + `</b> 评论过（原文无法找回）</div>`)
				} else {
					b.WriteString(`<div class="cmt"><b>` + esc(c.Name) + `</b>：` + esc(c.Content) + `</div>`)
				}
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
		if r, err := cfg.fetchPage(0, 1); err == nil && r.Code == 0 {
			say("✅ 检测到有效登录（QQ %s），跳过扫码", cfg.QQ)
		} else {
			lg("本地登录失效 err=%v", err)
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
	msgs = append(msgs, deleted...)

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
	lg("待下载图片 %d 张", len(urls))
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
				mime := http.DetectContentType(raw)
				if !strings.HasPrefix(mime, "image/") {
					mime = "image/jpeg" // 嗅探失败时回退
				}
				du := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
				mu.Lock()
				datauri[u] = du
				mu.Unlock()
			} else {
				lg("图片下载失败/过小 url=%s err=%v len=%d", u, err, len(raw))
			}
		}(u)
	}
	wg.Wait()
	say("\n图片完成：成功 %d/%d", len(datauri), len(urls))

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedTime > msgs[j].CreatedTime })
	doc := buildHTML(cfg.QQ, msgs, datauri)
	outPath := filepath.Join(dir, "QQ空间_"+cfg.QQ+".html")
	if err := os.WriteFile(outPath, []byte(doc), 0600); err != nil {
		say("❌ 写入网页失败: %v", err)
		waitExit()
		os.Exit(1)
	}
	fi, _ := os.Stat(outPath)
	say("\n✅ 完成！已生成 %s（%.1f MB）", outPath, float64(fi.Size())/1024/1024)
	say("   现存说说 %d 条 + 重建疑似已删除 %d 条 = 共 %d 条", existingCount, len(deleted), len(msgs))
	fmt.Println("   正在用默认浏览器打开…")
	openInSystem(outPath)
	if len(deleted) == 0 {
		fmt.Println("\nℹ️ 本次没有重建出已删除的说说。原因可能是：没有被互动过的已删说说，或动态流已不再保留相关痕迹（详见日志）。")
	}
	fmt.Println("\n⚠️ 提示：cookies.json 含你的登录凭证，用完建议删除，切勿发给他人。")
	fmt.Println("如仍有问题，请把同目录的日志文件发给开发者排查。")
	waitExit()
}
