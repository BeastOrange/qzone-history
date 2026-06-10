package main

// recover.go —— 跨数据源的回忆找回。
//
// 说说动态流只是其中一口井；照片(相册)、留言板、日志是**独立存储**的内容仓库，
// 说说被删它们依然在，且大多带 2010–2015 的真实时间戳。本文件新增隐藏入口：
//
//   --diagnose          打印 main_page_cgi 的 说说/相册/日志 计数（判断 2017 是删除边界还是漏抓）
//   --harvest-photos    抓所有相册的照片元数据（uploadtime/desc/原图URL），生成相册回忆 HTML
//   --harvest-board     抓留言板（别人留给你的话 + 时间）
//   --harvest-blogs     抓日志列表 + 正文
//   --resolve-tid <tid> 用 emotion_cgi_msgdetail_v6 还原单条说说全文
//   --recover-all       依次跑上面的 diagnose + photos + board + blogs
//
// 所有抓取都缓存优先（原始响应落盘到 recover_cache/），可断点续传、离线复用，
// 复用既有的 genGTK / JSONP 剥离 / 带 Referer 下原图 / buildHTML。正式导出完全不受影响。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const recoverCacheDirName = "recover_cache"

func recoverCacheDir(dir string) string { return filepath.Join(dir, recoverCacheDirName) }

// cachedGet 缓存优先地取一个 JSONP/JSON 接口的原始响应：
// 命中 recover_cache/<name>.body 直接返回；否则发请求（自带重试），成功落盘。
func (c *config) cachedGet(dir, name, url string) ([]byte, error) {
	p := filepath.Join(recoverCacheDir(dir), name+".body")
	if body, err := os.ReadFile(p); err == nil {
		return body, nil
	}
	body, err := c.httpGetWithRetry(url, 3, time.Second)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(recoverCacheDir(dir), 0700); err == nil {
		_ = os.WriteFile(p, body, 0600)
	}
	return body, nil
}

// unwrapJSONP 剥掉 callback(...) 外壳，返回内层 JSON。
func unwrapJSONP(body []byte) []byte {
	if m := jsonpRe.FindSubmatch(body); m != nil {
		return m[1]
	}
	return body
}

// ---------- #0 诊断：main_page_cgi 计数（说说/相册/日志总数） ----------

func (c *config) mainPageCounts(dir string) (ss, xc, rz int, err error) {
	u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/r.qzone.qq.com/cgi-bin/main_page_cgi?uin=%s&param=16&g_tk=%s",
		c.QQ, string(c.GTK))
	body, err := c.cachedGet(dir, "main_page", u)
	if err != nil {
		return 0, 0, 0, err
	}
	var r struct {
		Data struct {
			Module16 struct {
				Data struct {
					SS json.Number `json:"SS"`
					XC json.Number `json:"XC"`
					RZ json.Number `json:"RZ"`
				} `json:"data"`
			} `json:"module_16"`
		} `json:"data"`
	}
	if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
		return 0, 0, 0, fmt.Errorf("解析 main_page_cgi 失败: %w 原始(截断): %s", err, truncate(string(body), 400))
	}
	d := r.Data.Module16.Data
	ss64, _ := d.SS.Int64()
	xc64, _ := d.XC.Int64()
	rz64, _ := d.RZ.Int64()
	return int(ss64), int(xc64), int(rz64), nil
}

func runDiagnose(cfg *config, dir string) error {
	ss, xc, rz, err := cfg.mainPageCounts(dir)
	if err != nil {
		return err
	}
	say("===== 账号内容计数（main_page_cgi）=====")
	say("说说 SS=%d，相册 XC=%d，日志 RZ=%d", ss, xc, rz)
	say("说明：把 SS 与正式导出抓到的「现存说说数」对比——若相等，2017-09-03 就是你最老的现存说说")
	say("      （更早的是被删除的，只能靠重建/相册/邮件找回）；若 SS 更大，说说接口还有漏抓空间。")
	say("下一步：./QzoneExport --harvest-photos   （相册照片大概率完好保留 2015 回忆）")
	return nil
}

// ---------- #1 相册照片（最高价值，独立于说说存储） ----------

