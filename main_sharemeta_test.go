package main

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shareMetaFixture 是 renderShareMeta 的最小 HTML 夹具:带占位符,不依赖 embed 进去的
// web/dist/index.html(CI 里那只是一个 0 字节占位文件)。
const shareMetaFixture = `<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <!--share-meta-->
  </head>
  <body></body>
</html>
`

// shareMetaFixtureNoPlaceholder 模拟前端构建把占位符注释吃掉的情况。
const shareMetaFixtureNoPlaceholder = `<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
  </head>
  <body></body>
</html>
`

func TestRenderShareMeta(t *testing.T) {
	tests := []struct {
		name            string
		cfg             shareMetaSettings
		wantImageMeta   bool
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "absolute logo with configured server address",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com/",
				Logo:          "https://cdn.example.com/apex.png",
			},
			wantImageMeta: true,
			wantContains: []string{
				`property="og:url" content="https://api.example.com"`,
				`property="og:type" content="website"`,
				`property="og:site_name" content="Apex AI"`,
				`property="og:title" content="Apex AI"`,
				`property="og:description" content="Unified AI API gateway and admin dashboard."`,
				`property="og:image" content="https://cdn.example.com/apex.png"`,
				`rel="apple-touch-icon" href="https://cdn.example.com/apex.png"`,
				`name="twitter:card" content="summary_large_image"`,
				`name="twitter:image" content="https://cdn.example.com/apex.png"`,
			},
		},
		{
			name: "root-relative logo is absolutized with server address",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com/",
				Logo:          "/custom/logo.png",
			},
			wantImageMeta: true,
			wantContains: []string{
				`property="og:url" content="https://api.example.com"`,
				`property="og:image" content="https://api.example.com/custom/logo.png"`,
				`rel="apple-touch-icon" href="https://api.example.com/custom/logo.png"`,
			},
		},
		{
			name: "logo without leading slash is not a usable path",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com",
				Logo:          "logo.png",
			},
			// 既不是绝对 URL 也不是根相对路径,不能靠猜测补 "/" 后拼出地址。
			wantImageMeta:   false,
			wantNotContains: []string{"logo.png"},
		},
		{
			name: "uppercase absolute scheme logo is recognized",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com",
				Logo:          "HTTPS://CDN.example.com/a.png",
			},
			wantImageMeta: true,
			wantContains:  []string{`property="og:image" content="HTTPS://CDN.example.com/a.png"`},
		},
		{
			name: "root-relative logo without server address is skipped",
			cfg: shareMetaSettings{
				SiteName: "Apex AI",
				Logo:     "/custom/logo.png",
			},
			wantImageMeta:   false,
			wantContains:    []string{`property="og:site_name" content="Apex AI"`},
			wantNotContains: []string{"/custom/logo.png", `property="og:url"`},
		},
		{
			name: "default logo without server address is skipped",
			cfg:  shareMetaSettings{SiteName: "Apex AI"},
			// ServerAddress 默认空:这时不能再发 content="/logo.png" 这种相对地址。
			wantImageMeta:   false,
			wantNotContains: []string{`content="/logo.png"`},
		},
		{
			name: "data uri logo is unusable and not mangled",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com",
				Logo:          "data:image/png;base64,iVBORw0KGgo=",
			},
			wantImageMeta:   false,
			wantNotContains: []string{"data:image/png"},
		},
		{
			name: "protocol-relative logo is unusable and not mangled",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com",
				Logo:          "//cdn.example.com/a.png",
			},
			wantImageMeta:   false,
			wantNotContains: []string{"cdn.example.com", "https://api.example.com//cdn"},
		},
		{
			name: "non-absolute server address cannot absolutize logo",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "api.example.com",
				Logo:          "/logo.png",
			},
			wantImageMeta:   false,
			wantNotContains: []string{`property="og:url"`, `property="og:image"`},
		},
		{
			name: "empty site name falls back to default brand",
			cfg: shareMetaSettings{
				ServerAddress: "https://api.example.com",
				Logo:          "/logo.png",
			},
			wantImageMeta: true,
			wantContains: []string{
				`property="og:site_name" content="New API"`,
				`property="og:title" content="New API"`,
			},
		},
		{
			name: "custom description overrides default",
			cfg: shareMetaSettings{
				SiteName:      "Apex AI",
				ServerAddress: "https://api.example.com",
				Logo:          "/logo.png",
				Description:   "Self-hosted gateway.",
			},
			wantImageMeta: true,
			wantContains:  []string{`property="og:description" content="Self-hosted gateway."`},
		},
		{
			name: "special characters in site name stay inside the attribute",
			cfg: shareMetaSettings{
				SiteName:      `A "B" & <C>`,
				ServerAddress: "https://api.example.com",
				Logo:          "/logo.png",
			},
			wantImageMeta:   true,
			wantContains:    []string{`property="og:title" content="A &#34;B&#34; &amp; &lt;C&gt;"`},
			wantNotContains: []string{`content="A "B"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderShareMeta([]byte(shareMetaFixture), tc.cfg)
			require.NoError(t, err)
			got := string(out)

			assert.NotContains(t, got, "<!--share-meta-->", "占位符应被替换掉")
			assert.Contains(t, got, "</head>", "原 HTML 结构不能被破坏")

			for _, want := range tc.wantContains {
				assert.Contains(t, got, want)
			}
			for _, unwanted := range tc.wantNotContains {
				assert.NotContains(t, got, unwanted)
			}
			if tc.wantImageMeta {
				assert.Contains(t, got, `property="og:image"`)
				assert.Contains(t, got, `rel="apple-touch-icon"`)
				assert.Contains(t, got, `name="twitter:image"`)
			} else {
				assert.NotContains(t, got, `property="og:image"`)
				assert.NotContains(t, got, `rel="apple-touch-icon"`)
				assert.NotContains(t, got, `name="twitter:image"`)
			}
		})
	}
}

func TestRenderShareMetaInjectsOnceBeforeHeadEnd(t *testing.T) {
	cfg := shareMetaSettings{
		SiteName:      "Apex AI",
		ServerAddress: "https://api.example.com",
		Logo:          "/logo.png",
	}

	out, err := renderShareMeta([]byte(shareMetaFixture), cfg)
	require.NoError(t, err)
	got := string(out)

	assert.Equal(t, 1, strings.Count(got, `property="og:title"`), "元标签只能注入一次")
	metaIdx := strings.Index(got, `property="og:title"`)
	headIdx := strings.Index(got, "</head>")
	require.NotEqual(t, -1, metaIdx, "og:title 未注入")
	require.NotEqual(t, -1, headIdx, "</head> 丢失")
	assert.Less(t, metaIdx, headIdx, "元标签必须落在 </head> 之前")
}

func TestRenderShareMetaFallsBackBeforeHeadEndWhenPlaceholderMissing(t *testing.T) {
	cfg := shareMetaSettings{
		SiteName:      "Apex AI",
		ServerAddress: "https://api.example.com",
		Logo:          "https://cdn.example.com/apex.png",
	}

	out, err := renderShareMeta([]byte(shareMetaFixtureNoPlaceholder), cfg)
	require.NoError(t, err)
	got := string(out)

	assert.Contains(t, got, `property="og:title" content="Apex AI"`)
	assert.Contains(t, got, `property="og:image" content="https://cdn.example.com/apex.png"`)
	assert.Equal(t, 1, strings.Count(got, "</head>"), "只应替换第一个 </head>")

	metaIdx := strings.Index(got, `property="og:title"`)
	headIdx := strings.Index(got, "</head>")
	require.NotEqual(t, -1, metaIdx, "og:title 未注入")
	require.NotEqual(t, -1, headIdx, "</head> 丢失")
	assert.Less(t, metaIdx, headIdx, "兜底注入必须落在 </head> 之前")
}

func TestRenderShareMetaErrorsWithoutInjectionPoint(t *testing.T) {
	out, err := renderShareMeta([]byte("<html><body>no head</body></html>"), shareMetaSettings{SiteName: "Apex AI"})
	require.Error(t, err)
	assert.Nil(t, out)
}

// InjectShareMeta 是启动入口:确认它一次性从 common.OptionMap 读取三项配置并写回 indexPage,
// 而不是继续依赖 common.SystemName / common.Logo 这类无锁镜像变量。
func TestInjectShareMetaReadsOptionMapAndUpdatesIndexPage(t *testing.T) {
	previousPage := indexPage
	previousOptionMap := common.OptionMap
	t.Cleanup(func() {
		indexPage = previousPage
		common.OptionMap = previousOptionMap
	})

	indexPage = []byte(shareMetaFixture)
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{
		"SystemName":    "Apex AI",
		"Logo":          "/logo.png",
		"ServerAddress": "https://api.example.com",
	}
	common.OptionMapRWMutex.Unlock()

	InjectShareMeta()

	got := string(indexPage)
	assert.Contains(t, got, `property="og:site_name" content="Apex AI"`)
	assert.Contains(t, got, `property="og:image" content="https://api.example.com/logo.png"`)
	assert.NotContains(t, got, "<!--share-meta-->")
}
