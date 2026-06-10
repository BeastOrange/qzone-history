package main

import (
	"strings"
	"testing"
)

func TestUnwrapJSONP(t *testing.T) {
	cases := map[string]string{
		`_cb({"a":1})`:       `{"a":1}`,
		`callback({"x":2});`: `{"x":2}`,
		`{"raw":3}`:          `{"raw":3}`, // 没有外壳时原样返回
	}
	for in, want := range cases {
		if got := string(unwrapJSONP([]byte(in))); got != want {
			t.Errorf("unwrapJSONP(%q)=%q, 期望 %q", in, got, want)
		}
	}
}

func TestParseUploadTime(t *testing.T) {
	if got := parseUploadTime("2014-06-13 20:21:33"); got == 0 {
		t.Error("应能解析完整时间戳")
	}
	if got := parseUploadTime("2015-01-01"); got == 0 {
		t.Error("应能解析仅日期")
	}
	if got := parseUploadTime(""); got != 0 {
		t.Errorf("空串应返回 0，实际 %d", got)
	}
	// 同一时刻解析应一致且 2014 < 2015
	a := parseUploadTime("2014-06-13 20:21:33")
	b := parseUploadTime("2015-06-13 20:21:33")
	if !(a < b) {
		t.Errorf("2014 应早于 2015: a=%d b=%d", a, b)
	}
}

func TestPhotoToMsg(t *testing.T) {
	al := album{ID: "X1", Name: "毕业旅行"}
	p := photo{Desc: "在海边", Raw: "https://x/raw.jpg?w=200&h=200", URL: "https://x/u.jpg", UploadTime: "2015-07-01 10:00:00"}
	m := photoToMsg(al, p)
	if !strings.Contains(m.Content, "毕业旅行") || !strings.Contains(m.Content, "在海边") {
		t.Errorf("卡片内容应含相册名+描述: %q", m.Content)
	}
	if m.CreatedTime == 0 {
		t.Error("应解析出上传时间")
	}
	if len(m.ExtraImgs) != 1 {
		t.Fatalf("应有 1 张图，实际 %d", len(m.ExtraImgs))
	}
	// 优先用 raw，且经 toOriginal 去掉缩略图尺寸参数
	if strings.Contains(m.ExtraImgs[0], "w=200") {
		t.Errorf("应去掉缩略图尺寸参数: %q", m.ExtraImgs[0])
	}
}

func TestPhotoToMsgNoDescNoRaw(t *testing.T) {
	al := album{ID: "X2", Name: "日常"}
	p := photo{URL: "https://x/u.jpg", UploadTime: "2013-03-03 03:03:03"}
	m := photoToMsg(al, p)
	if !strings.Contains(m.Content, "日常") {
		t.Errorf("无描述时也应含相册名: %q", m.Content)
	}
	if len(m.ExtraImgs) != 1 || !strings.Contains(m.ExtraImgs[0], "u.jpg") {
		t.Errorf("无 raw 时应回退到 url: %v", m.ExtraImgs)
	}
}

func TestRecoverCacheDir(t *testing.T) {
	if got := recoverCacheDir("/tmp/x"); !strings.HasSuffix(got, "recover_cache") {
		t.Errorf("缓存目录名错误: %s", got)
	}
}
