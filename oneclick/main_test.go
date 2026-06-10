package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

// 一段贴近真实动态流的合成 HTML：两条互动（一条评论、一条点赞），同一条说说。
// 用来验证零依赖正则解析器能正确切分条目并提取字段。
const sampleFeed = `<div class="feeds"><li class="f-single f-s-s">` +
	`<a class="f-name q_namecard" link="nameCard_111222" href="//user.qzone.qq.com/111222">小明</a>` +
	`<span class="state">评论了我的说说</span>` +
	`<div class="info-detail">2021年3月5日 12:30</div>` +
	`<p class="txt-box-title ellipsis-one">今天天气真好出去玩</p>` +
	`<a class="img-item"><img src="http://a.com/p1.jpg?w=200&h=200"></a>` +
	`</li>` +
	`<li class="f-single f-s-s">` +
	`<a class="f-name q_namecard" link="nameCard_333444" href="//user.qzone.qq.com/333444">小红</a>` +
	`<span class="state">赞了我的说说</span>` +
	`<div class="info-detail">2021年3月5日 12:31</div>` +
	`<p class="txt-box-title ellipsis-one">今天天气真好出去玩</p>` +
	`</li>`

func TestParseActivities(t *testing.T) {
	c := &config{QQ: "999"}
	acts := c.parseActivities(sampleFeed)
	if len(acts) != 2 {
		t.Fatalf("期望解析 2 条活动，实际 %d 条", len(acts))
	}
	if acts[0].Type != typeComment {
		t.Errorf("第1条应为评论(typeComment=%d)，实际 %d", typeComment, acts[0].Type)
	}
	if acts[0].SenderName != "小明" || acts[0].SenderQQ != "111222" {
		t.Errorf("第1条发送者解析错误: name=%q qq=%q", acts[0].SenderName, acts[0].SenderQQ)
	}
	if acts[0].Content != "今天天气真好出去玩" {
		t.Errorf("第1条正文解析错误: %q", acts[0].Content)
	}
	if len(acts[0].ImageURLs) != 1 {
		t.Errorf("第1条应提取到 1 张图片，实际 %d", len(acts[0].ImageURLs))
	}
	if acts[1].Type != typeLike {
		t.Errorf("第2条应为点赞(typeLike=%d)，实际 %d", typeLike, acts[1].Type)
	}
	if acts[0].ReceiverQQ != "999" {
		t.Errorf("收件人 QQ 应为 999，实际 %q", acts[0].ReceiverQQ)
	}
}

func TestReconstructAndDedup(t *testing.T) {
	c := &config{QQ: "999"}
	acts := c.parseActivities(sampleFeed)
	recons := reconstructMoments(acts)
	if len(recons) != 1 {
		t.Fatalf("两条互动同一说说应聚合为 1 条，实际 %d", len(recons))
	}
	rm := recons[0]
	if rm.Likes != 1 {
		t.Errorf("应有 1 个赞，实际 %d", rm.Likes)
	}
	if len(rm.Comments) != 1 {
		t.Errorf("应有 1 条评论记录，实际 %d", len(rm.Comments))
	}

	// 去重：当现存说说里已存在同内容时，重建结果应被排除
	existing := []msg{{Content: "今天天气真好出去玩"}}
	deleted := reconAsDeletedMsgs(recons, existing)
	if len(deleted) != 0 {
		t.Errorf("内容已存在于现存说说，应被去重，实际保留 %d 条", len(deleted))
	}

	// 现存列表为空时，重建结果应作为“疑似已删除”保留
	deleted2 := reconAsDeletedMsgs(recons, nil)
	if len(deleted2) != 1 {
		t.Fatalf("现存为空时应保留 1 条疑似已删除，实际 %d", len(deleted2))
	}
	if !deleted2[0].Deleted {
		t.Error("保留的说说应标记 Deleted=true")
	}
}

func TestProcessOldHTML(t *testing.T) {
	// 模拟 JSONP 包裹：_Callback({...html:'<li ...>',opuin:...})
	raw := `_Callback({code:0,html:'<li class="f-single f-s-s">\x68\x69 hello</li>',opuin:'999'})`
	got := processOldHTML(raw)
	if got == "" {
		t.Fatal("processOldHTML 不应返回空")
	}
	if !contains(got, "f-single") {
		t.Errorf("处理后应包含 f-single: %q", got)
	}
	if !contains(got, "hi hello") { // \x68\x69 -> hi
		t.Errorf("十六进制应被解码为 hi: %q", got)
	}
}

