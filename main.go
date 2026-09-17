package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	"github.com/QuantumNous/new-api/relay"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	_ "github.com/QuantumNous/new-api/setting/performance_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	_ "net/http/pprof"
)

//go:embed web/dist
var buildFS embed.FS

//go:embed web/dist/index.html
var indexPage []byte

func main() {
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		os.Exit(jsplugin.RunCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	startTime := time.Now()
	kitutil.SetLogging(common.SysLog, func(message string) {
		logger.LogError(nil, message)
	})
	kitutil.SetSystemErrorLogging(common.SysError)

	err := InitResources()
	if err != nil {
		common.FatalLog("failed to initialize resources: " + err.Error())
		return
	}

	common.SysLog("New API " + common.Version + " started")
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	if common.DebugEnabled {
		common.SysLog("running in debug mode")
	}

	kitutil.Debug.Store(common.DebugEnabled)

	defer func() {
		err := model.CloseDB()
		if err != nil {
			common.FatalLog("failed to close database: " + err.Error())
		}
	}()

	if common.RedisEnabled {
		// for compatibility with old versions
		common.MemoryCacheEnabled = true
	}
	if common.MemoryCacheEnabled {
		common.SysLog("memory cache enabled")
		common.SysLog(fmt.Sprintf("sync frequency: %d seconds", common.SyncFrequency))

		// Add panic recovery and retry for InitChannelCache
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v, retrying once", r))
					// Retry once
					_, _, fixErr := model.FixAbility()
					if fixErr != nil {
						common.FatalLog(fmt.Sprintf("InitChannelCache failed: %s", fixErr.Error()))
					}
				}
			}()
			model.InitChannelCache()
		}()

		go model.SyncChannelCache(common.SyncFrequency)
	}
	wsmanager.StartSubscriber(context.Background())

	// Warm pricing after channel cache initialization so Advanced Custom
	// endpoint inference can read cached route settings on first request.
	model.GetPricing()

	// 热更新配置
	go model.SyncOptions(common.SyncFrequency)
	go controller.SyncTaskPlugins()

	// 周期性重载授权策略，保证多节点/多 master 部署下权限变更能传播到每个实例
	go authz.StartPolicySync(common.SyncFrequency)

	// 数据看板
	go model.UpdateQuotaData()

	if os.Getenv("CHANNEL_UPDATE_FREQUENCY") != "" {
		frequency, err := strconv.Atoi(os.Getenv("CHANNEL_UPDATE_FREQUENCY"))
		if err != nil {
			common.FatalLog("failed to parse CHANNEL_UPDATE_FREQUENCY: " + err.Error())
		}
		go controller.AutomaticallyUpdateChannels(frequency)
	}

	// Codex credential auto-refresh check every 10 minutes, refresh when expires within 1 day
	service.StartCodexCredentialAutoRefreshTask()

	// Subscription quota reset task (daily/weekly/monthly/custom)
	service.StartSubscriptionQuotaResetTask()

	// Report this process as a system instance so the System Info page can show
	// all currently alive nodes in multi-instance deployments.
	service.StartSystemInstanceReporter()

	// Wire task polling adaptor factory (breaks service -> relay import cycle).
	// Must run before the system task runner starts: the async_task_poll handler
	// calls service.RunTaskPollingOnce, which needs this factory set.
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		a := relay.GetTaskAdaptor(platform)
		if a == nil {
			return nil
		}
		return a
	}

	// Register the periodic channel test, upstream model update, and async task
	// polling (Midjourney / Suno / video) jobs as scheduled system tasks
	// (DB-lease dedup across masters + run history), then start the runner that
	// schedules and executes them. Master-only execution and the UpdateTask
	// switch are enforced inside the runner and each handler's Enabled().
	controller.RegisterScheduledSystemTasks()
	service.StartSystemTaskRunner()

	if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
		common.BatchUpdateEnabled = true
		common.SysLog("batch update enabled with interval " + strconv.Itoa(common.BatchUpdateInterval) + "s")
		model.InitBatchUpdater()
	}

	if os.Getenv("ENABLE_PPROF") == "true" {
		gopool.Go(func() {
			log.Println(http.ListenAndServe("0.0.0.0:8005", nil))
		})
		go common.Monitor()
		common.SysLog("pprof enabled")
	}

	err = common.StartPyroScope()
	if err != nil {
		common.SysError(fmt.Sprintf("start pyroscope error : %v", err))
	}

	// Initialize HTTP server
	server := gin.New()
	if err := middleware.ConfigureTrustedProxies(server); err != nil {
		common.FatalLog("failed to configure trusted proxies: " + err.Error())
		return
	}
	server.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		common.SysLog(fmt.Sprintf("panic detected: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Panic detected, error: %v. Please submit a issue here: https://github.com/Calcium-Ion/new-api", err),
				"type":    "new_api_panic",
			},
		})
	}))
	// This will cause SSE not to work!!!
	//server.Use(gzip.Gzip(gzip.DefaultCompression))
	server.Use(middleware.RequestId())
	server.Use(middleware.Version())
	server.Use(middleware.I18n())
	middleware.SetUpLogger(server)
	InjectUmamiAnalytics()
	InjectGoogleAnalytics()
	InjectShareMeta()

	// 设置路由
	router.SetRouter(server, router.WebAssets{
		BuildFS:   buildFS,
		IndexPage: indexPage,
	})
	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: server,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			common.FatalLog("failed to start HTTP server: " + err.Error())
		}
	}()

	time.Sleep(100 * time.Millisecond)

	common.LogStartupSuccess(startTime, port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	common.SysLog(fmt.Sprintf("received signal: %v, shutting down...", sig))

	// SSE streams may run for minutes; give them time to finish before forced exit
	shutdownTimeout := time.Duration(common.GetEnvOrDefault("SHUTDOWN_TIMEOUT_SECONDS", 120)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		common.SysError(fmt.Sprintf("server forced to shutdown: %v", err))
	}
	// 内存中的看板数据保存入库，避免重启丢失未落库数据 (issue #5679)
	if common.DataExportEnabled {
		model.SaveQuotaDataCache()
	}
	common.SysLog("server exited")
}

func InjectUmamiAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("UMAMI_WEBSITE_ID") != "" {
		umamiSiteID := os.Getenv("UMAMI_WEBSITE_ID")
		umamiScriptURL := os.Getenv("UMAMI_SCRIPT_URL")
		if umamiScriptURL == "" {
			umamiScriptURL = "https://analytics.umami.is/script.js"
		}
		analyticsInjectBuilder.WriteString("<script defer src=\"")
		analyticsInjectBuilder.WriteString(umamiScriptURL)
		analyticsInjectBuilder.WriteString("\" data-website-id=\"")
		analyticsInjectBuilder.WriteString(umamiSiteID)
		analyticsInjectBuilder.WriteString("\"></script>")
	}
	analyticsInjectBuilder.WriteString("<!--Umami QuantumNous-->\n")
	analyticsInject := []byte(analyticsInjectBuilder.String())
	placeholder := []byte("<!--umami-->\n")
	indexPage = bytes.ReplaceAll(indexPage, placeholder, analyticsInject)
}

func InjectGoogleAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("GOOGLE_ANALYTICS_ID") != "" {
		gaID := os.Getenv("GOOGLE_ANALYTICS_ID")
		// Google Analytics 4 (gtag.js)
		analyticsInjectBuilder.WriteString("<script async src=\"https://www.googletagmanager.com/gtag/js?id=")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("\"></script>")
		analyticsInjectBuilder.WriteString("<script>")
		analyticsInjectBuilder.WriteString("window.dataLayer = window.dataLayer || [];")
		analyticsInjectBuilder.WriteString("function gtag(){dataLayer.push(arguments);}")
		analyticsInjectBuilder.WriteString("gtag('js', new Date());")
		analyticsInjectBuilder.WriteString("gtag('config', '")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("');")
		analyticsInjectBuilder.WriteString("</script>")
	}
	analyticsInjectBuilder.WriteString("<!--Google Analytics QuantumNous-->\n")
	analyticsInject := []byte(analyticsInjectBuilder.String())
	placeholder := []byte("<!--Google Analytics-->\n")
	indexPage = bytes.ReplaceAll(indexPage, placeholder, analyticsInject)
}

