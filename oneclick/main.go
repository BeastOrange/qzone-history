// QQ空间说说一键导出（零依赖单文件版）
// 编译: go build -o qzone.exe   （或交叉编译为各平台）
// 运行: 双击即可。扫码登录 -> 抓取全部现存说说 -> 原图内嵌 -> 自动用浏览器打开。
package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/png"
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
const version = "1.6"

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

func qrPixelDark(img image.Image, x, y int) bool {
	bounds := img.Bounds()
	if x < bounds.Min.X || x >= bounds.Max.X || y < bounds.Min.Y || y >= bounds.Max.Y {
		return false
	}
	r, g, b, a := img.At(x, y).RGBA()
	if a < 0x8000 {
		return false
	}
	luma := (r*299 + g*587 + b*114) / 1000
	return luma < 0x8000
}

func qrTerminalScale(width int) int {
	const targetColumns = 72
	if width <= targetColumns {
		return 1
	}
	scale := (width + targetColumns - 1) / targetColumns
	if scale < 1 {
		return 1
	}
	return scale
}

func qrBlockDark(img image.Image, x, y, scale int) bool {
	total, dark := 0, 0
	for yy := y; yy < y+scale; yy++ {
		for xx := x; xx < x+scale; xx++ {
			total++
			if qrPixelDark(img, xx, yy) {
				dark++
			}
		}
	}
	return dark*2 >= total
}

func printQRInTerminal(w io.Writer, pngBytes []byte) error {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return err
	}
	bounds := img.Bounds()
	const quietZone = 2
	scale := qrTerminalScale(bounds.Dx())
	padding := quietZone * scale
	for y := bounds.Min.Y - padding; y < bounds.Max.Y+padding; y += 2 * scale {
		for x := bounds.Min.X - padding; x < bounds.Max.X+padding; x += scale {
			topDark := qrBlockDark(img, x, y, scale)
			bottomDark := qrBlockDark(img, x, y+scale, scale)
			switch {
			case topDark && bottomDark:
				_, _ = io.WriteString(w, "\x1b[30;40m▀")
			case topDark:
				_, _ = io.WriteString(w, "\x1b[30;47m▀")
			case bottomDark:
				_, _ = io.WriteString(w, "\x1b[37;40m▀")
			default:
				_, _ = io.WriteString(w, "\x1b[37;47m▀")
			}
		}
		_, _ = io.WriteString(w, "\x1b[0m\n")
	}
	_, _ = io.WriteString(w, "\x1b[0m")
	return nil
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

const outputDirName = "QzoneExport_output"