// 回归测试：JS 转义 \t \n 必须被正确还原为空白并折叠，绝不能残留成裸字母 t/n。
// 这是 v1.1 里“重建说说全是 ttttttt 乱码”的根因。
func TestProcessOldHTMLNoTabGarbage(t *testing.T) {
	// 模板缩进在 JSONP 里是 \t（反斜杠+t），正文是中文
	raw := `cb({html:'\t\t\t<li class="f-single f-s-s">\t\t<p class="txt-box-title ellipsis-one">\t\t\t终场哨响</p>\t\t</li>',opuin:'1'})`
	got := processOldHTML(raw)
	if contains(got, "ttt") {
		t.Errorf("不应残留裸字母 t（\\t 必须还原成制表符并折叠）: %q", got)
	}
	if contains(got, "nnn") {
		t.Errorf("不应残留裸字母 n: %q", got)
	}
	if !contains(got, "终场哨响") {
		t.Errorf("中文正文应保留: %q", got)
	}
	// 用解析器进一步确认正文干净
	c := &config{QQ: "1"}
	acts := c.parseActivities(got)
	if len(acts) != 1 || acts[0].Content != "终场哨响" {
		t.Fatalf("解析正文应为「终场哨响」，实际 %#v", acts)
	}
}

// 回归测试：一页动态流响应里 data:[] 数组含多个 feed，每个各带一个 html 段。
// 旧实现只取第一个 `html:'...',opuin` 段，导致一页 N 条只解析出 1 条（其余连同图片被丢弃，
// 这正是“重建只出 1 条、以前的图全没了”的根因）。新实现应提取并拼接全部 html 段。
func TestProcessOldHTMLMultiSegment(t *testing.T) {
	raw := `_Callback({"code":0,"data":{main:{total_number:3},data:[` +
		`{uin:'111',html:'\x3Cli class=\x22f-single f-s-s\x22>\x3Cp class=\x22txt-box-title ellipsis-one\x22>第一条\x3C\/p>\x3C\/li>',opuin:'1'},` +
		`{uin:'222',html:'\x3Cli class=\x22f-single f-s-s\x22>\x3Cp class=\x22txt-box-title ellipsis-one\x22>第二条\x3C\/p>\x3C\/li>',opuin:'2'},` +
		`{uin:'333',html:'\x3Cli class=\x22f-single f-s-s\x22>\x3Cp class=\x22txt-box-title ellipsis-one\x22>第三条\x3C\/p>\x3C\/li>',opuin:'3'}` +
		`]}})`
	got := processOldHTML(raw)
	for _, want := range []string{"第一条", "第二条", "第三条"} {
		if !contains(got, want) {
			t.Errorf("应包含 %q，实际: %q", want, got)
		}
	}
	if n := strings.Count(got, "f-single f-s-s"); n != 3 {
		t.Errorf("应提取出 3 个 li 条目，实际 %d", n)
	}
	c := &config{QQ: "999"}
	acts := c.parseActivities(got)
	if len(acts) != 3 {
		t.Fatalf("应解析出 3 条活动，实际 %d", len(acts))
	}
}

func TestUnescapeJS(t *testing.T) {
	cases := map[string]string{
		`a\tb`:     "a\tb",
		`a\nb`:     "a\nb",
		`a\\tb`:    "a\\tb", // \\ -> \, 然后字面 t
		`\x41\x42`: "AB",
		`中文`:       "中文",
		`it\'s`:    "it's",
		`a\/b`:     "a/b",
	}
	for in, want := range cases {
		if got := unescapeJS(in); got != want {
			t.Errorf("unescapeJS(%q)=%q, 期望 %q", in, got, want)
		}
	}
}

func TestRenderCardSkipsUndownloadedImages(t *testing.T) {
	okURL := "https://example.com/ok.jpg"
	brokenURL := "https://example.com/broken.jpg"
	dataURI := "data:image/jpeg;base64,AAAA"
	m := &msg{
		Content: "只有一张能显示的图",
		Pic: []pic{
			{URL: okURL},
			{URL: brokenURL},
		},
	}
	var b strings.Builder
	renderCard(&b, m, map[string]string{okURL: dataURI})
	got := b.String()

	if strings.Count(got, `<img `) != 1 {
		t.Fatalf("页面应只渲染 1 张成功下载的图片，实际 HTML: %s", got)
	}
	if !strings.Contains(got, `src="`+dataURI+`"`) {
		t.Errorf("页面应渲染成功内嵌的 data URI: %s", got)
	}
	if strings.Contains(got, brokenURL) {
		t.Errorf("下载失败的远程图片不应回退渲染成 broken img: %s", got)
	}
	if !strings.Contains(got, `class="imgs n1"`) {
		t.Errorf("图片网格应按实际可展示图片数计算为 n1: %s", got)
	}
}