type album struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	CreateTime json.Number `json:"createtime"`
	Total      json.Number `json:"total"`
}

type photo struct {
	Name       string `json:"name"`
	Desc       string `json:"desc"`
	URL        string `json:"url"`
	Raw        string `json:"raw"`
	UploadTime string `json:"uploadtime"` // "2014-06-13 20:21:33"
}

// parseUploadTime 把 "2014-06-13 20:21:33" 解析为本地时区 Unix 秒。
func parseUploadTime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		return t.Unix()
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t.Unix()
	}
	return 0
}

func (c *config) listAlbums(dir string) ([]album, error) {
	u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/photo.qzone.qq.com/fcgi-bin/fcg_list_album_v3?g_tk=%s&hostUin=%s&uin=%s&appid=4&inCharset=utf-8&outCharset=utf-8&source=qzone&plat=qzone&format=jsonp&pageStart=0&pageNum=1000",
		string(c.GTK), c.QQ, c.QQ)
	body, err := c.cachedGet(dir, "albums", u)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			Sort  []album `json:"albumListModeSort"`
			Class []album `json:"albumListModeClass"`
		} `json:"data"`
	}
	if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
		return nil, fmt.Errorf("解析相册列表失败: %w 原始(截断): %s", err, truncate(string(body), 400))
	}
	albums := r.Data.Sort
	if len(albums) == 0 {
		albums = r.Data.Class
	}
	return albums, nil
}

// listPhotos 翻页拉一个相册的全部照片（offset 翻页，按 total 收口）。
func (c *config) listPhotos(dir string, al album) ([]photo, error) {
	var all []photo
	const pageNum = 500
	for start := 0; ; start += pageNum {
		u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/photo.qzone.qq.com/fcgi-bin/cgi_list_photo?g_tk=%s&hostUin=%s&uin=%s&topicId=%s&appid=4&inCharset=utf-8&outCharset=utf-8&source=qzone&plat=qzone&format=jsonp&pageStart=%d&pageNum=%d",
			string(c.GTK), c.QQ, c.QQ, al.ID, start, pageNum)
		name := fmt.Sprintf("photos_%s_%d", al.ID, start)
		body, err := c.cachedGet(dir, name, u)
		if err != nil {
			return all, err
		}
		var r struct {
			Data struct {
				PhotoList    []photo `json:"photoList"`
				TotalInAlbum int     `json:"totalInAlbum"`
			} `json:"data"`
		}
		if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
			return all, fmt.Errorf("解析相册 %s 照片失败: %w", al.Name, err)
		}
		all = append(all, r.Data.PhotoList...)
		if len(r.Data.PhotoList) == 0 || len(all) >= r.Data.TotalInAlbum {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	return all, nil
}

// photoToMsg 把一张照片映射成一条"卡片"，复用 buildHTML 渲染（含原图、描述、时间）。
func photoToMsg(al album, p photo) msg {
	caption := p.Desc
	prefix := "📷 " + al.Name
	if caption != "" {
		caption = prefix + "｜" + caption
	} else {
		caption = prefix
	}
	src := p.Raw
	if src == "" {
		src = p.URL
	}
	return msg{
		Content:     caption,
		CreatedTime: parseUploadTime(p.UploadTime),
		ExtraImgs:   []string{toOriginal(src)},
	}
}

func runHarvestPhotos(cfg *config, dir string) error {
	albums, err := cfg.listAlbums(dir)
	if err != nil {
		return err
	}
	say("发现 %d 个相册，开始逐个拉取照片…", len(albums))
	var cards []msg
	for _, al := range albums {
		photos, err := cfg.listPhotos(dir, al)
		if err != nil {
			say("相册「%s」拉取出错（已保留已抓部分）: %v", al.Name, err)
		}
		ct, _ := al.CreateTime.Int64()
		say("  相册「%s」%d 张  建于 %s", al.Name, len(photos), fmtTime(ct))
		for _, p := range photos {
			cards = append(cards, photoToMsg(al, p))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(cards) == 0 {
		say("没有抓到照片。")
		return nil
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].CreatedTime > cards[j].CreatedTime })
	earliest := cards[len(cards)-1].CreatedTime
	say("共 %d 张照片，最早 %s", len(cards), fmtTime(earliest))

	datauri := cfg.downloadCards(cards)
	doc := buildHTML(cfg.QQ+" · 相册回忆", cards, nil, datauri)
	out := filepath.Join(dir, "相册回忆_"+cfg.QQ+".html")
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		return err
	}
	say("✅ 已生成相册回忆网页：%s", out)
	return nil
}

