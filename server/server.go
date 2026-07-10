// Package server is the single net/http listener that multiplexes /v1, /api, and
// the SPA by path. It is the only place that knows about HTTP.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/enowdev/enowx/core/mitm"
	"github.com/enowdev/enowx/core/plugins"
	"github.com/enowdev/enowx/core/provider"
	"github.com/enowdev/enowx/core/provider/custommgr"
	"github.com/enowdev/enowx/core/proxy"
	"github.com/enowdev/enowx/core/suno"
	syncpkg "github.com/enowdev/enowx/core/sync"
	"github.com/enowdev/enowx/core/transport"
	"github.com/enowdev/enowx/core/tunnel"
	"github.com/enowdev/enowx/server/handlers"
	"github.com/enowdev/enowx/server/middleware"
	"github.com/enowdev/enowx/store"
)

type Server struct {
	addr string
	mux  *chi.Mux
}

type Deps struct {
	Proxy      *proxy.Proxy
	Route      func(modelID string) string
	Registry   *provider.Registry
	Accounts   store.AccountStore
	Logs       store.LogStore
	Keys       store.KeyStore
	Warmups    store.WarmupStore
	Music      store.MusicStore
	SettingsKV store.SettingsStore
	Aliases    store.AliasStore
	Combos     store.ComboStore
	ApiTest    store.ApiTestStore
	Tunnel     *tunnel.Manager
	Plugins    *plugins.Manager
	MITM       *mitm.Manager
	Sync       *syncpkg.Manager
	CustomProv *custommgr.Manager
	Filters    store.FilterStore
	Proxies    store.ProxyStore
	Doer       transport.Doer
	Settings   handlers.SettingsInfo
}