func TestImageCandidatesKeepFallbackURLs(t *testing.T) {
	m := &msg{
		Pic: []pic{{
			Url3:     "https://example.com/original.jpg?w=100&h=100",
			URL:      "https://example.com/middle.jpg",
			Smallurl: "https://example.com/small.jpg",
		}},
	}

	items := imageCandidates(m)
	if len(items) != 1 {
		t.Fatalf("应生成 1 个图片候选组，实际 %d", len(items))
	}
	if items[0].Key != "https://example.com/original.jpg" {
		t.Fatalf("主键应优先使用原图地址，实际 %q", items[0].Key)
	}
	for _, want := range []string{
		"https://example.com/original.jpg",
		"https://example.com/original.jpg?w=100&h=100",
		"https://example.com/middle.jpg",
		"https://example.com/small.jpg",
	} {
		if !hasString(items[0].URLs, want) {
			t.Errorf("候选 URL 应包含 %q，实际 %#v", want, items[0].URLs)
		}
	}
}

func TestDownloadImageCandidateFallsBack(t *testing.T) {
	imageBytes := append([]byte{0xff, 0xd8, 0xff, 0xdb}, make([]byte, 256)...)
	item := imageCandidate{
		Key: "https://example.com/bad",
		URLs: []string{
			"https://example.com/bad",
			"https://example.com/ok",
		},
	}
	got, used, err := downloadImageCandidateWith(item, func(u string) ([]byte, error) {
		if strings.HasSuffix(u, "/ok") {
			return imageBytes, nil
		}
		return []byte("<html>not image</html>"), nil
	})
	if err != nil {
		t.Fatalf("备用地址可用时不应失败: %v", err)
	}
	if used != "https://example.com/ok" {
		t.Fatalf("应使用备用地址，实际 %q", used)
	}
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		preview := got
		if len(preview) > 40 {
			preview = preview[:40]
		}
		t.Fatalf("应返回 jpeg data URI，实际 %q", preview)
	}
}

func TestDownloadConcurrency(t *testing.T) {
	t.Setenv("QZONE_DOWNLOAD_CONCURRENCY", "")
	if got := downloadConcurrency(); got != defaultDownloadConcurrency {
		t.Fatalf("默认并发=%d，实际 %d", defaultDownloadConcurrency, got)
	}
	t.Setenv("QZONE_DOWNLOAD_CONCURRENCY", "24")
	if got := downloadConcurrency(); got != 24 {
		t.Fatalf("环境变量应设置并发 24，实际 %d", got)
	}
	t.Setenv("QZONE_DOWNLOAD_CONCURRENCY", "0")
	if got := downloadConcurrency(); got != defaultDownloadConcurrency {
		t.Fatalf("非法并发应回退默认值，实际 %d", got)
	}
	t.Setenv("QZONE_DOWNLOAD_CONCURRENCY", "100")
	if got := downloadConcurrency(); got != 64 {
		t.Fatalf("并发上限应为 64，实际 %d", got)
	}
}

func TestPrintQRInTerminal(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.Black)
	img.Set(1, 0, color.White)
	img.Set(0, 1, color.White)
	img.Set(1, 1, color.Black)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatalf("生成测试 PNG 失败: %v", err)
	}

	var out bytes.Buffer
	if err := printQRInTerminal(&out, pngBytes.Bytes()); err != nil {
		t.Fatalf("终端二维码渲染不应失败: %v", err)
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("输出应包含 ANSI 转义序列，实际 %q", out.String())
	}

	if err := printQRInTerminal(&out, []byte("bad png")); err == nil {
		t.Fatal("坏 PNG 应返回错误")
	}
}

func TestQRTerminalScale(t *testing.T) {
	if got := qrTerminalScale(72); got != 1 {
		t.Fatalf("72 列内不应缩放，实际 %d", got)
	}
	if got := qrTerminalScale(165); got != 3 {
		t.Fatalf("165 像素二维码应缩放为 3，实际 %d", got)
	}
}

func TestMonthWindow(t *testing.T) {
	w := monthWindow(2015, time.June)
	if w.Label != "2015-06" {
		t.Fatalf("窗口标签错误: %q", w.Label)
	}
	begin := time.Unix(w.Begin, 0)
	end := time.Unix(w.End, 0)
	if begin.Year() != 2015 || begin.Month() != time.June || begin.Day() != 1 {
		t.Fatalf("窗口开始时间错误: %v", begin)
	}
	if end.Year() != 2015 || end.Month() != time.July || end.Day() != 1 {
		t.Fatalf("窗口结束时间错误: %v", end)
	}
}

func TestExponentialOffsets(t *testing.T) {
	got := exponentialOffsets(20000)
	want := []int{0, 100, 200, 400, 800, 1600, 3200, 6400, 12800, 20000}
	if len(got) != len(want) {
		t.Fatalf("offset 数量错误: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offset[%d]=%d，期望 %d；完整结果 %#v", i, got[i], want[i], got)
		}
	}
}