// shareMetaSettings 是一次分享元标签渲染所需的配置快照。
type shareMetaSettings struct {
	SiteName      string
	ServerAddress string
	Logo          string
	Description   string
}

// InjectShareMeta 在启动时向 index.html 的 <!--share-meta--> 占位符注入
// Open Graph / Twitter Card 元标签,内容取自后台「SystemName / Logo / ServerAddress」
// 设置。手机端(微信、iOS、Telegram 等)生成分享预览卡片时读取的是原始 HTML head,
// 不执行前端 JS —— 没有这些标签只能回退到默认标题「New API」与默认 /logo.png。
// 仅启动时注入一次:在后台改 SystemName / Logo 后需重启本服务才生效。
func InjectShareMeta() {
	// SystemName / Logo / ServerAddress 由 model.updateOptionMap 在同一把写锁内一起更新,
	// 这里用一把读锁一次性取出,避免读到相互不一致的三元组。
	common.OptionMapRWMutex.RLock()
	cfg := shareMetaSettings{
		SiteName:      common.OptionMap["SystemName"],
		ServerAddress: common.OptionMap["ServerAddress"],
		Logo:          common.OptionMap["Logo"],
	}
	common.OptionMapRWMutex.RUnlock()
	cfg.Description = os.Getenv("SHARE_DESCRIPTION")

	injected, err := renderShareMeta(indexPage, cfg)
	if err != nil {
		common.SysError(fmt.Sprintf("inject share meta failed: %v", err))
		return
	}
	indexPage = injected
}

// renderShareMeta 把后台配置渲染成 Open Graph / Twitter Card 标签并注入 page。
// page 为完整的 index.html:优先替换 <!--share-meta--> 占位符,占位符缺失时退到
// 第一个 </head> 前插入。两者都不存在时返回错误,由调用方记录,避免注入静默失效。
func renderShareMeta(page []byte, cfg shareMetaSettings) ([]byte, error) {
	siteName := cfg.SiteName
	if siteName == "" {
		// 未配置系统名称时保持默认品牌,否则分享卡片标题会变成空白。
		siteName = "New API"
	}
	description := cfg.Description
	if description == "" {
		description = "Unified AI API gateway and admin dashboard."
	}
	baseURL := strings.TrimRight(cfg.ServerAddress, "/")

	// og:image 等标签会被微信 / Telegram / iOS 直接抓取,必须是绝对 http(s) URL。
	// 只有两种输入可用:绝对 http(s) URL,以及能拼上 ServerAddress 的根相对路径;
	// 其余(data:、协议相对 //host/...、裸域名等)一律跳过图片标签。
	logo := cfg.Logo
	if logo == "" {
		logo = "/logo.png"
	}
	var imageURL string
	lowerLogo := strings.ToLower(logo)
	switch {
	case strings.HasPrefix(lowerLogo, "http://"), strings.HasPrefix(lowerLogo, "https://"):
		imageURL = logo
	case strings.HasPrefix(logo, "/") && !strings.HasPrefix(logo, "//"):
		imageURL = baseURL + logo
	}
	if imageURL != "" {
		lowerImage := strings.ToLower(imageURL)
		if !strings.HasPrefix(lowerImage, "http://") && !strings.HasPrefix(lowerImage, "https://") {
			// ServerAddress 自身不是绝对地址,拼出来的图片 URL 同样不可用。
			imageURL = ""
		}
	}
	if imageURL == "" {
		common.SysError(fmt.Sprintf(
			"share meta: Logo %q 解析不出绝对 http(s) 图片地址,已跳过 og:image / twitter:image / apple-touch-icon;"+
				"请在后台配置 ServerAddress(与 Logo)后重启", logo))
	}

	b := &strings.Builder{}
	meta := func(name string, content string) {
		b.WriteString("<meta ")
		b.WriteString(name)
		b.WriteString(" content=\"")
		b.WriteString(html.EscapeString(content))
		b.WriteString("\" />\n")
	}
	lowerBase := strings.ToLower(baseURL)
	if strings.HasPrefix(lowerBase, "http://") || strings.HasPrefix(lowerBase, "https://") {
		meta("property=\"og:url\"", baseURL)
	}
	meta("property=\"og:type\"", "website")
	meta("property=\"og:site_name\"", siteName)
	meta("property=\"og:title\"", siteName)
	meta("property=\"og:description\"", description)
	if imageURL != "" {
		meta("property=\"og:image\"", imageURL)
		b.WriteString("<link rel=\"apple-touch-icon\" href=\"")
		b.WriteString(html.EscapeString(imageURL))
		b.WriteString("\" />\n")
		meta("name=\"twitter:card\"", "summary_large_image")
		meta("name=\"twitter:image\"", imageURL)
	}
	shareMeta := []byte(b.String())

	// 占位符匹配不依赖缩进:模板里是 4 空格缩进,前端构建可能改动首尾空白。
	placeholder := []byte("<!--share-meta-->\n")
	injected := bytes.Replace(page, placeholder, shareMeta, 1)
	if !bytes.Equal(injected, page) {
		common.SysLog(fmt.Sprintf("share meta injected at placeholder: site_name=%q image=%q url=%q",
			siteName, imageURL, baseURL))
		return injected, nil
	}

	// 占位符没被前端构建保留:退到在第一个 </head> 前插入(降级路径),避免注入静默失效
	// (否则分享卡片继续显示默认 New API 品牌,正是本注入要修的问题)。
	headEnd := []byte("</head>")
	fallback := bytes.Replace(page, headEnd, append(shareMeta, headEnd...), 1)
	if bytes.Equal(fallback, page) {
		return nil, fmt.Errorf("share meta: 占位符 %s 与 </head> 都不存在,无法注入", "<!--share-meta-->")
	}
	common.SysLog(fmt.Sprintf("share meta injected before </head> (placeholder missing, degraded path): site_name=%q image=%q url=%q",
		siteName, imageURL, baseURL))
	return fallback, nil
}