func New(addr string, d Deps) *Server {
	r := chi.NewRouter()
	v1 := handlers.NewV1(d.Proxy, d.Route, d.Logs, d.Keys)
	anthropic := handlers.NewAnthropic(d.Proxy, d.Route, d.Logs, d.Keys)
	if d.Aliases != nil {
		resolver := proxy.NewAliasResolver(d.Aliases.Map, 30*time.Second)
		v1.SetAliasResolver(resolver)
		anthropic.SetAliasResolver(resolver)
	}
	if d.Combos != nil {
		comboResolver := proxy.NewComboResolver(d.Combos.Map, 30*time.Second)
		v1.SetCombos(comboResolver, d.Combos)
		anthropic.SetCombos(comboResolver, d.Combos)
	}
	providers := handlers.NewProviders(d.Registry)
	accounts := handlers.NewAccounts(d.Accounts)
	requests := handlers.NewRequests(d.Logs)
	keys := handlers.NewKeys(d.Keys)
	settings := handlers.NewSettings(d.Settings)
	dbg := handlers.NewDebug(d.Settings.Version, d.Settings.Started)
	docs := handlers.NewDocs(d.Settings.Version)
	kiro := handlers.NewKiro(d.Doer, d.Accounts)
	codex := handlers.NewCodex(d.Doer, d.Accounts)
	claude := handlers.NewClaude(d.Doer, d.Accounts)
	antigravity := handlers.NewAntigravity(d.Doer, d.Accounts)
	leonardoAcc := handlers.NewLeonardo(d.Accounts, d.Doer)
	local := handlers.NewLocal(d.Accounts)
	usage := handlers.NewUsage(d.Registry, d.Accounts)
	models := handlers.NewModels(d.Registry, d.Accounts, d.Sync, d.Combos)
	aliases := handlers.NewAliases(d.Aliases, d.Combos)
	combos := handlers.NewCombos(d.Combos, d.Aliases)
	apitest := handlers.NewApiTest(d.ApiTest)
	warmup := handlers.NewWarmup(d.Proxy, d.Registry, d.Accounts, d.Warmups, d.Logs)
	apply := handlers.NewApply(d.Accounts)
	// Auto-warm newly-added accounts (credit check + test request) before pool.
	accounts.SetWarmer(warmup)
	// Backfill generic labels with the account email when the provider can resolve
	// one (e.g. Claude accounts added before email resolution existed).
	accounts.SetEmailResolver(func(a store.Account) string {
		prov, err := d.Registry.Get(a.Provider)
		if err != nil {
			return ""
		}
		er, ok := prov.(provider.EmailReporter)
		if !ok {
			return ""
		}
		return er.Email(provider.Account{ID: a.ID, Secret: a.Secret, Creds: a.Creds})
	})
	// On delete, push the tombstone to the cloud right away so a background pull
	// can't resurrect the account.
	if d.Sync != nil {
		accounts.SetSyncPush(func() { _, _, _ = d.Sync.Sync(context.Background()) })
		accounts.SetDonate(d.Sync.DonateLocalAccount)
		// Keep the cloud's Free-AI key-hash registry in sync: on key add/remove,
		// and once at startup (covers free users who don't auto-sync).
		keys.SetOnChange(func() { d.Sync.RegisterKeyHashes(context.Background()) })
		go d.Sync.RegisterKeyHashes(context.Background())
	}
	kiro.SetWarmer(warmup)
	codex.SetWarmer(warmup)
	claude.SetWarmer(warmup)
	antigravity.SetWarmer(warmup)
	leonardoAcc.SetWarmer(warmup)
	local.SetWarmer(warmup)
	dash := middleware.NewDashboard(d.SettingsKV)
	versionH := handlers.NewVersion(d.Settings.Version, d.Doer, dash)
	term := handlers.NewTerminal(dash)
	files := handlers.NewFiles(dash)
	agent := handlers.NewAgent(dash, d.Doer)
	pluginsH := handlers.NewPlugins(dash, d.Plugins)
	market := handlers.NewMarket(dash, d.Sync, d.Plugins)
	customProv := handlers.NewCustomProviders(dash, d.CustomProv, d.Accounts)
	filters := handlers.NewFilters(dash, d.Filters, d.Sync)
	otp := handlers.NewOTP(dash, d.Sync)
	registryH := handlers.NewRegistry(dash, d.Sync)
	freeAI := handlers.NewFreeAI(dash, d.Sync)
	proxies := handlers.NewProxy(d.Proxies, d.SettingsKV)
	if d.Sync != nil {
		proxies.SetSyncPush(func() { _, _, _ = d.Sync.Sync(context.Background()) })
	}
	go proxies.RunAutoCheck(context.Background()) // periodic proxy health checks (opt-in via settings)
	music := handlers.NewMusic(d.Music)
	sunoMusic := handlers.NewSuno(d.Accounts, d.Proxy, suno.New(d.Doer))
	tun := handlers.NewTunnel(d.Tunnel, d.Keys)
	integ := handlers.NewIntegrations(d.Keys, d.Tunnel, d.Settings.Port)
	mitmH := handlers.NewMITM(d.MITM, dash.Authorized)
	syncH := handlers.NewSync(d.Sync)
	authH := handlers.NewAuth(dash)
	auth := middleware.NewAuth(d.Keys)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Proxy endpoints, guarded by the optional API key.
	r.Group(func(r chi.Router) {
		r.Use(auth.Handler)
		r.Get("/v1/models", models.V1Models) // OpenAI-standard model list for external clients
		r.Post("/v1/chat/completions", v1.ChatCompletions)
		r.Post("/v1/images/generations", v1.Images)
		r.Post("/anthropic/v1/messages", anthropic.Messages)
	})

	r.Route("/api", func(r chi.Router) {
		// Gate the whole /api surface: local (same-machine) requests pass; remote
		// requests (e.g. via the tunnel) require the dashboard session. The
		// login-bootstrap endpoints (auth status/setup/login) are whitelisted
		// inside Require so a first-time remote user can still sign in.
		r.Use(dash.Require)
		r.Get("/providers", providers.List)
		r.Get("/providers/{name}/rotation", proxies.GetRotation)
		r.Put("/providers/{name}/rotation", proxies.SetRotation)
		r.Get("/filters", filters.List)
		r.Post("/filters", filters.Add)
		r.Patch("/filters/{id}", filters.Update)
		r.Delete("/filters/{id}", filters.Delete)
		r.Get("/filter-templates", filters.ListTemplates)
		r.Post("/filter-templates", filters.SaveTemplate)
		r.Post("/filter-templates/{name}/load", filters.LoadTemplate)
		r.Delete("/filter-templates/{name}", filters.DeleteTemplate)
		r.Get("/community/filter-templates", filters.CommunityList)
		r.Post("/community/filter-templates/publish", filters.CommunityPublish)
		r.Post("/community/filter-templates/{id}/install", filters.CommunityInstall)
		r.Delete("/community/filter-templates/{id}", filters.CommunityDelete)
		r.Get("/custom-providers", customProv.List)
		r.Post("/custom-providers", customProv.Create)
		r.Post("/custom-providers/probe", customProv.Probe)
		r.Patch("/custom-providers/{id}", customProv.Update)
		r.Delete("/custom-providers/{id}", customProv.Delete)
		r.Get("/accounts", accounts.List)
		r.Post("/accounts", accounts.Add)
		r.Patch("/accounts/{id}/status", accounts.SetStatus)
		r.Patch("/accounts/{id}/disabled", accounts.SetDisabled)
		r.Get("/accounts/{id}/usage", usage.Get)
		r.Get("/accounts/{id}/models", models.Get)
		r.Get("/models", models.All)
		r.Get("/model-aliases", aliases.List)
		r.Post("/model-aliases", aliases.Set)
		r.Delete("/model-aliases/{alias}", aliases.Delete)
		r.Get("/model-combos", combos.List)
		r.Post("/model-combos", combos.Create)
		r.Put("/model-combos/{id}", combos.Update)
		r.Delete("/model-combos/{id}", combos.Delete)

		r.Get("/apitest", apitest.All)
		r.Post("/apitest/collections", apitest.AddCollection)
		r.Patch("/apitest/collections/{id}", apitest.RenameCollection)
		r.Delete("/apitest/collections/{id}", apitest.DeleteCollection)
		r.Post("/apitest/requests", apitest.SaveRequest)
		r.Delete("/apitest/requests/{id}", apitest.DeleteRequest)
		r.Post("/apitest/environments", apitest.SaveEnvironment)
		r.Delete("/apitest/environments/{id}", apitest.DeleteEnvironment)
		r.Post("/apitest/environments/{id}/activate", apitest.ActivateEnvironment)
		r.Post("/apitest/history", apitest.AddHistory)
		r.Delete("/apitest/history", apitest.ClearHistory)
		r.Post("/accounts/{id}/warmup", warmup.Run)
		r.Post("/accounts/{id}/test-model", warmup.TestModel)
		r.Post("/accounts/{id}/apply", apply.Apply)
		r.Get("/warmup-logs", warmup.List)
		r.Delete("/warmup-logs", warmup.Clear)
		r.Delete("/accounts/{id}", accounts.Delete)
		r.Post("/accounts/{id}/donate", accounts.Donate)
		r.Post("/accounts/donate-bulk", accounts.DonateBulk)
		r.Get("/requests", requests.List)
		r.Delete("/requests", requests.Clear)
		r.Get("/requests/summary", requests.Summary)
		r.Get("/requests/series", requests.Series)
		r.Get("/requests/top-models", requests.TopModels)
		r.Get("/keys", keys.List)
		r.Post("/keys", keys.Add)
		r.Delete("/keys/{id}", keys.Delete)

		// Integrations: connect local CLI coding tools to this gateway.
		r.Get("/integrations", integ.List)
		r.Get("/integrations/info", integ.Info)
		r.Post("/integrations/{tool}", integ.Apply)
		r.Delete("/integrations/{tool}", integ.Reset)
		r.Post("/integrations/{tool}/snippet", integ.Snippet)

		// MITM: intercept a proprietary IDE's endpoint and reroute it to the gateway.
		r.Get("/mitm", mitmH.Status)
		r.Post("/mitm/trust", mitmH.InstallTrust)
		r.Post("/mitm/start", mitmH.Start)
		r.Post("/mitm/stop", mitmH.Stop)
		r.Post("/mitm/{tool}/enable", mitmH.EnableTool)
		r.Put("/mitm/{tool}/aliases", mitmH.SetAliases)
		r.Get("/settings", settings.Get)
		r.Get("/version", versionH.Get)
		r.Post("/update", versionH.Update)
		r.Get("/debug", dbg.Get)
		r.Get("/docs", docs.Get)

		r.Post("/accounts/kiro/manual", kiro.Manual)
		r.Post("/accounts/kiro/refresh", kiro.Refresh)
		r.Post("/accounts/kiro/aws/start", kiro.AWSStart)
		r.Get("/accounts/kiro/aws/poll", kiro.AWSPoll)
		r.Post("/accounts/kiro/oauth/start", kiro.OAuthStart)
		r.Post("/accounts/kiro/oauth/exchange", kiro.OAuthExchange)
		r.Post("/accounts/claude/oauth/start", claude.OAuthStart)
		r.Post("/accounts/claude/oauth/exchange", claude.OAuthExchange)

		r.Post("/accounts/codex/oauth/start", codex.OAuthStart)
		r.Post("/accounts/codex/oauth/exchange", codex.OAuthExchange)
		r.Post("/accounts/codex/manual", codex.Manual)

		r.Post("/accounts/antigravity/oauth/start", antigravity.OAuthStart)
		r.Post("/accounts/antigravity/oauth/exchange", antigravity.OAuthExchange)
		r.Post("/accounts/antigravity/manual", antigravity.Manual)

		r.Post("/accounts/leonardo/cookie", leonardoAcc.FromCookie)
		r.Post("/accounts/leonardo/browser/start", leonardoAcc.BrowserStart)
		r.Post("/accounts/leonardo/browser/poll", leonardoAcc.BrowserPoll)
		r.Post("/accounts/leonardo/browser/cancel", leonardoAcc.BrowserCancel)

		r.Get("/local-sources", local.Scan)
		r.Post("/local-sources/import", local.Import)

		r.Get("/files", files.List)
		r.Get("/files/watch", files.Watch)

		// Terminal profiles (per-terminal credential isolation via HOME).
		termProfiles := handlers.NewTermProfiles(dash)
		r.Get("/term-profiles", termProfiles.List)
		r.Post("/term-profiles", termProfiles.Create)
		r.Delete("/term-profiles/{slug}", termProfiles.Delete)
		r.Get("/files/read", files.Read)
		r.Get("/files/raw", files.Raw)

		// OTP (Warpize SMS): everything under /api/otp/* forwards to the cloud.
		r.HandleFunc("/otp/*", otp.Proxy)

		// Community MCP & Skill registry.
		r.Get("/registry", registryH.List)
		r.Get("/registry/{id}", registryH.Get)
		r.Post("/registry/publish", registryH.Publish)

		// Free AI — account donation + available models.
		r.Get("/ai/info", freeAI.Info)
		r.Get("/ai/models", freeAI.Models)
		r.Post("/free-ai/donate", freeAI.Donate)
		r.Get("/free-ai/donations", freeAI.List)
		r.Delete("/free-ai/donations/{id}", freeAI.Withdraw)

		// Outbound proxy pool.
		r.Get("/proxies", proxies.List)
		r.Post("/proxies", proxies.Add)
		r.Delete("/proxies/{id}", proxies.Delete)
		r.Patch("/proxies/{id}/enabled", proxies.Toggle)
		r.Post("/proxies/{id}/test", proxies.Test)
		r.Get("/proxies/settings", proxies.GetSettings)
		r.Put("/proxies/settings", proxies.SaveSettings)

		r.Post("/agent/fs/read", agent.FSRead)
		r.Post("/agent/fs/list", agent.FSList)
		r.Post("/agent/fs/write", agent.FSWrite)
		r.Post("/agent/fs/edit", agent.FSEdit)
		r.Post("/agent/exec", agent.Exec)
		r.Post("/agent/http", agent.HTTP)

		r.Get("/auth/status", authH.Status)
		r.Post("/auth/setup", authH.Setup)
		r.Post("/auth/login", authH.Login)
		r.Post("/auth/logout", authH.Logout)
		r.Post("/auth/change", authH.Change)

		r.Get("/sync/status", syncH.Status)
		r.Post("/sync/login", syncH.LoginStart)
		r.Get("/sync/login/poll", syncH.LoginPoll)
		r.Post("/sync/logout", syncH.Logout)
		r.Post("/sync/now", syncH.Now)
		r.Post("/sync/auto", syncH.SetAuto)
		r.Patch("/profile", syncH.UpdateProfile)
		r.Post("/profile/avatar", syncH.UploadAvatar)
		r.Post("/profile/banner", syncH.UploadBanner)
		r.Post("/upload/image", syncH.UploadImage)
		r.Get("/users/{id}/profile", syncH.PublicProfile)
		r.Get("/users/by-name/{name}", syncH.UserByName)
		r.Get("/users/mention", syncH.MentionUsers)
		r.Get("/users/{id}/posts", syncH.UserPosts)
		r.Get("/posts", syncH.PostsList)
		r.Post("/posts", syncH.PostCreate)
		r.Patch("/posts/{id}", syncH.PostEdit)
		r.Delete("/posts/{id}", syncH.PostDelete)
		r.Post("/posts/{id}/upvote", syncH.PostUpvote)
		r.Post("/posts/{id}/reactions", syncH.PostReact)
		r.Get("/posts/{id}/comments", syncH.PostComments)
		r.Post("/posts/{id}/comments", syncH.CommentAdd)
		r.Patch("/comments/{id}", syncH.CommentEdit)
		r.Delete("/comments/{id}", syncH.CommentDelete)
		r.Post("/comments/{id}/reactions", syncH.CommentReact)

		r.Get("/subscription", syncH.Subscription)
		r.Post("/subscription/subscribe", syncH.Subscribe)
		r.Post("/subscription/validate-coupon", syncH.ValidateCoupon)
		r.Post("/subscription/redeem", syncH.Redeem)
		r.Get("/subscription/order/{ref}", syncH.SubscriptionOrder)
		// Gmail store (proxied to cloud).
		r.Get("/store/gmail", syncH.GmailStoreInfo)
		r.Post("/store/gmail/order", syncH.GmailBuy)
		r.Get("/store/gmail/order/{ref}", syncH.GmailOrderStatus)
		r.Get("/store/gmail/order/{ref}/accounts", syncH.GmailOrderAccounts)
		r.Get("/admin/gmail/stock", syncH.AdminGmailStock)
		r.Post("/admin/gmail/stock", syncH.AdminGmailAddStock)
		r.Delete("/admin/gmail/stock/{id}", syncH.AdminGmailDeleteStock)
		r.Get("/admin/gmail/orders", syncH.AdminGmailOrders)
		r.Put("/admin/gmail/price", syncH.AdminGmailSetPrice)
		r.Post("/subscription/gift", syncH.GiftPremium)
		r.Get("/search-users", syncH.SearchUsers)
		r.Get("/inbox", syncH.Inbox)
		r.Post("/inbox/read", syncH.InboxRead)
		r.Post("/bug-reports", syncH.ReportBug)
		r.Get("/admin/bug-reports", syncH.BugReports)
		r.Post("/admin/bug-reports/{id}/resolve", syncH.ResolveBug)
		r.Post("/admin/bug-reports/{id}/reopen", syncH.ReopenBug)
		r.Delete("/admin/bug-reports/{id}", syncH.DeleteBug)
		r.Get("/admin/inbox", syncH.AdminInboxList)
		r.Post("/admin/inbox", syncH.SendInbox)
		r.Delete("/admin/inbox/{id}", syncH.DeleteInbox)
		r.Get("/admin/inbox/roles", syncH.InboxRoles)
		r.Get("/admin/coupons", syncH.AdminCoupons)
		r.Post("/admin/coupons", syncH.CreateCoupon)
		r.Delete("/admin/coupons/{id}", syncH.DeleteCoupon)
		r.Get("/admin/redeem-codes", syncH.AdminRedeemCodes)
		r.Post("/admin/redeem-codes", syncH.CreateRedeemCode)
		r.Delete("/admin/redeem-codes/{id}", syncH.DeleteRedeemCode)
		r.Get("/marketplace/listings", syncH.MarketplaceList)
		r.Get("/marketplace/my-listings", syncH.MarketplaceMine)
		r.Get("/marketplace/listings/{id}", syncH.MarketplaceGet)
		r.Post("/marketplace/listings", syncH.MarketplaceCreate)
		r.Patch("/marketplace/listings/{id}", syncH.MarketplaceUpdate)
		r.Delete("/marketplace/listings/{id}", syncH.MarketplaceDelete)
		r.Get("/marketplace/rekber/fee", syncH.RekberFee)
		r.Get("/marketplace/rekber/threads", syncH.RekberThreads)
		r.Post("/marketplace/rekber/threads", syncH.RekberCreate)
		r.Get("/marketplace/rekber/threads/{id}", syncH.RekberGetThread)
		r.Post("/marketplace/rekber/threads/{id}/messages", syncH.RekberSend)
		r.Get("/marketplace/rekber/threads/{id}/delivery", syncH.RekberDelivery)
		r.Get("/marketplace/rekber/orders", syncH.RekberOrders)
		r.Post("/marketplace/rekber/threads/{id}/review", syncH.RekberReview)
		r.Get("/marketplace/sellers/{id}/reviews", syncH.SellerReviews)
		r.Post("/marketplace/rekber/threads/{id}/{action}", syncH.RekberAction)
		r.Get("/marketplace/admin/rekber/account", syncH.RekberAccountGet)
		r.Put("/marketplace/admin/rekber/account", syncH.RekberAccountSet)
		r.Get("/marketplace/payout", syncH.PayoutGet)
		r.Put("/marketplace/payout", syncH.PayoutSet)
		r.Get("/marketplace/official", syncH.OfficialList)
		r.Post("/marketplace/orders", syncH.OrderCreate)
		r.Get("/marketplace/orders", syncH.OrdersList)
		r.Get("/marketplace/orders/{id}", syncH.OrderGet)
		r.Get("/marketplace/admin/vip/balance", syncH.VIPBalance)
		r.Get("/marketplace/admin/vip/catalog", syncH.VIPCatalog)
		r.Get("/marketplace/admin/vip/products", syncH.VIPProducts)
		r.Post("/marketplace/admin/vip/products", syncH.VIPProductUpsert)
		r.Patch("/marketplace/admin/vip/products/{id}", syncH.VIPProductToggle)
		r.Delete("/marketplace/admin/vip/products/{id}", syncH.VIPProductDelete)

		r.Get("/shop", syncH.Shop)
		r.Post("/shop/buy", syncH.ShopBuy)
		r.Post("/shop/equip", syncH.ShopEquip)
		r.Get("/admin/flags", syncH.AdminFlags)
		r.Post("/admin/flags/{id}/review", syncH.AdminReviewFlag)
		r.Get("/admin/log", syncH.AdminLog)
		r.Get("/admin/stats", syncH.AdminStats)
		r.Get("/admin/users", syncH.AdminUsers)
		r.Get("/admin/users/{id}/detail", syncH.AdminUserDetail)
		r.Get("/admin/models", syncH.AdminModels)
		r.Post("/admin/models", syncH.AdminUpsertModel)
		r.Patch("/admin/models/{id}", syncH.AdminUpdateModel)
		r.Delete("/admin/models/{id}", syncH.AdminDeleteModel)
		r.Post("/admin/users/{id}/{action}", syncH.AdminUserAction)
		r.Get("/chat/messages", syncH.ChatList)
		r.Post("/chat/messages", syncH.ChatSend)
		r.Post("/chat/share-music", syncH.ChatShareMusic)
		r.Patch("/chat/messages/{id}", syncH.ChatEdit)
		r.Delete("/chat/messages/{id}", syncH.ChatDelete)
		r.Post("/chat/messages/{id}/reactions", syncH.ChatReact)
		r.Get("/chat/stream", syncH.ChatStream)
		r.Get("/search", syncH.Search)
		r.Get("/notifications", syncH.Notifications)
		r.Get("/community/stats", syncH.CommunityStats)
		r.Get("/kleos/daily", syncH.KleosDailyStatus)
		r.Post("/kleos/daily", syncH.KleosDaily)
		r.Get("/legacy/accounts", syncH.LegacyAccounts)
		r.Post("/notifications/read", syncH.NotificationsRead)

		r.Get("/tunnel/status", tun.Status)
		r.Post("/tunnel/enable", tun.Enable)
		r.Post("/tunnel/disable", tun.Disable)
		r.Post("/tunnel/login", tun.Login)
		r.Post("/tunnel/named", tun.Named)

		r.Get("/music/search", music.Search)
		r.Get("/music/stream", music.Stream)
		r.Get("/music/suno/key", sunoMusic.GetKey)
		r.Post("/music/generate", sunoMusic.Generate)
		r.Get("/music/generate/status", sunoMusic.Status)
		r.Get("/music/discover", music.Discover)
		r.Get("/music/history", music.RecentPlays)
		r.Post("/music/history", music.RecordPlay)
		r.Delete("/music/history", music.ClearHistory)
		r.Get("/music/playlists", music.ListPlaylists)
		r.Post("/music/playlists", music.CreatePlaylist)
		r.Post("/music/playlists/import", music.ImportPlaylist)
		r.Get("/music/playlists/{id}", music.GetPlaylist)
		r.Delete("/music/playlists/{id}", music.DeletePlaylist)
		r.Get("/music/playlists/{id}/export", music.ExportPlaylist)
		r.Post("/music/playlists/{id}/tracks", music.AddTrack)
		r.Delete("/music/playlists/{id}/tracks/{videoId}", music.RemoveTrack)
	})

	// Privileged surfaces registered outside the /api Route block above still get
	// the same dashboard gate (local passes; remote needs a session).
	r.Group(func(r chi.Router) {
		r.Use(dash.Require)

		// Real PTY shell over WebSocket.
		r.Get("/api/terminal", term.WS)

		// Plugin management.
		r.Route("/api/plugins", func(r chi.Router) {
			r.Get("/", pluginsH.List)
			r.Post("/", pluginsH.Create)
			r.Post("/{id}/start", pluginsH.Start)
			r.Post("/{id}/stop", pluginsH.Stop)
			r.Post("/{id}/reveal", pluginsH.Reveal)
			r.Get("/{id}/icon", pluginsH.Icon)
			r.Post("/{id}/icon", pluginsH.UploadIcon)
			r.Get("/{id}/logs", pluginsH.Logs)
			r.Delete("/{id}", pluginsH.Delete)
		})
		// Plugin marketplace (publish/browse/install) + admin scan settings.
		r.Route("/api/market", func(r chi.Router) {
			r.Post("/publish", market.Publish)
			r.Get("/plugins", market.List)
			r.Post("/install/{id}", market.Install)
			r.Post("/update/{id}", market.Update)
		})
		r.Get("/api/admin/plugin-scan", market.GetScanSettings)
		r.Put("/api/admin/plugin-scan", market.SaveScanSettings)
		r.Get("/api/admin/plugin-reviews", market.Reviews)
		r.Get("/api/admin/plugin-reviews/{id}", market.ReviewDetail)
		r.Get("/api/admin/marketplace", market.AdminPlugins)
		r.Get("/api/admin/marketplace/{id}/source", market.PluginSource)
		r.Post("/api/admin/marketplace/{id}/{action}", market.SetStatus)
		r.Delete("/api/admin/marketplace/{id}", market.Takedown)

		// Plugin UIs, reverse-proxied to their sidecar.
		r.Handle("/plugins/*", pluginsH.PluginProxy())
	})

	// WebOS SPA on the same port (everything not matched above).
	r.Handle("/*", spaHandler())

	return &Server{addr: addr, mux: r}
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.addr, s.mux)
}