func TestTotalNumber(t *testing.T) {
	got, ok := totalNumber([]byte(`cb({data:{main:{total_number:123}}})`))
	if !ok || got != 123 {
		t.Fatalf("应提取 total_number=123，实际 got=%d ok=%v", got, ok)
	}
	if _, ok := totalNumber([]byte(`cb({})`)); ok {
		t.Fatal("缺少 total_number 时 ok 应为 false")
	}
}

func TestFeedURLUsesProbeWindow(t *testing.T) {
	c := &config{QQ: "10000", GTK: "12345"}
	u := c.feedURL(1420041600, 1422720000, 400, 100)
	for _, want := range []string{
		"uin=10000",
		"begin_time=1420041600",
		"end_time=1422720000",
		"offset=400",
		"count=100",
		"g_tk=12345",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("URL 应包含 %q，实际 %s", want, u)
		}
	}
}

func TestWriteProbeRecord(t *testing.T) {
	var b bytes.Buffer
	rec := probeRecord{
		Type:        "request",
		Strategy:    "month-window",
		Window:      "2015-01",
		WindowBegin: 1420041600,
		WindowEnd:   1422720000,
		Offset:      0,
		Count:       100,
		Status:      "ok",
		ParsedCount: 2,
	}
	if err := writeProbeRecord(&b, rec); err != nil {
		t.Fatalf("写入 JSONL 失败: %v", err)
	}
	if !strings.HasSuffix(b.String(), "\n") {
		t.Fatalf("JSONL 应以换行结尾: %q", b.String())
	}
	var got probeRecord
	if err := json.Unmarshal(bytes.TrimSpace(b.Bytes()), &got); err != nil {
		t.Fatalf("JSONL 应可反序列化: %v", err)
	}
	if got.Window != "2015-01" || got.ParsedCount != 2 {
		t.Fatalf("记录内容错误: %#v", got)
	}
}

func TestWriteActivityRecord(t *testing.T) {
	window := monthWindow(2015, time.June)
	act := activity{
		SenderQQ:   "111222",
		SenderName: "小明",
		ReceiverQQ: "999",
		Content:    "2015 的夏天",
		TimeText:   "2015年6月1日 12:00",
		Timestamp:  time.Date(2015, time.June, 1, 12, 0, 0, 0, time.Local),
		ImageURLs:  []string{"https://example.com/a.jpg?w=100&h=100"},
		Type:       typeLike,
	}
	rec := makeActivityRecord(window, 400, "offset-list", 3, act)

	var b bytes.Buffer
	if err := writeActivityRecord(&b, rec); err != nil {
		t.Fatalf("写入 activity JSONL 失败: %v", err)
	}
	var got activityRecord
	if err := json.Unmarshal(bytes.TrimSpace(b.Bytes()), &got); err != nil {
		t.Fatalf("activity JSONL 应可反序列化: %v", err)
	}
	if got.Type != "activity" || got.ActivityType != "like" || got.ActivityTypeCode != int(typeLike) {
		t.Fatalf("activity 类型字段错误: %#v", got)
	}
	if got.Window != "2015-06" || got.Offset != 400 || got.Index != 3 {
		t.Fatalf("activity probe 定位字段错误: %#v", got)
	}
	if got.Content != "2015 的夏天" || got.TimestampUnix == 0 || len(got.ImageURLs) != 1 {
		t.Fatalf("activity 内容字段错误: %#v", got)
	}
}