func InitResources() error {
	// Initialize resources here if needed
	// This is a placeholder function for future resource initialization
	err := godotenv.Load(".env")
	if err != nil {
		if common.DebugEnabled {
			common.SysLog("No .env file found, using default environment variables. If needed, please create a .env file and set the relevant variables.")
		}
	}

	// 加载环境变量
	common.InitEnv()

	logger.SetupLogger()

	// Initialize model settings
	ratio_setting.InitRatioSettings()

	service.InitHttpClient()

	service.InitTokenEncoders()

	// Initialize SQL Database
	err = model.InitDB()
	if err != nil {
		common.FatalLog("failed to initialize database: " + err.Error())
		return err
	}
	if err = authz.Init(model.DB); err != nil {
		common.FatalLog("failed to initialize authorization: " + err.Error())
		return err
	}
	if common.PasswordLoginEncryptionEnabled {
		if err = model.InitPasswordEncryption(); err != nil {
			common.FatalLog("failed to initialize password encryption: " + err.Error())
			return err
		}
	}

	model.CheckSetup()

	// Initialize options, should after model.InitDB()
	if common.IsMasterNode {
		if err := model.MigrateRetiredFrontendOptions(); err != nil {
			common.SysError("failed to migrate retired frontend options: " + err.Error())
		}
	}
	model.InitOptionMap()

	// 清理旧的磁盘缓存文件
	common.CleanupOldCacheFiles()

	// Initialize SQL Database
	err = model.InitLogDB()
	if err != nil {
		return err
	}

	// Initialize Redis
	err = common.InitRedisClient()
	if err != nil {
		return err
	}

	perfmetrics.Init()

	// 启动系统监控
	common.StartSystemMonitor()

	// Initialize i18n
	err = i18n.Init()
	if err != nil {
		common.SysError("failed to initialize i18n: " + err.Error())
		// Don't return error, i18n is not critical
	} else {
		common.SysLog("i18n initialized with languages: " + strings.Join(i18n.SupportedLanguages(), ", "))
	}
	// Register user language loader for lazy loading
	i18n.SetUserLangLoader(model.GetUserLanguage)

	// Load custom OAuth providers from database
	err = oauth.LoadCustomProviders()
	if err != nil {
		common.SysError("failed to load custom OAuth providers: " + err.Error())
		// Don't return error, custom OAuth is not critical
	}

	service.StartAuthArtifactCleanup()

	return nil
}
