package main

import "testing"

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