func TestAnalyzeProbeFile(t *testing.T) {
	path := t.TempDir() + "/probe_depth_test.jsonl"
	var b bytes.Buffer
	records := []probeRecord{
		{
			Type:             "request",
			Strategy:         "month-window",
			Window:           "2017-09",
			Status:           "ok",
			ParsedCount:      2,
			MinTimeText:      "2017年9月4日",
			MinTimestampUnix: time.Date(2017, time.September, 4, 0, 0, 0, 0, time.Local).Unix(),
		},
		{
			Type:             "request",
			Strategy:         "month-window",
			Window:           "2015-06",
			Status:           "ok",
			ParsedCount:      1,
			MinTimeText:      "2015年6月1日",
			MinTimestampUnix: time.Date(2015, time.June, 1, 0, 0, 0, 0, time.Local).Unix(),
		},
		{
			Type:   "request",
			Status: "error",
		},
	}
	for _, rec := range records {
		if err := writeProbeRecord(&b, rec); err != nil {
			t.Fatalf("写入测试记录失败: %v", err)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatalf("写入测试 JSONL 失败: %v", err)
	}

	got, err := analyzeProbeFile(path)
	if err != nil {
		t.Fatalf("分析 JSONL 不应失败: %v", err)
	}
	if got.RequestCount != 3 || got.ErrorCount != 1 || got.ParsedActivityCount != 3 {
		t.Fatalf("统计错误: %#v", got)
	}
	if got.ErrorCounts[""] != 1 {
		t.Fatalf("错误类型统计应包含空错误 1 次: %#v", got.ErrorCounts)
	}
	if !got.FoundBefore20170903 || !got.Found2015 {
		t.Fatalf("应识别突破和 2015 记录: %#v", got)
	}
	if got.EarliestWindow != "2015-06" {
		t.Fatalf("最早窗口应为 2015-06，实际 %#v", got)
	}
}

func TestAnalyzeProbeFileWithActivityRecords(t *testing.T) {
	path := t.TempDir() + "/probe_offset_list_test.jsonl"
	var b bytes.Buffer
	if err := writeProbeRecord(&b, probeRecord{Type: "request", Status: "ok", ParsedCount: 1}); err != nil {
		t.Fatalf("写入 request 记录失败: %v", err)
	}
	rec := activityRecord{
		Type:          "activity",
		Strategy:      "offset-list",
		Window:        "all-time",
		Content:       "更早的记录",
		TimeText:      "2015年1月2日 08:00",
		TimestampUnix: time.Date(2015, time.January, 2, 8, 0, 0, 0, time.Local).Unix(),
		ActivityType:  "like",
	}
	if err := writeActivityRecord(&b, rec); err != nil {
		t.Fatalf("写入 activity 记录失败: %v", err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatalf("写入测试 JSONL 失败: %v", err)
	}

	got, err := analyzeProbeFile(path)
	if err != nil {
		t.Fatalf("分析 JSONL 不应失败: %v", err)
	}
	if got.RequestCount != 1 || got.ActivityRecordCount != 1 {
		t.Fatalf("统计数量错误: %#v", got)
	}
	if !got.FoundBefore20170903 || !got.Found2015 || got.EarliestTimeText != "2015年1月2日 08:00" {
		t.Fatalf("activity 明细应参与最早时间判断: %#v", got)
	}
}

func TestBuildPreviewYearFromProbeFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/probe_offset_list_test.jsonl"
	var b bytes.Buffer
	window2015 := monthWindow(2015, time.June)
	acts := []activity{
		{
			SenderQQ:   "111",
			SenderName: "小明",
			ReceiverQQ: "999",
			Content:    "2015 的夏天",
			TimeText:   "2015年6月1日 12:00",
			Timestamp:  time.Date(2015, time.June, 1, 12, 0, 0, 0, time.Local),
			ImageURLs:  []string{"https://example.com/a.jpg?w=100&h=100"},
			Type:       typeLike,
		},
		{
			SenderQQ:   "222",
			SenderName: "小红",
			ReceiverQQ: "999",
			Content:    "2015 的夏天",
			TimeText:   "2015年6月1日 12:05",
			Timestamp:  time.Date(2015, time.June, 1, 12, 5, 0, 0, time.Local),
			Type:       typeComment,
		},
		{
			SenderQQ:   "333",
			SenderName: "小刚",
			ReceiverQQ: "999",
			Content:    "2016 的记录",
			TimeText:   "2016年1月1日 12:00",
			Timestamp:  time.Date(2016, time.January, 1, 12, 0, 0, 0, time.Local),
			Type:       typeLike,
		},
	}
	for i, act := range acts {
		if err := writeActivityRecord(&b, makeActivityRecord(window2015, 3000, "offset-list", i, act)); err != nil {
			t.Fatalf("写入 activity 记录失败: %v", err)
		}
	}
	// 重复写入同一条 activity，预览生成前应去重，避免互动数翻倍。
	if err := writeActivityRecord(&b, makeActivityRecord(window2015, 3100, "offset-list", 9, acts[0])); err != nil {
		t.Fatalf("写入重复 activity 记录失败: %v", err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatalf("写入测试 JSONL 失败: %v", err)
	}

	result, err := buildPreviewYearFromProbeFile(path, 2015, dir)
	if err != nil {
		t.Fatalf("生成预览不应失败: %v", err)
	}
	if result.ActivityRecords != 4 || result.YearActivities != 2 || result.Reconstructed != 1 {
		t.Fatalf("预览统计错误: %#v", result)
	}
	htmlBytes, err := os.ReadFile(result.OutputPath)
	if err != nil {
		t.Fatalf("读取预览 HTML 失败: %v", err)
	}
	html := string(htmlBytes)
	if !strings.Contains(html, "2015 的夏天") {
		t.Fatalf("预览应包含 2015 内容: %s", html)
	}
	if strings.Contains(html, "2016 的记录") {
		t.Fatalf("预览不应包含 2016 内容: %s", html)
	}
	if strings.Count(html, "♥ 1") != 1 {
		t.Fatalf("重复 activity 不应导致点赞数翻倍: %s", html)
	}
	if !strings.Contains(html, `src="https://example.com/a.jpg"`) {
		t.Fatalf("预览应引用原图 URL 但不下载: %s", html)
	}
}

func TestOffsetRange(t *testing.T) {
	start, end, step := offsetRange(nil)
	if start != 3300 || end != 6300 || step != 100 {
		t.Fatalf("默认 offset 范围错误: %d %d %d", start, end, step)
	}

	start, end, step = offsetRange([]string{
		"--offset-start", "3200",
		"--offset-end", "4200",
		"--offset-step", "50",
	})
	if start != 3200 || end != 4200 || step != 50 {
		t.Fatalf("自定义 offset 范围错误: %d %d %d", start, end, step)
	}

	start, end, step = offsetRange([]string{
		"--offset-start", "-1",
		"--offset-end", "10",
		"--offset-step", "0",
	})
	if start != 0 || end != 10 || step != 100 {
		t.Fatalf("非法 offset 参数应被修正: %d %d %d", start, end, step)
	}
}

func TestIntListArg(t *testing.T) {
	got := intListArg([]string{"--offsets", "0,100,bad,100,-1,3200"}, "--offsets", []int{1})
	want := []int{0, 100, 3200}
	if len(got) != len(want) {
		t.Fatalf("offset 列表长度错误: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offset[%d]=%d，期望 %d；完整结果 %#v", i, got[i], want[i], got)
		}
	}

	got = intListArg(nil, "--offsets", []int{1, 2})
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("缺少参数时应返回默认列表，实际 %#v", got)
	}
}

