package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
)

// indexPage 是 embed 全局,InjectShareMeta 消费 <!--share-meta--> 占位符后不可逆;
// 每个用例前必须恢复到注入前的原始内容。
var pristineIndexPage []byte

func resetIndexPage() {
	if pristineIndexPage == nil {
		pristineIndexPage = make([]byte, len(indexPage))
		copy(pristineIndexPage, indexPage)
		return
	}
	indexPage = make([]byte, len(pristineIndexPage))
	copy(indexPage, pristineIndexPage)
}

func TestInjectShareMeta(t *testing.T) {
	resetIndexPage()
	common.SystemName = "Apex AI"
	common.Logo = "https://cdn.example.com/apex.png"
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{"ServerAddress": "https://api.example.com/"}
	common.OptionMapRWMutex.Unlock()

	InjectShareMeta()
	out := string(indexPage)

	wants := []string{
		`property="og:type" content="website"`,
		`property="og:url" content="https://api.example.com"`,
		`property="og:site_name" content="Apex AI"`,
		`property="og:title" content="Apex AI"`,
		`property="og:image" content="https://cdn.example.com/apex.png"`,
		`rel="apple-touch-icon" href="https://cdn.example.com/apex.png"`,
		`name="twitter:card" content="summary_large_image"`,
		`name="twitter:image" content="https://cdn.example.com/apex.png"`,
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("注入结果缺少 %q\n--- 注入后 indexPage ---\n%s", want, out)
		}
	}
}

func TestInjectShareMetaRelativeLogoAbsolutized(t *testing.T) {
	resetIndexPage()
	common.SystemName = "Apex AI"
	common.Logo = "/custom/logo.png"
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{"ServerAddress": "https://api.example.com"}
	common.OptionMapRWMutex.Unlock()

	InjectShareMeta()
	out := string(indexPage)
	if !strings.Contains(out, `content="https://api.example.com/custom/logo.png"`) {
		t.Errorf("相对 Logo 未被拼上 baseURL\n%s", out)
	}
	if strings.Contains(out, `content="/logo.png"`) {
		t.Errorf("不应出现裸 /logo.png 兜底(默认 New API 图标)\n%s", out)
	}
}

// 占位符若被前端构建剥掉,注入要退到 </head> 前,不能静默失效。
func TestInjectShareMetaFallbackWhenPlaceholderMissing(t *testing.T) {
	resetIndexPage()
	// 模拟 dist 里没有 <!--share-meta--> 注释(构建把注释吃掉的最坏情形)
	placeholder := []byte("    <!--share-meta-->\n")
	stripped := bytes.ReplaceAll(indexPage, placeholder, nil)
	if bytes.Equal(stripped, indexPage) {
		t.Fatal("测试前提失败:原始 indexPage 里没有占位符")
	}
	indexPage = stripped

	common.SystemName = "Apex AI"
	common.Logo = "https://cdn.example.com/apex.png"
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{"ServerAddress": "https://api.example.com"}
	common.OptionMapRWMutex.Unlock()

	InjectShareMeta()
	out := string(indexPage)
	if !strings.Contains(out, `property="og:title" content="Apex AI"`) {
		t.Errorf("占位符缺失时兜底注入失败(应插到 </head> 前)\n%s", out)
	}
	if !strings.Contains(out, `property="og:image" content="https://cdn.example.com/apex.png"`) {
		t.Errorf("兜底注入缺 og:image\n%s", out)
	}
	if !strings.Contains(out, "</head>") {
		t.Errorf("兜底注入吞掉了 </head>\n%s", out)
	}
}