// outputDir 返回 exe 同层级的统一输出目录。所有运行产物（cookies、日志、HTML、缓存、图片）
// 都放在这里，避免把 exe 同目录弄乱，也避免把大量图片内嵌进一个超大 HTML。
func outputDir() string {
	base := exeDir()
	out := filepath.Join(base, outputDirName)
	if err := os.MkdirAll(out, 0700); err != nil {
		return base
	}
	// 兼容旧版本：如果 exe 同级已有 cookies.json，首次运行自动复用到输出目录。
	oldCookie := filepath.Join(base, "cookies.json")
	newCookie := filepath.Join(out, "cookies.json")
	if _, err := os.Stat(newCookie); os.IsNotExist(err) {
		if b, err := os.ReadFile(oldCookie); err == nil {
			_ = os.WriteFile(newCookie, b, 0600)
		}
	}
	return out
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

type probeWindow struct {
	Label string
	Begin int64
	End   int64
}

type probeRecord struct {
	Type             string `json:"type"`
	Strategy         string `json:"strategy"`
	Window           string `json:"window,omitempty"`
	WindowBegin      int64  `json:"window_begin,omitempty"`
	WindowEnd        int64  `json:"window_end,omitempty"`
	Offset           int    `json:"offset"`
	Count            int    `json:"count"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	LatencyMS        int64  `json:"latency_ms"`
	BodySize         int    `json:"body_size"`
	ProcessedSize    int    `json:"processed_size"`
	HasTotalNumber   bool   `json:"has_total_number"`
	TotalNumber      int    `json:"total_number,omitempty"`
	HasHTMLSegment   bool   `json:"has_html_segment"`
	HasFSingle       bool   `json:"has_f_single"`
	ParsedCount      int    `json:"parsed_count"`
	MinTimeText      string `json:"min_time_text,omitempty"`
	MaxTimeText      string `json:"max_time_text,omitempty"`
	MinTimestampUnix int64  `json:"min_timestamp_unix,omitempty"`
	MaxTimestampUnix int64  `json:"max_timestamp_unix,omitempty"`
}

type activityRecord struct {
	Type             string   `json:"type"`
	Strategy         string   `json:"strategy"`
	Window           string   `json:"window,omitempty"`
	WindowBegin      int64    `json:"window_begin,omitempty"`
	WindowEnd        int64    `json:"window_end,omitempty"`
	Offset           int      `json:"offset"`
	Index            int      `json:"index"`
	SenderQQ         string   `json:"sender_qq,omitempty"`
	SenderName       string   `json:"sender_name,omitempty"`
	ReceiverQQ       string   `json:"receiver_qq,omitempty"`
	Content          string   `json:"content,omitempty"`
	TimeText         string   `json:"time_text,omitempty"`
	TimestampUnix    int64    `json:"timestamp_unix,omitempty"`
	ImageURLs        []string `json:"image_urls,omitempty"`
	ActivityType     string   `json:"activity_type"`
	ActivityTypeCode int      `json:"activity_type_code"`
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
	fmt.Println("✅ 二维码已生成，请用【手机QQ】扫描下方二维码并在手机上点击确认登录…")
	if err := printQRInTerminal(os.Stdout, png); err != nil {
		say("❌ 终端二维码显示失败：%v", err)
		waitExit()
		os.Exit(1)
	}
	lg("login: 已在终端展示二维码")

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

func (c *config) feedURL(begin, end int64, offset, count int) string {
	return fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/ic2.qzone.qq.com/cgi-bin/feeds/feeds2_html_pav_all?uin=%s&begin_time=%d&end_time=%d&getappnotification=1&getnotifi=1&has_get_key=0&offset=%d&set=0&count=%d&useutf8=1&outputhtmlfeed=1&scope=1&format=jsonp&g_tk=%s",
		c.QQ, begin, end, offset, count, string(c.GTK))
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
	// ExtraImgs 是图片直链（重建说说、相册照片都走这里）；只要有就优先用，与 Deleted 无关
	if len(m.ExtraImgs) > 0 {
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
		body, err := c.httpGet(c.feedURL(0, 0, offset, pageSize))
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

func activityTypeName(t activityType) string {
	switch t {
	case typeMoment:
		return "moment"
	case typeForward:
		return "forward"
	case typeLike:
		return "like"
	case typeComment:
		return "comment"
	case typeBoardMsg:
		return "board_message"
	case typeReply:
		return "reply"
	case typeView:
		return "view"
	default:
		return "other"
	}
}

func activityTypeFromRecord(rec activityRecord) activityType {
	switch rec.ActivityType {
	case "moment":
		return typeMoment
	case "forward":
		return typeForward
	case "like":
		return typeLike
	case "comment":
		return typeComment
	case "board_message":
		return typeBoardMsg
	case "reply":
		return typeReply
	case "view":
		return typeView
	case "other":
		return typeOther
	}
	if rec.ActivityTypeCode >= int(typeMoment) && rec.ActivityTypeCode <= int(typeOther) {
		return activityType(rec.ActivityTypeCode)
	}
	return typeOther
}

// ---------- 深度探查实验 ----------

const probePageSize = 100

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func monthWindow(year int, month time.Month) probeWindow {
	begin := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	end := begin.AddDate(0, 1, 0)
	return probeWindow{
		Label: fmt.Sprintf("%04d-%02d", year, int(month)),
		Begin: begin.Unix(),
		End:   end.Unix(),
	}
}

func defaultProbeWindows() []probeWindow {
	return []probeWindow{
		monthWindow(2017, time.September),
		monthWindow(2017, time.August),
		monthWindow(2016, time.January),
		monthWindow(2015, time.January),
		monthWindow(2015, time.June),
		monthWindow(2015, time.December),
	}
}

func exponentialOffsets(maxOffset int) []int {
	if maxOffset < 0 {
		maxOffset = 0
	}
	offsets := []int{0, 100}
	for next := 200; next < maxOffset; next *= 2 {
		offsets = append(offsets, next)
	}
	if offsets[len(offsets)-1] != maxOffset {
		offsets = append(offsets, maxOffset)
	}
	out := offsets[:0]
	seen := map[int]bool{}
	for _, offset := range offsets {
		if offset <= maxOffset && !seen[offset] {
			seen[offset] = true
			out = append(out, offset)
		}
	}
	return out
}

func totalNumber(raw []byte) (int, bool) {
	m := totalNumRe.FindSubmatch(raw)
	if m == nil {
		return 0, false
	}
	total, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return total, true
}

func activityTimeRange(acts []activity) (string, string, int64, int64) {
	if len(acts) == 0 {
		return "", "", 0, 0
	}
	minIdx, maxIdx := -1, -1
	var minTime, maxTime time.Time
	for i, act := range acts {
		if act.Timestamp.IsZero() {
			continue
		}
		if minIdx < 0 || act.Timestamp.Before(minTime) {
			minIdx = i
			minTime = act.Timestamp
		}
		if maxIdx < 0 || act.Timestamp.After(maxTime) {
			maxIdx = i
			maxTime = act.Timestamp
		}
	}
	if minIdx < 0 {
		return "", "", 0, 0
	}
	return acts[minIdx].TimeText, acts[maxIdx].TimeText, minTime.Unix(), maxTime.Unix()
}

type probeResult struct {
	Record     probeRecord
	Activities []activity
	RawBody    []byte
}

// analyzeFeedBody 把一段原始动态流响应解析成 probeRecord（不发网络请求）。
// harvest 缓存命中和 --analyze-cache 离线分析都复用它，保证口径与在线探测完全一致。
func analyzeFeedBody(c *config, window probeWindow, offset int, strategy string, body []byte) probeRecord {
	rawText := string(body)
	processed := processOldHTML(rawText)
	acts := c.parseActivities(processed)
	total, ok := totalNumber(body)
	minText, maxText, minUnix, maxUnix := activityTimeRange(acts)
	return probeRecord{
		Type:             "request",
		Strategy:         strategy,
		Window:           window.Label,
		WindowBegin:      window.Begin,
		WindowEnd:        window.End,
		Offset:           offset,
		Count:            harvestPageSize,
		Status:           "ok",
		BodySize:         len(body),
		ProcessedSize:    len(processed),
		HasTotalNumber:   ok,
		TotalNumber:      total,
		HasHTMLSegment:   strings.Contains(rawText, "html:'"),
		HasFSingle:       strings.Contains(processed, "f-single"),
		ParsedCount:      len(acts),
		MinTimeText:      minText,
		MaxTimeText:      maxText,
		MinTimestampUnix: minUnix,
		MaxTimestampUnix: maxUnix,
	}
}

func (c *config) probeFeed(window probeWindow, offset, count int, strategy string) probeRecord {
	return c.probeFeedWithRetry(window, offset, count, strategy, 3, time.Second)
}

func (c *config) probeFeedWithRetry(window probeWindow, offset, count int, strategy string, attempts int, baseDelay time.Duration) probeRecord {
	return c.probeFeedResultWithRetry(window, offset, count, strategy, attempts, baseDelay).Record
}

func (c *config) probeFeedResultWithRetry(window probeWindow, offset, count int, strategy string, attempts int, baseDelay time.Duration) probeResult {
	start := time.Now()
	u := c.feedURL(window.Begin, window.End, offset, count)
	body, err := c.httpGetWithRetry(u, attempts, baseDelay)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		rec := probeRecord{
			Type:        "request",
			Strategy:    strategy,
			Window:      window.Label,
			WindowBegin: window.Begin,
			WindowEnd:   window.End,
			Offset:      offset,
			Count:       count,
			LatencyMS:   latency,
			Status:      "error",
			Error:       err.Error(),
		}
		return probeResult{Record: rec}
	}
	rec := analyzeFeedBody(c, window, offset, strategy, body)
	rec.Count = count
	rec.LatencyMS = latency
	return probeResult{Record: rec, Activities: c.parseActivities(processOldHTML(string(body))), RawBody: body}
}

func writeProbeRecord(w io.Writer, rec probeRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func makeActivityRecord(window probeWindow, offset int, strategy string, index int, act activity) activityRecord {
	return activityRecord{
		Type:             "activity",
		Strategy:         strategy,
		Window:           window.Label,
		WindowBegin:      window.Begin,
		WindowEnd:        window.End,
		Offset:           offset,
		Index:            index,
		SenderQQ:         act.SenderQQ,
		SenderName:       act.SenderName,
		ReceiverQQ:       act.ReceiverQQ,
		Content:          act.Content,
		TimeText:         act.TimeText,
		TimestampUnix:    tsToUnix(act.Timestamp),
		ImageURLs:        append([]string(nil), act.ImageURLs...),
		ActivityType:     activityTypeName(act.Type),
		ActivityTypeCode: int(act.Type),
	}
}

func writeActivityRecord(w io.Writer, rec activityRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func writeActivityRecords(w io.Writer, window probeWindow, offset int, strategy string, acts []activity) error {
	for i, act := range acts {
		if err := writeActivityRecord(w, makeActivityRecord(window, offset, strategy, i, act)); err != nil {
			return err
		}
	}
	return nil
}

type probeAnalysis struct {
	Path                  string
	RequestCount          int
	ErrorCount            int
	ActivityRecordCount   int
	ParsedActivityCount   int
	EarliestTimeText      string
	EarliestTimestampUnix int64
	EarliestWindow        string
	FoundBefore20170903   bool
	Found2015             bool
	ErrorCounts           map[string]int
}

var probeLogTimeRe = regexp.MustCompile(`probe_(?:depth|offset_range|offset_list)_(\d{8}_\d{6})\.jsonl$`)

func probeLogTimeKey(path string) string {
	m := probeLogTimeRe.FindStringSubmatch(filepath.Base(path))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func latestProbePath(dir string) (string, error) {
	patterns := []string{"probe_depth_*.jsonl", "probe_offset_range_*.jsonl", "probe_offset_list_*.jsonl"}
	var matches []string
	for _, pattern := range patterns {
		items, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return "", err
		}
		matches = append(matches, items...)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("未找到 probe_depth_*.jsonl、probe_offset_range_*.jsonl 或 probe_offset_list_*.jsonl")
	}
	sort.Slice(matches, func(i, j int) bool {
		ki, kj := probeLogTimeKey(matches[i]), probeLogTimeKey(matches[j])
		if ki != kj {
			return ki < kj
		}
		return matches[i] < matches[j]
	})
	return matches[len(matches)-1], nil
}

func probePathFromArgs(args []string, dir string) (string, error) {
	for i, arg := range args {
		if arg != "--analyze-probe" {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			return args[i+1], nil
		}
		return latestProbePath(dir)
	}
	return "", fmt.Errorf("缺少 --analyze-probe")
}

func analyzeProbeFile(path string) (probeAnalysis, error) {
	f, err := os.Open(path)
	if err != nil {
		return probeAnalysis{}, err
	}
	defer f.Close()

	out := probeAnalysis{Path: path, ErrorCounts: map[string]int{}}
	cutoff := time.Date(2017, time.September, 3, 0, 0, 0, 0, time.Local).Unix()
	start2015 := time.Date(2015, time.January, 1, 0, 0, 0, 0, time.Local).Unix()
	end2016 := time.Date(2016, time.January, 1, 0, 0, 0, 0, time.Local).Unix()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	observeTime := func(ts int64, text, window string) {
		if ts == 0 {
			return
		}
		if out.EarliestTimestampUnix == 0 || ts < out.EarliestTimestampUnix {
			out.EarliestTimestampUnix = ts
			out.EarliestTimeText = text
			out.EarliestWindow = window
		}
		if ts < cutoff {
			out.FoundBefore20170903 = true
		}
		if ts >= start2015 && ts < end2016 {
			out.Found2015 = true
		}
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &kind); err != nil {
			return out, fmt.Errorf("解析 JSONL 失败: %w", err)
		}
		if kind.Type == "activity" {
			var rec activityRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return out, fmt.Errorf("解析 activity JSONL 失败: %w", err)
			}
			out.ActivityRecordCount++
			observeTime(rec.TimestampUnix, rec.TimeText, rec.Window)
			continue
		}
		if kind.Type != "" && kind.Type != "request" {
			continue
		}
		var rec probeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return out, fmt.Errorf("解析 request JSONL 失败: %w", err)
		}
		out.RequestCount++
		if rec.Status == "error" {
			out.ErrorCount++
			out.ErrorCounts[rec.Error]++
		}
		out.ParsedActivityCount += rec.ParsedCount
		if rec.ParsedCount <= 0 || rec.MinTimestampUnix == 0 {
			continue
		}
		observeTime(rec.MinTimestampUnix, rec.MinTimeText, rec.Window)
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func runAnalyzeProbe(args []string, dir string) error {
	path, err := probePathFromArgs(args, dir)
	if err != nil {
		return err
	}
	result, err := analyzeProbeFile(path)
	if err != nil {
		return err
	}
	say("探查日志: %s", result.Path)
	say("请求数=%d 错误数=%d 解析活动数=%d", result.RequestCount, result.ErrorCount, result.ParsedActivityCount)
	if result.ActivityRecordCount > 0 {
		say("活动明细记录=%d（可用于 --preview-year）", result.ActivityRecordCount)
	}
	if len(result.ErrorCounts) > 0 {
		parts := make([]string, 0, len(result.ErrorCounts))
		for k, v := range result.ErrorCounts {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(parts)
		say("错误类型: %s", strings.Join(parts, ", "))
	}
	if result.EarliestTimestampUnix > 0 {
		say("最早活动: %s（窗口 %s，Unix=%d）", result.EarliestTimeText, result.EarliestWindow, result.EarliestTimestampUnix)
	} else {
		say("最早活动: 未解析到带时间的活动")
	}
	say("是否突破 2017-09-03: %v", result.FoundBefore20170903)
	say("是否发现 2015 年活动: %v", result.Found2015)
	return nil
}

func previewYearFromArgs(args []string) (int, error) {
	for i, arg := range args {
		if arg != "--preview-year" {
			continue
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return 0, fmt.Errorf("缺少 --preview-year 后的年份")
		}
		year, err := strconv.Atoi(args[i+1])
		if err != nil || year < 2005 || year > time.Now().Year()+1 {
			return 0, fmt.Errorf("年份参数无效: %q", args[i+1])
		}
		return year, nil
	}
	return 0, fmt.Errorf("缺少 --preview-year")
}

func previewProbePathFromArgs(args []string, dir string) (string, error) {
	for i, arg := range args {
		if arg != "--preview-year" {
			continue
		}
		if i+2 < len(args) && !strings.HasPrefix(args[i+2], "--") {
			return args[i+2], nil
		}
		return latestProbePath(dir)
	}
	return "", fmt.Errorf("缺少 --preview-year")
}

func activityFromRecord(rec activityRecord) activity {
	var ts time.Time
	if rec.TimestampUnix > 0 {
		ts = time.Unix(rec.TimestampUnix, 0)
	}
	return activity{
		SenderQQ:   rec.SenderQQ,
		SenderName: rec.SenderName,
		ReceiverQQ: rec.ReceiverQQ,
		Content:    rec.Content,
		Timestamp:  ts,
		TimeText:   rec.TimeText,
		ImageURLs:  append([]string(nil), rec.ImageURLs...),
		Type:       activityTypeFromRecord(rec),
	}
}

func readActivityRecords(path string) ([]activityRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []activityRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &kind); err != nil {
			return nil, fmt.Errorf("解析 JSONL 失败: %w", err)
		}
		if kind.Type != "activity" {
			continue
		}
		var rec activityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("解析 activity JSONL 失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func activityDedupeKey(a activity) string {
	return strings.Join([]string{
		a.SenderQQ,
		a.SenderName,
		a.ReceiverQQ,
		a.Content,
		a.TimeText,
		strconv.FormatInt(tsToUnix(a.Timestamp), 10),
		strconv.Itoa(int(a.Type)),
		strings.Join(a.ImageURLs, "|"),
	}, "\x00")
}

func activitiesForYear(records []activityRecord, year int) []activity {
	seen := map[string]bool{}
	var out []activity
	for _, rec := range records {
		if rec.TimestampUnix == 0 || time.Unix(rec.TimestampUnix, 0).In(time.Local).Year() != year {
			continue
		}
		act := activityFromRecord(rec)
		key := activityDedupeKey(act)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, act)
	}
	return out
}

func remoteImageMap(msgs []msg) map[string]string {
	out := map[string]string{}
	for i := range msgs {
		for _, u := range picURLs(&msgs[i]) {
			if u != "" {
				out[u] = u
			}
		}
	}
	return out
}

type previewBuildResult struct {
	ProbePath       string
	OutputPath      string
	ActivityRecords int
	YearActivities  int
	Reconstructed   int
}

func buildPreviewYearFromProbeFile(path string, year int, dir string) (previewBuildResult, error) {
	records, err := readActivityRecords(path)
	if err != nil {
		return previewBuildResult{}, err
	}
	if len(records) == 0 {
		return previewBuildResult{}, fmt.Errorf("探查日志没有 activity 明细；请用 --capture-activities 重新探查")
	}
	acts := activitiesForYear(records, year)
	recons := reconstructMoments(acts)
	deleted := reconAsDeletedMsgs(recons, nil)
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].CreatedTime > deleted[j].CreatedTime })

	doc := buildHTML(fmt.Sprintf("%d 年探查预览", year), nil, deleted, remoteImageMap(deleted))
	out := filepath.Join(dir, fmt.Sprintf("reconstructed_%d_preview.html", year))
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		return previewBuildResult{}, err
	}
	return previewBuildResult{
		ProbePath:       path,
		OutputPath:      out,
		ActivityRecords: len(records),
		YearActivities:  len(acts),
		Reconstructed:   len(deleted),
	}, nil
}

func runPreviewYear(args []string, dir string) error {
	year, err := previewYearFromArgs(args)
	if err != nil {
		return err
	}
	path, err := previewProbePathFromArgs(args, dir)
	if err != nil {
		return err
	}
	result, err := buildPreviewYearFromProbeFile(path, year, dir)
	if err != nil {
		return err
	}
	say("探查日志: %s", result.ProbePath)
	say("activity 明细=%d，%d 年活动=%d，重建预览=%d 条", result.ActivityRecords, year, result.YearActivities, result.Reconstructed)
	say("已生成预览 HTML: %s", result.OutputPath)
	say("提示：预览页不下载图片，只引用探查日志里的图片 URL。")
	return nil
}

func intArg(args []string, name string, fallback int) int {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			value, err := strconv.Atoi(args[i+1])
			if err == nil {
				return value
			}
		}
	}
	return fallback
}

func strArg(args []string, name, fallback string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return fallback
}

func offsetRange(args []string) (int, int, int) {
	start := intArg(args, "--offset-start", 3300)
	end := intArg(args, "--offset-end", 6300)
	step := intArg(args, "--offset-step", 100)
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if step <= 0 {
		step = 100
	}
	return start, end, step
}

func probeDelay(args []string, fallback time.Duration) time.Duration {
	ms := intArg(args, "--probe-delay-ms", int(fallback/time.Millisecond))
	if ms < 200 {
		ms = 200
	}
	if ms > 30000 {
		ms = 30000
	}
	return time.Duration(ms) * time.Millisecond
}

func probeCooldownSchedule() []time.Duration {
	return []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
}

type probeCooldown struct {
	enabled  bool
	schedule []time.Duration
	next     int
	sleep    func(time.Duration)
}

func newProbeCooldown(args []string) *probeCooldown {
	return newProbeCooldownWithSleep(args, time.Sleep)
}

func newProbeCooldownWithSleep(args []string, sleep func(time.Duration)) *probeCooldown {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &probeCooldown{
		enabled:  !hasArg(args, "--no-auto-cooldown"),
		schedule: probeCooldownSchedule(),
		sleep:    sleep,
	}
}

func isProbeCooldownError(rec probeRecord) bool {
	if rec.Status != "error" {
		return false
	}
	return strings.Contains(rec.Error, "HTTP 501") || strings.Contains(rec.Error, "HTTP 429")
}

func (p *probeCooldown) reset() {
	if p != nil {
		p.next = 0
	}
}

func (p *probeCooldown) waitIfNeeded(strategy string, offset int, rec probeRecord) bool {
	if p == nil || !p.enabled || !isProbeCooldownError(rec) {
		return false
	}
	if p.next >= len(p.schedule) {
		say("%s offset=%d 持续返回 %s，自动冷却次数已用尽", strategy, offset, rec.Error)
		return false
	}
	delay := p.schedule[p.next]
	p.next++
	say("%s offset=%d 返回 %s，自动冷却 %s 后重试（%d/%d）",
		strategy, offset, rec.Error, delay, p.next, len(p.schedule))
	p.sleep(delay)
	return true
}

func (c *config) probeFeedResultWithCooldown(window probeWindow, offset, count int, strategy string, attempts int, baseDelay time.Duration, cooldown *probeCooldown) probeResult {
	for {
		result := c.probeFeedResultWithRetry(window, offset, count, strategy, attempts, baseDelay)
		if cooldown != nil && cooldown.waitIfNeeded(strategy, offset, result.Record) {
			continue
		}
		if !isProbeCooldownError(result.Record) {
			cooldown.reset()
		}
		return result
	}
}

// probeRawWithCooldown 与 probeFeedResultWithCooldown 行为一致，但单次请求（attempts=1），
// 用于 harvest：靠 cooldown 处理 501/429 长冷却，避免每页内部多次快速重试加剧风控。
func (c *config) probeRawWithCooldown(window probeWindow, offset, count int, strategy string, cooldown *probeCooldown) probeResult {
	return c.probeFeedResultWithCooldown(window, offset, count, strategy, 1, time.Second, cooldown)
}

func intListArg(args []string, name string, fallback []int) []int {
	var raw string
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			raw = args[i+1]
			break
		}
	}
	if raw == "" {
		return append([]int(nil), fallback...)
	}
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 0 || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return append([]int(nil), fallback...)
	}
	return out
}

func defaultOffsetList() []int {
	return []int{
		0, 100, 200, 400, 800, 1600,
		3000, 3100, 3150, 3190, 3200, 3210, 3250, 3300, 3400,
		3600, 4000, 4400, 4800, 5200, 5600, 6000, 6400, 6800, 7200,
		8000, 9600, 12800,
	}
}

func runProbeOffsetRange(cfg *config, dir string, args []string) error {
	start, end, step := offsetRange(args)
	delay := probeDelay(args, 900*time.Millisecond)
	cooldown := newProbeCooldown(args)
	captureActivities := hasArg(args, "--capture-activities")
	maxConsecutiveErrors := intArg(args, "--max-consecutive-errors", 5)
	if maxConsecutiveErrors < 1 {
		maxConsecutiveErrors = 5
	}
	out := filepath.Join(dir, "probe_offset_range_"+time.Now().Format("20060102_150405")+".jsonl")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("创建 offset 探查日志失败: %w", err)
	}
	defer f.Close()

	say("开始 offset 密集探查，范围 %d..%d step=%d delay=%s，结果写入 %s", start, end, step, delay, out)
	base := probeWindow{Label: "all-time", Begin: 0, End: 0}
	healthResult := cfg.probeFeedResultWithCooldown(base, 0, probePageSize, "health-check", 1, time.Second, cooldown)
	health := healthResult.Record
	if err := writeProbeRecord(f, health); err != nil {
		return fmt.Errorf("写入健康检查记录失败: %w", err)
	}
	if captureActivities {
		if err := writeActivityRecords(f, base, 0, "health-check", healthResult.Activities); err != nil {
			return fmt.Errorf("写入健康检查活动记录失败: %w", err)
		}
	}
	if health.Status == "error" {
		say("健康检查失败 offset=0 error=%s；停止探查，建议稍后冷却后重试", health.Error)
		return nil
	}
	say("健康检查通过 offset=0 parsed=%d total=%d", health.ParsedCount, health.TotalNumber)
	consecutiveErrors := 0
	for offset := start; offset <= end; offset += step {
		result := cfg.probeFeedResultWithCooldown(base, offset, probePageSize, "offset-range", 1, time.Second, cooldown)
		rec := result.Record
		if err := writeProbeRecord(f, rec); err != nil {
			return fmt.Errorf("写入 offset 探查记录失败: %w", err)
		}
		if captureActivities {
			if err := writeActivityRecords(f, base, offset, "offset-range", result.Activities); err != nil {
				return fmt.Errorf("写入 offset 活动记录失败: %w", err)
			}
		}
		say("offset=%d parsed=%d total=%d min=%s max=%s status=%s",
			offset, rec.ParsedCount, rec.TotalNumber, rec.MinTimeText, rec.MaxTimeText, rec.Status)
		if rec.Status == "error" {
			lg("offset 密集探查 offset=%d 失败: %s", offset, rec.Error)
			consecutiveErrors++
		} else {
			consecutiveErrors = 0
		}
		if consecutiveErrors >= maxConsecutiveErrors {
			say("连续 %d 个 offset 失败，提前停止以避免无效请求", consecutiveErrors)
			break
		}
		time.Sleep(delay)
	}
	say("offset 密集探查完成：%s", out)
	return nil
}

func runProbeOffsetList(cfg *config, dir string, args []string) error {
	offsets := intListArg(args, "--offsets", defaultOffsetList())
	delay := probeDelay(args, 900*time.Millisecond)
	cooldown := newProbeCooldown(args)
	captureActivities := hasArg(args, "--capture-activities")
	out := filepath.Join(dir, "probe_offset_list_"+time.Now().Format("20060102_150405")+".jsonl")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("创建 offset 列表探查日志失败: %w", err)
	}
	defer f.Close()

	say("开始 offset 候选点探查，共 %d 个点 delay=%s，结果写入 %s", len(offsets), delay, out)
	base := probeWindow{Label: "all-time", Begin: 0, End: 0}
	healthResult := cfg.probeFeedResultWithCooldown(base, 0, probePageSize, "health-check", 1, time.Second, cooldown)
	health := healthResult.Record
	if err := writeProbeRecord(f, health); err != nil {
		return fmt.Errorf("写入健康检查记录失败: %w", err)
	}
	if captureActivities {
		if err := writeActivityRecords(f, base, 0, "health-check", healthResult.Activities); err != nil {
			return fmt.Errorf("写入健康检查活动记录失败: %w", err)
		}
	}
	if health.Status == "error" {
		say("健康检查失败 offset=0 error=%s；停止探查，建议稍后冷却后重试", health.Error)
		return nil
	}
	say("健康检查通过 offset=0 parsed=%d total=%d", health.ParsedCount, health.TotalNumber)
	for _, offset := range offsets {
		if offset == 0 {
			continue
		}
		result := cfg.probeFeedResultWithCooldown(base, offset, probePageSize, "offset-list", 1, time.Second, cooldown)
		rec := result.Record
		if err := writeProbeRecord(f, rec); err != nil {
			return fmt.Errorf("写入 offset 列表探查记录失败: %w", err)
		}
		if captureActivities {
			if err := writeActivityRecords(f, base, offset, "offset-list", result.Activities); err != nil {
				return fmt.Errorf("写入 offset 列表活动记录失败: %w", err)
			}
		}
		say("offset=%d parsed=%d total=%d min=%s max=%s status=%s",
			offset, rec.ParsedCount, rec.TotalNumber, rec.MinTimeText, rec.MaxTimeText, rec.Status)
		if rec.Status == "error" {
			lg("offset 候选点探查 offset=%d 失败: %s", offset, rec.Error)
		}
		time.Sleep(delay)
	}
	say("offset 候选点探查完成：%s", out)
	return nil
}

func runProbeDepth(cfg *config, dir string, args []string) error {
	delay := probeDelay(args, 900*time.Millisecond)
	cooldown := newProbeCooldown(args)
	captureActivities := hasArg(args, "--capture-activities")
	out := filepath.Join(dir, "probe_depth_"+time.Now().Format("20060102_150405")+".jsonl")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("创建探查日志失败: %w", err)
	}
	defer f.Close()

	say("开始深度探查实验，结果写入 %s", out)
	for _, window := range defaultProbeWindows() {
		result := cfg.probeFeedResultWithCooldown(window, 0, probePageSize, "month-window", 3, time.Second, cooldown)
		rec := result.Record
		if err := writeProbeRecord(f, rec); err != nil {
			return fmt.Errorf("写入探查记录失败: %w", err)
		}
		if captureActivities {
			if err := writeActivityRecords(f, window, 0, "month-window", result.Activities); err != nil {
				return fmt.Errorf("写入窗口活动记录失败: %w", err)
			}
		}
		say("窗口 %s offset=0 parsed=%d total=%d status=%s", window.Label, rec.ParsedCount, rec.TotalNumber, rec.Status)
		time.Sleep(delay)
	}

	base := probeWindow{Label: "all-time", Begin: 0, End: 0}
	for _, offset := range exponentialOffsets(20000) {
		result := cfg.probeFeedResultWithCooldown(base, offset, probePageSize, "exponential-offset", 3, time.Second, cooldown)
		rec := result.Record
		if err := writeProbeRecord(f, rec); err != nil {
			return fmt.Errorf("写入探查记录失败: %w", err)
		}
		if captureActivities {
			if err := writeActivityRecords(f, base, offset, "exponential-offset", result.Activities); err != nil {
				return fmt.Errorf("写入全量 offset 活动记录失败: %w", err)
			}
		}
		say("全量 offset=%d parsed=%d total=%d status=%s", offset, rec.ParsedCount, rec.TotalNumber, rec.Status)
		if rec.Status == "error" {
			lg("深度探查 offset=%d 失败: %s", offset, rec.Error)
		}
		time.Sleep(delay)
	}
	say("深度探查实验完成：%s", out)
	return nil
}

// ---------- 动态流原始响应本地缓存（cache-first harvester） ----------
//
// 同一个 offset 的原始响应只由 offset 唯一决定（begin_time/end_time 已证明被服务端忽略，
// count 固定 100），且历史动态流内容不再变化，因此可以安全地长期缓存到本地。
// 落盘后所有分析/重建走本地，零网络；harvest 本身可断点续传、被风控就自动冷却。

const feedCacheDirName = "feed_cache"
const harvestPageSize = 100

func feedCacheDir(dir string) string {
	return filepath.Join(dir, feedCacheDirName)
}

// cachePath offset 零填充到 7 位，保证字典序 = offset 升序。
func cachePath(dir string, offset int) string {
	return filepath.Join(feedCacheDir(dir), fmt.Sprintf("off_%07d.body", offset))
}

func isCached(dir string, offset int) bool {
	info, err := os.Stat(cachePath(dir, offset))
	return err == nil && !info.IsDir()
}

func writeCache(dir string, offset int, body []byte) error {
	if err := os.MkdirAll(feedCacheDir(dir), 0700); err != nil {
		return err
	}
	return os.WriteFile(cachePath(dir, offset), body, 0600)
}

func readCache(dir string, offset int) ([]byte, error) {
	return os.ReadFile(cachePath(dir, offset))
}

var cacheFileRe = regexp.MustCompile(`^off_(\d{7})\.body$`)

// cachedOffsets 返回已缓存的全部 offset，升序。
func cachedOffsets(dir string) ([]int, error) {
	entries, err := os.ReadDir(feedCacheDir(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var offs []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := cacheFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, err := strconv.Atoi(m[1]); err == nil {
			offs = append(offs, v)
		}
	}
	sort.Ints(offs)
	return offs, nil
}

// harvestPage 优先读缓存；缓存未命中才发一次网络请求（自带 501/429 冷却），成功后落盘。
// 返回 (record, fromCache, fetched)：fromCache=命中缓存未发请求；fetched=成功取到并已落盘。
func (c *config) harvestPage(dir string, offset int, cooldown *probeCooldown) (probeRecord, bool, bool) {
	base := probeWindow{Label: "all-time", Begin: 0, End: 0}
	if body, err := readCache(dir, offset); err == nil {
		rec := analyzeFeedBody(c, base, offset, "harvest-cache", body)
		return rec, true, false
	}
	result := c.probeRawWithCooldown(base, offset, harvestPageSize, "harvest", cooldown)
	if result.Record.Status != "ok" {
		return result.Record, false, false
	}
	if err := writeCache(dir, offset, result.RawBody); err != nil {
		lg("harvest 写缓存失败 offset=%d err=%v", offset, err)
		return result.Record, false, false
	}
	return result.Record, false, true
}

func harvestOffsets(args []string) []int {
	start := intArg(args, "--offset-start", 0)
	end := intArg(args, "--offset-end", 100000) // 提高默认上限，配合 auto-stop
	step := intArg(args, "--offset-step", harvestPageSize)
	if start < 0 {
		start = 0
	}
	if step < 1 {
		step = harvestPageSize
	}
	if end < start {
		end = start
	}
	var offs []int
	for o := start; o <= end; o += step {
		offs = append(offs, o)
	}
	return offs
}

// runHarvest 缓存优先地抓取一段 offset 网格：已缓存的跳过、未缓存的限速抓取并落盘。
// 可随时中断、重跑续传；被 501/429 风控时自动冷却。
func runHarvest(cfg *config, dir string, args []string) error {
	offsets := harvestOffsets(args)
	delay := probeDelay(args, 3000*time.Millisecond)
	cooldown := newProbeCooldown(args)
	autoStopEmpty := intArg(args, "--auto-stop-empty", 0) // 0 = 不自动停止
	manifestPath := filepath.Join(feedCacheDir(dir), "manifest.jsonl")
	if err := os.MkdirAll(feedCacheDir(dir), 0700); err != nil {
		return fmt.Errorf("创建缓存目录失败: %w", err)
	}
	mf, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("打开 manifest 失败: %w", err)
	}
	defer mf.Close()

	say("开始 harvest：offset %d..%d step %d，共 %d 个点 delay=%s",
		offsets[0], offsets[len(offsets)-1], offsets[1]-offsets[0], len(offsets), delay)
	if autoStopEmpty > 0 {
		say("自动停止：连续 %d 个空 offset 将终止探索", autoStopEmpty)
	}
	say("缓存目录：%s（已缓存的 offset 会直接跳过，可随时中断续传）", feedCacheDir(dir))

	var cached, fetched, empty, failed, consecutiveEmpty int
	for _, offset := range offsets {
		if isCached(dir, offset) {
			cached++
			continue
		}
		rec, fromCache, ok := cfg.harvestPage(dir, offset, cooldown)
		if fromCache {
			cached++
			continue
		}
		if !ok {
			failed++
			say("offset=%d 抓取失败 status=%s error=%s", offset, rec.Status, rec.Error)
			if rec.Status == "error" && !isProbeCooldownError(rec) {
				// 非风控错误（如健康检查级别失败）也继续，但记日志
				lg("harvest offset=%d 非冷却类错误: %s", offset, rec.Error)
			}
			time.Sleep(delay)
			continue
		}
		fetched++
		if rec.ParsedCount == 0 {
			empty++
			consecutiveEmpty++
			if autoStopEmpty > 0 && consecutiveEmpty >= autoStopEmpty {
				say("连续 %d 个 offset 为空，自动停止探索（当前 offset=%d）", consecutiveEmpty, offset)
				break
			}
		} else {
			consecutiveEmpty = 0 // 有数据则重置计数
		}
		if err := writeProbeRecord(mf, rec); err != nil {
			lg("harvest 写 manifest 失败 offset=%d err=%v", offset, err)
		}
		say("offset=%d ✅ 已缓存 parsed=%d total=%d min=%s", offset, rec.ParsedCount, rec.TotalNumber, rec.MinTimeText)
		time.Sleep(delay)
	}
	say("harvest 完成：命中缓存 %d、新抓 %d（其中空页 %d）、失败 %d", cached, fetched, empty, failed)
	say("下一步：./QzoneExport --analyze-cache  （纯离线，出边界报告）")
	return nil
}

// boundaryReport 缓存边界分析结果。
type boundaryReport struct {
	CachedCount       int    `json:"cached_count"`
	EmptyCount        int    `json:"empty_count"`
	NonEmptyCount     int    `json:"nonempty_count"`
	DeepestCached     int    `json:"deepest_cached_offset"`
	DeepestWithData   int    `json:"deepest_offset_with_data"`
	TailEmptyRun      int    `json:"tail_consecutive_empty"`
	GapOffsets        []int  `json:"gap_offsets"`
	GlobalMinTimeText string `json:"global_min_time_text"`
	GlobalMinUnix     int64  `json:"global_min_unix"`
	GlobalMaxTimeText string `json:"global_max_time_text"`
	DedupActivities   int    `json:"dedup_activities"`
}

// analyzeCache 纯离线遍历缓存，解析+去重所有活动，计算边界报告。
func analyzeCache(cfg *config, dir string) (boundaryReport, []activity, error) {
	offs, err := cachedOffsets(dir)
	if err != nil {
		return boundaryReport{}, nil, err
	}
	if len(offs) == 0 {
		return boundaryReport{}, nil, fmt.Errorf("缓存为空；请先运行 --harvest")
	}
	base := probeWindow{Label: "all-time", Begin: 0, End: 0}
	rep := boundaryReport{CachedCount: len(offs), GlobalMinUnix: 0}
	seen := map[string]bool{}
	var all []activity
	var minUnix int64
	var maxUnix int64
	// 缺口判定按 harvest 的固定 100 网格；从稀疏缓存反推步长会漏报缺口
	step := harvestPageSize
	for _, offset := range offs {
		body, err := readCache(dir, offset)
		if err != nil {
			continue
		}
		rec := analyzeFeedBody(cfg, base, offset, "analyze-cache", body)
		if offset > rep.DeepestCached {
			rep.DeepestCached = offset
		}
		if rec.ParsedCount == 0 {
			rep.EmptyCount++
		} else {
			rep.NonEmptyCount++
			if offset > rep.DeepestWithData {
				rep.DeepestWithData = offset
			}
		}
		for _, a := range cfg.parseActivities(processOldHTML(string(body))) {
			key := activityDedupeKey(a)
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, a)
			u := tsToUnix(a.Timestamp)
			if u > 0 {
				if minUnix == 0 || u < minUnix {
					minUnix = u
					rep.GlobalMinTimeText = a.TimeText
				}
				if u > maxUnix {
					maxUnix = u
					rep.GlobalMaxTimeText = a.TimeText
				}
			}
		}
	}
	rep.GlobalMinUnix = minUnix
	rep.DedupActivities = len(all)
	// 缺口：理论网格 0..DeepestCached step，缺失的 offset
	cachedSet := map[int]bool{}
	for _, o := range offs {
		cachedSet[o] = true
	}
	for o := offs[0]; o <= rep.DeepestCached; o += step {
		if !cachedSet[o] {
			rep.GapOffsets = append(rep.GapOffsets, o)
		}
	}
	// 尾部连续空页：从最深 offset 往回数连续 ParsedCount==0 的页
	for i := len(offs) - 1; i >= 0; i-- {
		body, err := readCache(dir, offs[i])
		if err != nil {
			break
		}
		if analyzeFeedBody(cfg, base, offs[i], "tail", body).ParsedCount == 0 {
			rep.TailEmptyRun++
		} else {
			break
		}
	}
	return rep, all, nil
}

func runAnalyzeCache(cfg *config, dir string, args []string) error {
	rep, all, err := analyzeCache(cfg, dir)
	if err != nil {
		return err
	}
	say("===== 缓存边界报告 =====")
	say("已缓存页=%d（空页=%d，有数据=%d） 缺口 offset 数=%d", rep.CachedCount, rep.EmptyCount, rep.NonEmptyCount, len(rep.GapOffsets))
	say("最深已缓存 offset=%d，最深有数据 offset=%d", rep.DeepestCached, rep.DeepestWithData)
	say("尾部连续空页=%d", rep.TailEmptyRun)
	say("去重后活动总数=%d", rep.DedupActivities)
	say("全局最早活动=%s（unix=%d）", rep.GlobalMinTimeText, rep.GlobalMinUnix)
	say("全局最晚活动=%s", rep.GlobalMaxTimeText)
	complete := len(rep.GapOffsets) == 0 && rep.TailEmptyRun >= 20
	if complete {
		say("✅ 网格无缺口且尾部连续空页≥20：可判定 %s 为动态流真实边界", rep.GlobalMinTimeText)
	} else {
		if len(rep.GapOffsets) > 0 {
			preview := rep.GapOffsets
			if len(preview) > 10 {
				preview = preview[:10]
			}
			say("⚠️ 仍有 %d 个缺口未补全（如 %v…），重跑 --harvest 自动续传", len(rep.GapOffsets), preview)
		}
		if rep.TailEmptyRun < 20 {
			say("⚠️ 尾部连续空页仅 %d（<20），用 --harvest --offset-end %d 继续往深处扫以确认边界", rep.TailEmptyRun, rep.DeepestCached+2000)
		}
	}
	// 导出去重活动为 JSONL，复用 --preview-year / --analyze-probe
	actPath := filepath.Join(dir, "cache_activities_"+time.Now().Format("20060102_150405")+".jsonl")
	if af, err := os.OpenFile(actPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
		base := probeWindow{Label: "all-time", Begin: 0, End: 0}
		_ = writeActivityRecords(af, base, 0, "analyze-cache", all)
		af.Close()
		say("已导出去重活动明细：%s", actPath)
	}
	// 全量重建 HTML（不限年份）
	recons := reconstructMoments(all)
	deleted := reconAsDeletedMsgs(recons, nil)
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].CreatedTime > deleted[j].CreatedTime })
	// 默认只引用远程图片 URL（零网络）；加 --download-images 则真正下载并 base64 内嵌，防图床过期
	var datauri map[string]string
	if hasArg(args, "--download-images") {
		say("开始下载重建说说的图片并内嵌（防止图床链接过期）…")
		datauri = cfg.downloadCards(deleted)
	} else {
		datauri = remoteImageMap(deleted)
	}
	doc := buildHTML("缓存全量重建预览", nil, deleted, datauri)
	outHTML := filepath.Join(dir, "reconstructed_all_from_cache.html")
	if err := os.WriteFile(outHTML, []byte(doc), 0600); err != nil {
		return err
	}
	say("已生成全量重建预览：%s（%d 条疑似已删除）", outHTML, len(deleted))
	if !hasArg(args, "--download-images") {
		say("提示：加 --download-images 可把图片下载并内嵌进 HTML（防止 QQ 图床链接日后失效）")
	}
	return nil
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
	out := filepath.Join(outputDir(), "QQ空间_demo.html")
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		fmt.Println("写入失败:", err)
		return
	}
	fmt.Println("已生成演示网页:", out)
	openInSystem(out)
}

func loadOrLogin(dir string) *config {
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
	return cfg
}

func loadOrLoginForProbe(dir string, args []string) *config {
	cfg := loadConfig(dir)
	if cfg != nil {
		lg("检测到本地 cookies.json，校验登录态…")
		fmt.Println("检测到本地 cookies.json，正在校验登录态…")
		if validateConfig(cfg) {
			say("✅ 检测到有效登录（QQ %s），跳过扫码", cfg.QQ)
			return cfg
		}
		fmt.Println("ℹ️ 旧登录已过期")
	}
	if hasArg(args, "--no-login") {
		say("❌ 登录态无效，且指定了 --no-login；未执行扫码登录")
		waitExit()
		os.Exit(1)
	}
	return login(dir)
}

func main() {
	if hasArg(os.Args[1:], "--demo") {
		runDemo()
		return
	}
	if hasArg(os.Args[1:], "--analyze-probe") {
		if err := runAnalyzeProbe(os.Args[1:], outputDir()); err != nil {
			fmt.Println("❌ 探查日志分析失败:", err)
			os.Exit(1)
		}
		return
	}
	if hasArg(os.Args[1:], "--preview-year") {
		if err := runPreviewYear(os.Args[1:], outputDir()); err != nil {
			fmt.Println("❌ 生成年份预览失败:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println("================================================")
	fmt.Printf("        QQ空间说说导出工具  v%s\n", version)
	fmt.Println("================================================")
	dir := outputDir()
	logPath := initLog(dir)
	if logPath != "" {
		fmt.Printf("📝 详细日志: %s\n", logPath)
	}
	defer closeLog()

	if hasArg(os.Args[1:], "--analyze-cache") {
		var cfg *config
		if hasArg(os.Args[1:], "--download-images") {
			// 下载图片需要有效登录态（g_tk）
			cfg = loadOrLoginForProbe(dir, os.Args[1:])
		} else {
			cfg = loadConfig(dir)
			if cfg == nil {
				cfg = &config{QQ: ""}
			}
		}
		if err := runAnalyzeCache(cfg, dir, os.Args[1:]); err != nil {
			say("❌ 缓存分析失败: %v", err)
			os.Exit(1)
		}
		return
	}
	if hasArg(os.Args[1:], "--harvest") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runHarvest(cfg, dir, os.Args[1:]); err != nil {
			say("❌ harvest 失败: %v", err)
			waitExit()
			os.Exit(1)
		}
		fmt.Println("\n⚠️ 提示：缓存目录含你的空间内容摘要与图片 URL，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--diagnose") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runDiagnose(cfg, dir); err != nil {
			say("❌ 诊断失败: %v", err)
		}
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--harvest-photos") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runHarvestPhotos(cfg, dir); err != nil {
			say("❌ 相册找回失败: %v", err)
		}
		fmt.Println("\n⚠️ 提示：相册回忆网页含你的照片，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--harvest-board") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runHarvestBoard(cfg, dir); err != nil {
			say("❌ 留言板找回失败: %v", err)
		}
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--harvest-blogs") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runHarvestBlogs(cfg, dir); err != nil {
			say("❌ 日志找回失败: %v", err)
		}
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--resolve-tid") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		tid := strArg(os.Args[1:], "--resolve-tid", "")
		if err := runResolveTid(cfg, dir, tid); err != nil {
			say("❌ 说说详情还原失败: %v", err)
		}
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--recover-all") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runRecoverAll(cfg, dir); err != nil {
			say("❌ 回忆找回失败: %v", err)
		}
		fmt.Println("\n⚠️ 提示：找回的网页含你的照片与内容，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--probe-offset-list") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runProbeOffsetList(cfg, dir, os.Args[1:]); err != nil {
			say("❌ offset 候选点探查失败: %v", err)
			waitExit()
			os.Exit(1)
		}
		fmt.Println("\n⚠️ 提示：探查日志可能含你的空间内容摘要，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--probe-offset-range") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runProbeOffsetRange(cfg, dir, os.Args[1:]); err != nil {
			say("❌ offset 密集探查失败: %v", err)
			waitExit()
			os.Exit(1)
		}
		fmt.Println("\n⚠️ 提示：探查日志可能含你的空间内容摘要，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}
	if hasArg(os.Args[1:], "--probe-depth") {
		cfg := loadOrLoginForProbe(dir, os.Args[1:])
		if err := runProbeDepth(cfg, dir, os.Args[1:]); err != nil {
			say("❌ 深度探查失败: %v", err)
			waitExit()
			os.Exit(1)
		}
		fmt.Println("\n⚠️ 提示：探查日志可能含你的空间内容摘要，请自行保管。")
		if runtime.GOOS == "windows" {
			waitExit()
		}
		return
	}

	cfg := loadOrLogin(dir)
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