func TestProbeDelay(t *testing.T) {
	if got := probeDelay(nil, 900*time.Millisecond); got != 900*time.Millisecond {
		t.Fatalf("默认 probe delay 错误: %s", got)
	}
	if got := probeDelay([]string{"--probe-delay-ms", "5000"}, 900*time.Millisecond); got != 5*time.Second {
		t.Fatalf("自定义 probe delay 错误: %s", got)
	}
	if got := probeDelay([]string{"--probe-delay-ms", "50"}, 900*time.Millisecond); got != 200*time.Millisecond {
		t.Fatalf("probe delay 下限应为 200ms，实际 %s", got)
	}
	if got := probeDelay([]string{"--probe-delay-ms", "60000"}, 900*time.Millisecond); got != 30*time.Second {
		t.Fatalf("probe delay 上限应为 30s，实际 %s", got)
	}
	if got := probeDelay([]string{"--probe-delay-ms", "bad"}, 900*time.Millisecond); got != 900*time.Millisecond {
		t.Fatalf("非法 probe delay 应回退默认值，实际 %s", got)
	}
}

func TestProbeCooldownWaitIfNeeded(t *testing.T) {
	var slept []time.Duration
	cooldown := newProbeCooldownWithSleep(nil, func(d time.Duration) {
		slept = append(slept, d)
	})
	rec := probeRecord{Status: "error", Error: "HTTP 501"}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
	for i, delay := range want {
		if !cooldown.waitIfNeeded("test", i, rec) {
			t.Fatalf("第 %d 次 HTTP 501 应触发冷却", i+1)
		}
		if slept[i] != delay {
			t.Fatalf("第 %d 次冷却=%s，期望 %s", i+1, slept[i], delay)
		}
	}
	if cooldown.waitIfNeeded("test", 99, rec) {
		t.Fatal("冷却次数用尽后不应继续 sleep")
	}

	cooldown.reset()
	if !cooldown.waitIfNeeded("test", 100, rec) {
		t.Fatal("reset 后应重新从 5 分钟开始冷却")
	}
	if slept[len(slept)-1] != 5*time.Minute {
		t.Fatalf("reset 后冷却应回到 5 分钟，实际 %s", slept[len(slept)-1])
	}
}

func TestProbeCooldownCanBeDisabled(t *testing.T) {
	var slept []time.Duration
	cooldown := newProbeCooldownWithSleep([]string{"--no-auto-cooldown"}, func(d time.Duration) {
		slept = append(slept, d)
	})
	rec := probeRecord{Status: "error", Error: "HTTP 501"}
	if cooldown.waitIfNeeded("test", 0, rec) {
		t.Fatal("--no-auto-cooldown 时不应触发冷却")
	}
	if len(slept) != 0 {
		t.Fatalf("禁用冷却后不应 sleep，实际 %#v", slept)
	}
}