// downloadCards 并发下载卡片里的图片并 base64 内嵌（复刻正式导出的下载编排）。
func (c *config) downloadCards(cards []msg) map[string]string {
	var images []imageCandidate
	seen := map[string]bool{}
	for i := range cards {
		for _, item := range imageCandidates(&cards[i]) {
			if !seen[item.Key] {
				seen[item.Key] = true
				images = append(images, item)
			}
		}
	}
	concurrency := downloadConcurrency()
	fmt.Printf("下载 %d 张图片（原图画质，并发 %d）…\n", len(images), concurrency)
	datauri := map[string]string{}
	var mu, dmu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	var done int
	for _, item := range images {
		wg.Add(1)
		sem <- struct{}{}
		go func(item imageCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			du, _, err := c.downloadImageCandidate(item)
			dmu.Lock()
			done++
			fmt.Printf("  %d/%d\r", done, len(images))
			dmu.Unlock()
			if err == nil {
				mu.Lock()
				datauri[item.Key] = du
				mu.Unlock()
			} else {
				lg("相册图片下载失败 key=%s err=%v", maskURL(item.Key), err)
			}
		}(item)
	}
	wg.Wait()
	say("\n图片完成：成功 %d/%d", len(datauri), len(images))
	return datauri
}

// ---------- #2 留言板（别人留给你的话 + 时间） ----------

func runHarvestBoard(cfg *config, dir string) error {
	const num = 20
	var cards []msg
	var total int
	for start := 0; ; start += num {
		u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/m.qzone.qq.com/cgi-bin/new/get_msgb?format=jsonp&inCharset=utf-8&outCharset=utf-8&uin=%s&hostUin=%s&start=%d&num=%d&g_tk=%s",
			cfg.QQ, cfg.QQ, start, num, string(cfg.GTK))
		body, err := cfg.cachedGet(dir, fmt.Sprintf("board_%d", start), u)
		if err != nil {
			return err
		}
		var r struct {
			Data struct {
				Total       int `json:"total"`
				CommentList []struct {
					Uin     json.Number `json:"uin"`
					Nick    string      `json:"nickname"`
					Name    string      `json:"name"`
					Content string      `json:"content"`
					PubTime json.Number `json:"pubtime"`
				} `json:"commentList"`
			} `json:"data"`
		}
		if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
			return fmt.Errorf("解析留言板失败: %w 原始(截断): %s", err, truncate(string(body), 400))
		}
		total = r.Data.Total
		if len(r.Data.CommentList) == 0 {
			break
		}
		for _, m := range r.Data.CommentList {
			name := m.Nick
			if name == "" {
				name = m.Name
			}
			pt, _ := m.PubTime.Int64()
			cards = append(cards, msg{
				Content:     fmt.Sprintf("💬 %s 留言：%s", name, m.Content),
				CreatedTime: pt,
			})
		}
		if len(cards) >= total {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if len(cards) == 0 {
		say("留言板没有内容。")
		return nil
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].CreatedTime > cards[j].CreatedTime })
	say("共 %d 条留言，最早 %s", len(cards), fmtTime(cards[len(cards)-1].CreatedTime))
	doc := buildHTML(cfg.QQ+" · 留言板", cards, nil, map[string]string{})
	out := filepath.Join(dir, "留言板_"+cfg.QQ+".html")
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		return err
	}
	say("✅ 已生成留言板网页：%s", out)
	return nil
}

// ---------- #3 日志（长文，往往 2010–2015） ----------

