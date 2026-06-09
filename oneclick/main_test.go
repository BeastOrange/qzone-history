package main

import (
	"strings"
	"testing"
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

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
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