func TestIsProbeCooldownError(t *testing.T) {
	if !isProbeCooldownError(probeRecord{Status: "error", Error: "HTTP 501"}) {
		t.Fatal("HTTP 501 应触发自动冷却")
	}
	if !isProbeCooldownError(probeRecord{Status: "error", Error: "HTTP 429"}) {
		t.Fatal("HTTP 429 应触发自动冷却")
	}
	if isProbeCooldownError(probeRecord{Status: "error", Error: "HTTP 500"}) {
		t.Fatal("HTTP 500 暂不应触发自动冷却")
	}
	if isProbeCooldownError(probeRecord{Status: "ok"}) {
		t.Fatal("ok 记录不应触发自动冷却")
	}
}

func TestDefaultOffsetListIncludesSparseGrid(t *testing.T) {
	got := defaultOffsetList()
	for _, want := range []int{0, 1600, 3000, 3100, 3200, 3400, 4800, 6400, 8000, 9600, 12800} {
		if !hasInt(got, want) {
			t.Fatalf("默认 offset 列表应包含 %d，实际 %#v", want, got)
		}
	}
}

func TestLatestProbePathSupportsRangeLogs(t *testing.T) {
	dir := t.TempDir()
	depth := dir + "/probe_depth_20260101_000002.jsonl"
	rangeLog := dir + "/probe_offset_range_20260101_000001.jsonl"
	listLog := dir + "/probe_offset_list_20260101_000004.jsonl"
	if err := os.WriteFile(depth, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("写入 depth 日志失败: %v", err)
	}
	if err := os.WriteFile(rangeLog, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("写入 range 日志失败: %v", err)
	}

	got, err := latestProbePath(dir)
	if err != nil {
		t.Fatalf("查找最新日志不应失败: %v", err)
	}
	if got != depth {
		t.Fatalf("应按时间戳选择最新日志，实际 %q", got)
	}

	newerRangeLog := dir + "/probe_offset_range_20260101_000003.jsonl"
	if err := os.WriteFile(newerRangeLog, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("写入更新 range 日志失败: %v", err)
	}
	got, err = latestProbePath(dir)
	if err != nil {
		t.Fatalf("查找最新日志不应失败: %v", err)
	}
	if got != newerRangeLog {
		t.Fatalf("应选择更新的 range 日志，实际 %q", got)
	}
	if err := os.WriteFile(listLog, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("写入 list 日志失败: %v", err)
	}
	got, err = latestProbePath(dir)
	if err != nil {
		t.Fatalf("查找最新日志不应失败: %v", err)
	}
	if got != listLog {
		t.Fatalf("应选择最新 list 日志，实际 %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func hasInt(items []int, want int) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func hasString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// 类名顺序反过来（f-s-s 在 f-single 前面）时，解析器仍应能切出条目。
// 这是“在别人账号上重建出 0 条”的一类隐患，必须保证顺序无关。
func TestParseActivitiesClassOrderIndependent(t *testing.T) {
	feed := `<li class="f-s-s f-single">` +
		`<a class="q_namecard f-name" link="nameCard_111" href="#">甲</a>` +
		`<span class="state">赞了我的说说</span>` +
		`<div class="info-detail">2020年1月1日 10:00</div>` +
		`<p class="ellipsis-one txt-box-title">某条说说内容</p>` +
		`</li>`
	c := &config{QQ: "1"}
	acts := c.parseActivities(feed)
	if len(acts) != 1 {
		t.Fatalf("类名顺序反转时应解析出 1 条，实际 %d", len(acts))
	}
	if acts[0].Type != typeLike || acts[0].Content != "某条说说内容" {
		t.Errorf("解析结果不正确: type=%d content=%q", acts[0].Type, acts[0].Content)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---------- 动态流缓存（cache-first harvester）测试 ----------

// 构造一段最小可解析的动态流 JSONP 响应：含一个 li.f-single.f-s-s 活动。
func fakeFeedBody(content, timeText string) []byte {
	html := `<li class="f-single f-s-s">` +
		`<a class="f-name q_namecard" link="nameCard_111" href="#">某人</a>` +
		`<span class="state">赞了我的说说</span>` +
		`<div class="info-detail">` + timeText + `</div>` +
		`<p class="txt-box-title ellipsis-one">` + content + `</p>` +
		`</li>`
	return []byte(`_Callback({"code":0,"total_number:1,"data":{data:[{html:'` + html + `',opuin:'111'}]}})`)
}

func emptyFeedBody() []byte {
	return []byte(`_Callback({"code":0})`)
}

func TestCachePathPaddingAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := cachePath(dir, 3200)
	if !strings.HasSuffix(p, "off_0003200.body") {
		t.Fatalf("缓存路径零填充错误: %s", p)
	}
	if isCached(dir, 3200) {
		t.Fatal("空目录不应命中缓存")
	}
	body := fakeFeedBody("内容", "2015年6月1日 10:00")
	if err := writeCache(dir, 3200, body); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	if !isCached(dir, 3200) {
		t.Fatal("写入后应命中缓存")
	}
	got, err := readCache(dir, 3200)
	if err != nil || string(got) != string(body) {
		t.Fatalf("读缓存不一致: err=%v", err)
	}
}

func TestCachedOffsetsSorted(t *testing.T) {
	dir := t.TempDir()
	for _, o := range []int{3200, 0, 800, 100} {
		if err := writeCache(dir, o, emptyFeedBody()); err != nil {
			t.Fatal(err)
		}
	}
	offs, err := cachedOffsets(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 100, 800, 3200}
	if len(offs) != len(want) {
		t.Fatalf("缓存 offset 数=%d，期望 %d", len(offs), len(want))
	}
	for i := range want {
		if offs[i] != want[i] {
			t.Fatalf("offset 未升序: %v", offs)
		}
	}
}

func TestCachedOffsetsEmptyDir(t *testing.T) {
	offs, err := cachedOffsets(t.TempDir())
	if err != nil || len(offs) != 0 {
		t.Fatalf("空目录应返回空且无错误: offs=%v err=%v", offs, err)
	}
}

func TestHarvestOffsetsGrid(t *testing.T) {
	offs := harvestOffsets([]string{"--offset-start", "0", "--offset-end", "300", "--offset-step", "100"})
	want := []int{0, 100, 200, 300}
	if len(offs) != len(want) {
		t.Fatalf("网格大小=%d，期望 %d (%v)", len(offs), len(want), offs)
	}
	for i := range want {
		if offs[i] != want[i] {
			t.Fatalf("网格不对: %v", offs)
		}
	}
}

func TestHarvestPageCacheHitSkipsNetwork(t *testing.T) {
	dir := t.TempDir()
	cfg := &config{QQ: "999"}
	writeCache(dir, 100, fakeFeedBody("命中", "2016年1月1日 12:00"))
	rec, fromCache, fetched := cfg.harvestPage(dir, 100, nil)
	if !fromCache {
		t.Fatal("已缓存的 offset 应走缓存，不发请求")
	}
	if fetched {
		t.Fatal("缓存命中不应标记为新抓取")
	}
	if rec.Status != "ok" || rec.ParsedCount != 1 {
		t.Fatalf("缓存命中应能解析: status=%s parsed=%d", rec.Status, rec.ParsedCount)
	}
}

func TestAnalyzeCacheBoundaryComplete(t *testing.T) {
	dir := t.TempDir()
	cfg := &config{QQ: "999"}
	// offset 0 有数据（较新），offset 100 有数据（较老），之后 20 个连续空页 -> 边界可判定
	writeCache(dir, 0, fakeFeedBody("较新", "2020年1月1日 10:00"))
	writeCache(dir, 100, fakeFeedBody("较老", "2017年9月3日 13:49"))
	for i := 2; i < 22; i++ {
		writeCache(dir, i*100, emptyFeedBody())
	}
	rep, all, err := analyzeCache(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NonEmptyCount != 2 {
		t.Errorf("有数据页应为 2，实际 %d", rep.NonEmptyCount)
	}
	if rep.DeepestWithData != 100 {
		t.Errorf("最深有数据 offset 应为 100，实际 %d", rep.DeepestWithData)
	}
	if rep.TailEmptyRun < 20 {
		t.Errorf("尾部连续空页应≥20，实际 %d", rep.TailEmptyRun)
	}
	if len(rep.GapOffsets) != 0 {
		t.Errorf("无缺口网格应为 0 缺口，实际 %v", rep.GapOffsets)
	}
	if rep.GlobalMinTimeText != "2017年9月3日 13:49" {
		t.Errorf("全局最早活动应为 2017-09-03，实际 %q", rep.GlobalMinTimeText)
	}
	if len(all) != 2 {
		t.Errorf("去重后活动应为 2 条，实际 %d", len(all))
	}
}

func TestAnalyzeCacheDetectsGap(t *testing.T) {
	dir := t.TempDir()
	cfg := &config{QQ: "999"}
	// 缓存 0 和 200，缺 100 -> 应报告缺口
	writeCache(dir, 0, fakeFeedBody("a", "2020年1月1日 10:00"))
	writeCache(dir, 200, fakeFeedBody("b", "2019年1月1日 10:00"))
	rep, _, err := analyzeCache(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.GapOffsets) != 1 || rep.GapOffsets[0] != 100 {
		t.Errorf("应检测到缺口 offset=100，实际 %v", rep.GapOffsets)
	}
}

func TestAnalyzeCacheEmptyErrors(t *testing.T) {
	cfg := &config{QQ: "999"}
	if _, _, err := analyzeCache(cfg, t.TempDir()); err == nil {
		t.Fatal("空缓存应报错，提示先 harvest")
	}
}