func runHarvestBlogs(cfg *config, dir string) error {
	const num = 100
	type blogAbs struct {
		BlogID json.Number `json:"blogId"`
		Title  string      `json:"title"`
		PubURL string      `json:"pubTime"` // 部分版本是字符串
		PubNum json.Number `json:"pubtime"`
	}
	var cards []msg
	for pos := 0; ; pos += num {
		u := fmt.Sprintf("https://h5.qzone.qq.com/proxy/domain/b.qzone.qq.com/cgi-bin/blognew/get_abs?inCharset=utf-8&outCharset=utf-8&format=jsonp&hostUin=%s&uin=%s&g_tk=%s&pos=%d&num=%d&blogType=0&reqInfo=1",
			cfg.QQ, cfg.QQ, string(cfg.GTK), pos, num)
		body, err := cfg.cachedGet(dir, fmt.Sprintf("blogs_%d", pos), u)
		if err != nil {
			return err
		}
		var r struct {
			Data struct {
				List []blogAbs `json:"list"`
			} `json:"data"`
		}
		if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
			return fmt.Errorf("解析日志列表失败: %w 原始(截断): %s", err, truncate(string(body), 400))
		}
		if len(r.Data.List) == 0 {
			break
		}
		for _, b := range r.Data.List {
			pt, _ := b.PubNum.Int64()
			cards = append(cards, msg{
				Content:     "📝 " + b.Title,
				CreatedTime: pt,
			})
		}
		time.Sleep(400 * time.Millisecond)
	}
	if len(cards) == 0 {
		say("没有日志。")
		return nil
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].CreatedTime > cards[j].CreatedTime })
	say("共 %d 篇日志，最早 %s（正文未抓，仅标题+时间索引）", len(cards), fmtTime(cards[len(cards)-1].CreatedTime))
	doc := buildHTML(cfg.QQ+" · 日志", cards, nil, map[string]string{})
	out := filepath.Join(dir, "日志_"+cfg.QQ+".html")
	if err := os.WriteFile(out, []byte(doc), 0600); err != nil {
		return err
	}
	say("✅ 已生成日志索引网页：%s", out)
	return nil
}

// ---------- #4 单条说说详情（按 tid 还原全文） ----------

func runResolveTid(cfg *config, dir, tid string) error {
	if tid == "" {
		return fmt.Errorf("用法：--resolve-tid <tid>")
	}
	u := fmt.Sprintf("https://user.qzone.qq.com/proxy/domain/taotao.qq.com/cgi-bin/emotion_cgi_msgdetail_v6?uin=%s&tid=%s&g_tk=%s&format=jsonp",
		cfg.QQ, tid, string(cfg.GTK))
	body, err := cfg.cachedGet(dir, "tid_"+tid, u)
	if err != nil {
		return err
	}
	var r msglistResp
	if err := json.Unmarshal(unwrapJSONP(body), &r); err != nil {
		return fmt.Errorf("解析说说详情失败: %w 原始(截断): %s", err, truncate(string(body), 800))
	}
	if len(r.Msglist) == 0 {
		// 详情接口有时直接返回单条对象而非列表
		var single msg
		if err := json.Unmarshal(unwrapJSONP(body), &single); err == nil && single.Content != "" {
			r.Msglist = []msg{single}
		}
	}
	if len(r.Msglist) == 0 {
		say("tid=%s 未取到内容（可能已彻底删除或 tid 无效）", tid)
		return nil
	}
	m := r.Msglist[0]
	say("tid=%s 内容：%s", tid, m.Content)
	say("时间：%s  赞：%d  评论：%d", fmtTime(m.CreatedTime), m.LikeTotal, len(m.Commentlist))
	return nil
}

// ---------- 一键全跑 ----------

func runRecoverAll(cfg *config, dir string) error {
	if err := runDiagnose(cfg, dir); err != nil {
		say("诊断出错: %v", err)
	}
	say("---- 相册 ----")
	if err := runHarvestPhotos(cfg, dir); err != nil {
		say("相册出错: %v", err)
	}
	say("---- 留言板 ----")
	if err := runHarvestBoard(cfg, dir); err != nil {
		say("留言板出错: %v", err)
	}
	say("---- 日志 ----")
	if err := runHarvestBlogs(cfg, dir); err != nil {
		say("日志出错: %v", err)
	}
	say("===== 回忆找回完成 =====")
	return nil
}
