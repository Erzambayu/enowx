// Package sync mirrors local enowx state to the enowxlabs cloud server using
// the item/LWW protocol (see ~/V2/SYNC.md). It is the client half: it snapshots
// local data into sync items, pushes the locally-newer ones, pulls the
// server-newer ones, and applies them back. The pilot data type is playlists.
package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	stdsync "sync"
	"time"

	"github.com/enowdev/enowx/store"
)

// DefaultServerURL is the built-in enowx cloud endpoint (production). Users don't
// configure this; an override can still be stored via SetServer for development
// (e.g. pointing at the api-dev staging cloud).
const DefaultServerURL = "https://api.enowxlabs.com"

// settings keys (in the gateway's settings KV)
const (
	keyServerURL = "sync_server_url"
	keyToken     = "sync_token"
	keyEnabled   = "sync_enabled" // logged in (token present, not revoked)
	keyAuto      = "sync_auto"    // user's global automatic-sync toggle
	keyCursor    = "sync_cursor"  // last pull watermark (unix millis)
	keyUser      = "sync_user"    // cached /me JSON (identity + plan)
	keyDevice    = "sync_device"  // stable per-device id (for usage watermark)
)

// Manager owns the client-side sync state and the HTTP calls to enowxlabs.
type Manager struct {
	settings store.SettingsStore
	music    store.MusicStore
	logs     store.LogStore
	http     *http.Client

	// Optional stores for full sync (accounts/keys/aliases/custom providers).
	// Set via SetFullSync; nil disables the corresponding type.
	accounts store.AccountStore
	keys     store.KeyStore
	aliases  store.AliasStore
	combos   store.ComboStore
	custom   store.CustomProviderStore
	proxies  store.ProxyStore
	// onCustomProvider re-registers a pulled custom provider live (custommgr).
	onCustomProvider func(store.CustomProvider)
	onCustomDelete   func(prefix, name string)

	subsMu stdsync.Mutex
	subs   map[int]chan LiveEvent
	nextID int
}

// SetFullSync wires the extra local stores (and a live custom-provider register
// hook) so the manager can snapshot/apply accounts, keys, aliases, and custom
// providers. Called once at startup after the stores + custommgr exist.
func (m *Manager) SetFullSync(a store.AccountStore, k store.KeyStore, al store.AliasStore, cb store.ComboStore, cp store.CustomProviderStore, px store.ProxyStore, onCP func(store.CustomProvider), onDel func(prefix, name string)) {
	m.accounts, m.keys, m.aliases, m.combos, m.custom, m.proxies = a, k, al, cb, cp, px
	m.onCustomProvider, m.onCustomDelete = onCP, onDel
}

// LiveEvent is a cloud event relayed to UI subscribers (e.g. chat messages).
type LiveEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func New(settings store.SettingsStore, music store.MusicStore, logs store.LogStore) *Manager {
	return &Manager{
		settings: settings, music: music, logs: logs,
		// 60s (was 30): a first full sync of a large account pool can take a while
		// over a remote DB; the server batches the upsert in one tx, but give the
		// client headroom so a big initial push doesn't time out.
		http: &http.Client{Timeout: 60 * time.Second},
		subs: map[int]chan LiveEvent{},
	}
}

// Subscribe registers a channel that receives relayed cloud live events until
// the returned cancel func is called.
func (m *Manager) Subscribe() (<-chan LiveEvent, func()) {
	ch := make(chan LiveEvent, 16)
	m.subsMu.Lock()
	id := m.nextID
	m.nextID++
	m.subs[id] = ch
	m.subsMu.Unlock()
	return ch, func() {
		m.subsMu.Lock()
		delete(m.subs, id)
		close(ch)
		m.subsMu.Unlock()
	}
}

// publish fans a live event out to all subscribers (non-blocking).
func (m *Manager) publish(ev liveEvent) {
	out := LiveEvent{Event: ev.Event, Data: ev.Data}
	m.subsMu.Lock()
	for _, ch := range m.subs {
		select {
		case ch <- out:
		default: // drop if a slow subscriber is full
		}
	}
	m.subsMu.Unlock()
}

// deviceID returns a stable id for this device, generating one on first use.
// Used so the cloud usage-watermark advances per device, not globally.
func (m *Manager) deviceID(ctx context.Context) string {
	if id := m.get(ctx, keyDevice); id != "" {
		return id
	}
	id := randHex(8)
	_ = m.settings.Set(ctx, keyDevice, id)
	return id
}

func (m *Manager) get(ctx context.Context, key string) string {
	v, _ := m.settings.Get(ctx, key)
	return v
}

// Configured reports whether a token is set (the server URL is built-in).
func (m *Manager) Configured(ctx context.Context) bool {
	return m.get(ctx, keyToken) != ""
}

func (m *Manager) Enabled(ctx context.Context) bool {
	return m.get(ctx, keyEnabled) == "1" && m.Configured(ctx)
}

// AutoEnabled reports the user's global automatic-sync toggle. Defaults to OFF
// (only an explicit "1" turns it on) — full sync is a Premium perk, so it's
// auto-enabled when the user becomes Premium (see EnsurePremiumSync). Requires
// being logged in.
func (m *Manager) AutoEnabled(ctx context.Context) bool {
	return m.get(ctx, keyAuto) == "1" && m.Enabled(ctx)
}

// EnsurePremiumSync auto-enables auto-sync the first time a user is seen as
// Premium (when the toggle was never set). Called after applying /me.
func (m *Manager) EnsurePremiumSync(ctx context.Context, premium bool) {
	if premium && m.get(ctx, keyAuto) == "" {
		_ = m.settings.Set(ctx, keyAuto, "1")
	}
}

// SetAuto flips the global automatic-sync toggle.
func (m *Manager) SetAuto(ctx context.Context, on bool) error {
	v := "1"
	if !on {
		v = "0"
	}
	return m.settings.Set(ctx, keyAuto, v)
}

// SetServer stores the cloud base URL (e.g. https://labs.enowxlabs.com).
func (m *Manager) SetServer(ctx context.Context, url string) error {
	return m.settings.Set(ctx, keyServerURL, strings.TrimRight(url, "/"))
}

// SetToken stores the sync token issued after Discord login (and caches /me).
func (m *Manager) SetToken(ctx context.Context, token, userJSON string) error {
	if err := m.settings.Set(ctx, keyToken, token); err != nil {
		return err
	}
	if userJSON != "" {
		_ = m.settings.Set(ctx, keyUser, userJSON)
	}
	return m.settings.Set(ctx, keyEnabled, "1")
}

func (m *Manager) Logout(ctx context.Context) error {
	_ = m.settings.Set(ctx, keyToken, "")
	_ = m.settings.Set(ctx, keyUser, "")
	return m.settings.Set(ctx, keyEnabled, "0")
}

// ServerURL returns the built-in default, overridable only via the
// ENOWX_SYNC_SERVER env var (for development). A previously-persisted value is
// intentionally ignored so the endpoint can't get stuck on a stale URL.
func (m *Manager) ServerURL(_ context.Context) string {
	if v := os.Getenv("ENOWX_SYNC_SERVER"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultServerURL
}
func (m *Manager) UserJSON(ctx context.Context) string { return m.get(ctx, keyUser) }

// --- Discord login (device-code style against enowxlabs) ---

// LoginStart asks the server for a Discord authorize URL. The caller opens it;
// the user authorizes; then LoginPoll retrieves the token. The server URL is
// built in (see ServerURL); the argument is ignored.
func (m *Manager) LoginStart(ctx context.Context, _ string) (authorizeURL, state string, err error) {
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	// /auth is public (no token yet). Send our stable device id so the server
	// can use it as an anti-fraud (shared-device) signal.
	if err := m.callNoAuth(ctx, http.MethodPost, "/auth/discord/start", map[string]any{"device": m.deviceID(ctx)}, &resp); err != nil {
		return "", "", err
	}
	return resp.AuthorizeURL, resp.State, nil
}

// LoginPoll checks whether the browser flow finished; on success it stores the
// token and returns the cached user JSON.
func (m *Manager) LoginPoll(ctx context.Context, state string) (done bool, userJSON string, err error) {
	var resp struct {
		Status    string          `json:"status"`
		SyncToken string          `json:"sync_token"`
		User      json.RawMessage `json:"user"`
	}
	if err := m.callNoAuth(ctx, http.MethodGet, "/auth/discord/poll?state="+state, nil, &resp); err != nil {
		return false, "", err
	}
	if resp.Status != "done" {
		return false, "", nil
	}
	if err := m.SetToken(ctx, resp.SyncToken, string(resp.User)); err != nil {
		return true, "", err
	}
	return true, string(resp.User), nil
}

// Me refreshes identity + roles + plan from the server and caches it.
func (m *Manager) Me(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/me", nil, &raw); err != nil {
		return "", err
	}
	_ = m.settings.Set(ctx, keyUser, string(raw))
	// Auto-enable auto-sync the first time we see the user as Premium.
	m.EnsurePremiumSync(ctx, m.hasFullSync(ctx))
	return string(raw), nil
}

// UpdateProfile edits the user's own profile fields on the server, refreshes the
// cached /me, and returns the updated user JSON.
func (m *Manager) UpdateProfile(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPatch, "/me/profile", body, &raw); err != nil {
		return "", err
	}
	// Refresh the cached identity so the UI reflects the edit immediately.
	if me, err := m.Me(ctx); err == nil {
		return me, nil
	}
	return string(raw), nil
}

// PublicProfile fetches another user's public profile by id.
func (m *Manager) PublicProfile(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/users/"+id+"/profile", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// UserByName resolves a username to a user id (for @mention profile links).
func (m *Manager) UserByName(ctx context.Context, name string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/users/by-name/"+url.PathEscape(name), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// CatalogModel is a DB-catalog model entry from the cloud (for non-fetchable
// providers).
type CatalogModel struct {
	ModelID   string `json:"model_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	OwnedBy   string `json:"owned_by"`
	MaxInput  int    `json:"max_input"`
	MaxOutput int    `json:"max_output"`
}

// rawOrNil returns body as a request payload, or nil if empty (so an empty body
// isn't marshaled to "null").
func rawOrNil(body json.RawMessage) any {
	if len(body) == 0 {
		return nil
	}
	return body
}

// AdminModels fetches the full model catalog for a provider (admin editing).
func (m *Manager) AdminModels(ctx context.Context, providerName string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/models?provider="+url.QueryEscape(providerName), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminUpsertModel creates/updates a catalog entry.
func (m *Manager) AdminUpsertModel(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/models", rawOrNil(body), &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminUpdateModel edits a catalog entry by id.
func (m *Manager) AdminUpdateModel(ctx context.Context, id string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPatch, "/admin/models/"+id, rawOrNil(body), &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminDeleteModel removes a catalog entry by id.
func (m *Manager) AdminDeleteModel(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodDelete, "/admin/models/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ProviderModels asks the cloud for a provider's DB-managed model catalog
// (best-effort; empty on any error, e.g. offline).
func (m *Manager) ProviderModels(ctx context.Context, providerName string) []CatalogModel {
	var out struct {
		Models []CatalogModel `json:"models"`
	}
	if err := m.call(ctx, http.MethodGet, "/models?provider="+url.QueryEscape(providerName), nil, &out); err != nil {
		return []CatalogModel{}
	}
	if out.Models == nil {
		return []CatalogModel{}
	}
	return out.Models
}

// MentionUsers returns @mention autocomplete candidates (empty q = default list).
func (m *Manager) MentionUsers(ctx context.Context, q string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/users/mention?q="+url.QueryEscape(q), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// UploadMedia forwards a raw multipart body (avatar/banner) to the cloud with
// the given content type, returning the JSON response.
func (m *Manager) UploadMedia(ctx context.Context, path, contentType string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ServerURL(ctx)+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	req.Header.Set("Content-Type", contentType)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("upload failed (%d): %s", resp.StatusCode, string(raw))
	}
	return string(raw), nil
}

// UserPosts fetches a user's posts (for their profile page).
func (m *Manager) UserPosts(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/users/"+id+"/posts", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PostsList fetches a page of the community feed (query appended).
func (m *Manager) PostsList(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/posts"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- plugin marketplace ---

// PublishPlugin uploads a plugin bundle (zip) + metadata; the cloud scans it and
// returns {status:"approved"|"rejected", reason, id}.
func (m *Manager) PublishPlugin(ctx context.Context, fields map[string]string, zipBytes []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", "bundle.zip")
	_, _ = fw.Write(zipBytes)
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ServerURL(ctx)+"/plugins", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("publish failed (%d): %s", resp.StatusCode, string(raw))
	}
	return string(raw), nil
}

// --- MCP & Skill registry ---

// RegistryPublish forwards a skill's JSON body ({kind,name,description,version,
// files[]}) to the cloud, which scans it and commits the files to enowX-Skill.
func (m *Manager) RegistryPublish(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ServerURL(ctx)+"/registry/publish", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", errors.New(strings.TrimSpace(string(raw)))
	}
	return string(raw), nil
}

// --- Free AI donation (proxy to cloud) ---

// FreeAIDonate forwards a donation (JSON body) to the cloud, which health-checks
// then stores it. Returns the raw JSON response.
func (m *Manager) FreeAIDonate(ctx context.Context, body []byte) (string, error) {
	return m.rawPost(ctx, "/free-ai/donate", body)
}

// DonateLocalAccount hands a local account's provider + creds (+ models) to the
// cloud Free-AI pool. For native providers the cloud validates them; the caller
// deletes the local account on success.
func (m *Manager) DonateLocalAccount(ctx context.Context, provider string, creds map[string]string, models []string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"provider": provider,
		"label":    provider,
		"creds":    creds,
		"models":   models,
	})
	return m.rawPost(ctx, "/free-ai/donate", body)
}

// FreeAIDonations lists the caller's donated accounts.
func (m *Manager) FreeAIDonations(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/free-ai/donations", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RegisterKeyHashes sends non-secret sha256 hashes of the user's gateway API
// keys to the cloud so they can be used to authenticate Free-AI requests. Works
// for all users (ungated) — it's just hashes, not the key payload. Best-effort.
func (m *Manager) RegisterKeyHashes(ctx context.Context) {
	if m.keys == nil || !m.Enabled(ctx) {
		return
	}
	list, err := m.keys.List(ctx)
	if err != nil || len(list) == 0 {
		return
	}
	hashes := make([]string, 0, len(list))
	for _, k := range list {
		hashes = append(hashes, fullHash(k.Secret))
	}
	var out json.RawMessage
	_ = m.call(ctx, http.MethodPost, "/free-ai/register-keys", map[string]any{"hashes": hashes}, &out)
}

// FreeAIModels lists the models the pool can currently serve (with Kleos prices).
func (m *Manager) FreeAIModels(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/ai/models", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// FreeAIWithdraw removes one of the caller's donated accounts.
func (m *Manager) FreeAIWithdraw(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodDelete, "/free-ai/donations/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// rawPost sends a raw JSON body with the auth token and returns the raw response.
func (m *Manager) rawPost(ctx context.Context, path string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ServerURL(ctx)+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return "", errors.New(strings.TrimSpace(string(raw)))
	}
	return string(raw), nil
}

// RegistryList browses published MCP/Skill items.
func (m *Manager) RegistryList(ctx context.Context, kind, query string) (string, error) {
	var raw json.RawMessage
	path := "/registry?kind=" + url.QueryEscape(kind)
	if query != "" {
		path += "&q=" + url.QueryEscape(query)
	}
	if err := m.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RegistryGet returns one item's detail (and counts a download).
func (m *Manager) RegistryGet(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/registry/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarketPlugins lists published plugins.
func (m *Manager) MarketPlugins(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/plugins"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// InstallPlugin records an install and returns {bundle_url, slug, name, runtime}.
func (m *Manager) InstallPlugin(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/plugins/"+id+"/install", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ReviewLog lists the plugin review audit log (moderator).
func (m *Manager) ReviewLog(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/plugin-reviews"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ReviewDetail returns one review with its source snapshot.
func (m *Manager) ReviewDetail(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/plugin-reviews/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminPlugins lists plugins of a status for moderators (approve/reject queue).
func (m *Manager) AdminPlugins(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/plugins"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PluginSource fetches a plugin's full source (moderator inspection).
func (m *Manager) PluginSource(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/plugins/"+id+"/source", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RekberAccountGet/Set proxy the admin rekber account setting.
func (m *Manager) RekberAccountGet(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/rekber/account", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) RekberAccountSet(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPut, "/admin/rekber/account", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SetPluginStatus approves/rejects a plugin (moderator override). action = approve|reject.
func (m *Manager) SetPluginStatus(ctx context.Context, id, action string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/plugins/"+id+"/"+action, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// TakedownPlugin removes a plugin from the marketplace (moderator).
func (m *Manager) TakedownPlugin(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodDelete, "/admin/plugins/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PayoutGet/Set proxy the caller's marketplace payout account.
func (m *Manager) PayoutGet(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/payout", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) PayoutSet(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPut, "/marketplace/payout", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RekberDelivery fetches the private delivered goods for a thread (buyer/seller).
func (m *Manager) RekberDelivery(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/rekber/threads/"+id+"/delivery", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RekberOrders lists the caller's delivered rekber deals (My Orders).
func (m *Manager) RekberOrders(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/rekber/orders", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminSettings gets the admin settings (endpoint + has_key, never the key).
func (m *Manager) AdminSettings(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/settings", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SaveAdminSettings updates the admin settings.
func (m *Manager) SaveAdminSettings(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPut, "/admin/settings", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// DownloadBundle fetches a plugin bundle zip from a URL.
func (m *Manager) DownloadBundle(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download failed (%d)", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 40<<20))
}

// PostCreate creates a post.
func (m *Manager) PostCreate(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/posts", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PostAction proxies a post sub-action: method PATCH/DELETE/POST on /posts/{id}{suffix}.
func (m *Manager) PostAction(ctx context.Context, method, id, suffix string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, method, "/posts/"+id+suffix, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- marketplace ---

// MarketplaceList browses listings (query string like "?kind=community&q=...").
func (m *Manager) MarketplaceList(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/listings"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarketplaceMine lists the caller's own listings (any status).
func (m *Manager) MarketplaceMine(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/my-listings", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarketplaceGet fetches one listing.
func (m *Manager) MarketplaceGet(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/listings/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarketplaceCreate creates a listing.
func (m *Manager) MarketplaceCreate(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/marketplace/listings", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarketplaceAction proxies a listing sub-action: PATCH/DELETE on /marketplace/listings/{id}.
func (m *Manager) MarketplaceAction(ctx context.Context, method, id string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, method, "/marketplace/listings/"+id, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- rekber (escrow deals) ---

// RekberGet proxies a GET under /marketplace/rekber (path is the suffix, e.g.
// "/threads" or "/threads/5?after=0").
func (m *Manager) RekberGet(ctx context.Context, path string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/rekber"+path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// RekberPost proxies a POST under /marketplace/rekber.
func (m *Manager) RekberPost(ctx context.Context, path string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/marketplace/rekber"+path, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SellerReviews proxies GET /marketplace/sellers/{id}/reviews.
func (m *Manager) SellerReviews(ctx context.Context, sellerID, query string) (string, error) {
	var raw json.RawMessage
	path := "/marketplace/sellers/" + sellerID + "/reviews"
	if query != "" {
		path += "?" + query
	}
	if err := m.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- bug reports ---

func (m *Manager) ReportBug(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/bug-reports", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) BugReports(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	path := "/admin/bug-reports"
	if query != "" {
		path += "?" + query
	}
	if err := m.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) SetBugStatus(ctx context.Context, id, action string) error {
	return m.call(ctx, http.MethodPost, "/admin/bug-reports/"+id+"/"+action, nil, nil)
}

func (m *Manager) DeleteBug(ctx context.Context, id string) error {
	return m.call(ctx, http.MethodDelete, "/admin/bug-reports/"+id, nil, nil)
}

// --- inbox ---

func (m *Manager) Inbox(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/inbox", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) InboxRead(ctx context.Context, body any) error {
	return m.call(ctx, http.MethodPost, "/inbox/read", body, nil)
}

// --- admin inbox ---

func (m *Manager) SendInbox(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/inbox", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminInboxList(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/inbox", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) DeleteInbox(ctx context.Context, id string) error {
	return m.call(ctx, http.MethodDelete, "/admin/inbox/"+id, nil, nil)
}

func (m *Manager) InboxRoles(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/inbox/roles", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- subscriptions ---

// Subscription fetches the caller's Premium status + the plan on offer.
func (m *Manager) Subscription(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/subscription", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- Gmail store proxies ---

func (m *Manager) GmailStoreInfo(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/store/gmail", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) GmailBuy(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/store/gmail/order", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) GmailOrderStatus(ctx context.Context, ref string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/store/gmail/order/"+url.PathEscape(ref), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) GmailOrderAccounts(ctx context.Context, ref string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/store/gmail/order/"+url.PathEscape(ref)+"/accounts", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Admin (GOD-only, enforced server-side).
func (m *Manager) AdminGmailStock(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/gmail/stock", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminGmailAddStock(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/gmail/stock", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminGmailDeleteStock(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodDelete, "/admin/gmail/stock/"+url.PathEscape(id), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminGmailOrders(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/gmail/orders", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminGmailSetPrice(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPut, "/admin/gmail/price", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SubscriptionOrderStatus proxies a poll of a subscription order's status.
func (m *Manager) SubscriptionOrderStatus(ctx context.Context, ref string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/subscription/order/"+url.PathEscape(ref), nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SubscribePremium starts a Premium payment (optionally with a coupon) and
// returns the gateway response (pay url, or {free:true}).
func (m *Manager) SubscribePremium(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/subscription/subscribe", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ValidateCoupon previews a coupon's discount on the Premium price.
func (m *Manager) ValidateCoupon(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/subscription/validate-coupon", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// GiftPremium gifts Premium to another user (by username), optionally with a coupon.
func (m *Manager) GiftPremium(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/subscription/gift", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// SearchUsers finds users by query (for the gift recipient picker).
func (m *Manager) SearchUsers(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	path := "/search"
	if query != "" {
		path += "?q=" + url.QueryEscape(query)
	}
	if err := m.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- admin coupons ---

func (m *Manager) AdminCoupons(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/coupons", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) CreateCoupon(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/coupons", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) DeleteCoupon(ctx context.Context, id string) error {
	return m.call(ctx, http.MethodDelete, "/admin/coupons/"+id, nil, nil)
}

// --- redeem codes ---

// RedeemCode redeems a premium code for the current user.
func (m *Manager) RedeemCode(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/subscription/redeem", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) AdminRedeemCodes(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/redeem-codes", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) CreateRedeemCode(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/redeem-codes", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Manager) DeleteRedeemCode(ctx context.Context, id string) error {
	return m.call(ctx, http.MethodDelete, "/admin/redeem-codes/"+id, nil, nil)
}

// --- community filter templates ---

// CommunityFilterTemplates browses the public template list.
func (m *Manager) CommunityFilterTemplates(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	path := "/filter-templates"
	if query != "" {
		path += "?" + query
	}
	if err := m.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// GetCommunityFilterTemplate returns one template including its rules.
func (m *Manager) GetCommunityFilterTemplate(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/filter-templates/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// InstallCommunityFilterTemplate bumps the counter and returns the rules to merge.
func (m *Manager) InstallCommunityFilterTemplate(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/filter-templates/"+id+"/install", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PublishFilterTemplate publishes a named set of rules to the community.
func (m *Manager) PublishFilterTemplate(ctx context.Context, body any) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/filter-templates", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// DeleteCommunityFilterTemplate removes a template the user owns.
func (m *Manager) DeleteCommunityFilterTemplate(ctx context.Context, id string) error {
	return m.call(ctx, http.MethodDelete, "/filter-templates/"+id, nil, nil)
}

// --- marketplace orders ---

// OrderCreate starts an official-store order; returns the pay URL.
func (m *Manager) OrderCreate(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/marketplace/orders", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// OrdersList lists the caller's orders.
func (m *Manager) OrdersList(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/orders", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// OrderGet fetches one order (for status polling).
func (m *Manager) OrderGet(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/orders/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// OfficialList lists the curated Official-Store products.
func (m *Manager) OfficialList(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/marketplace/official", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// VIPAdminGet proxies a GET under /admin/vip (e.g. "/balance", "/catalog?kind=..", "/products").
func (m *Manager) VIPAdminGet(ctx context.Context, path string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/vip"+path, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// VIPAdminSend proxies a mutation under /admin/vip.
func (m *Manager) VIPAdminSend(ctx context.Context, method, path string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, method, "/admin/vip"+path, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// PostComments fetches a post's comments.
func (m *Manager) PostComments(ctx context.Context, postID string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/posts/"+postID+"/comments", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// CommentAdd posts a comment on a post.
func (m *Manager) CommentAdd(ctx context.Context, postID string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/posts/"+postID+"/comments", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// CommentAction proxies an action on a comment by id (edit/delete/react).
func (m *Manager) CommentAction(ctx context.Context, method, id, suffix string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, method, "/comments/"+id+suffix, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Search runs a text search over posts + users (query appended).
func (m *Manager) Search(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/search"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Notifications fetches the user's notifications + unread count.
func (m *Manager) Notifications(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/notifications", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// CommunityStats proxies coarse community numbers (total + online users).
func (m *Manager) CommunityStats(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/community/stats", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// KleosDailyStatus peeks at today's daily-login reward (amount + already claimed).
func (m *Manager) KleosDailyStatus(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/kleos/daily", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// KleosDaily claims the daily-login Kleos.
func (m *Manager) KleosDaily(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/kleos/daily", map[string]any{}, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// LegacyAccounts proxies the account-restore migration lookup (decrypted old
// provider accounts for the logged-in user).
func (m *Manager) LegacyAccounts(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/legacy/accounts", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// NotificationsRead marks all notifications read.
func (m *Manager) NotificationsRead(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/notifications/read", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminFlags fetches the moderator duplicate-account review queue.
func (m *Manager) AdminFlags(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/flags", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminReviewFlag dismisses a flagged link.
func (m *Manager) AdminReviewFlag(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/admin/flags/"+id+"/review", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminLog fetches the moderation audit log.
func (m *Manager) AdminLog(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/log", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminStats fetches community-wide admin counters.
func (m *Manager) AdminStats(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/stats", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminUsers fetches the default admin user list (moderators first).
func (m *Manager) AdminUsers(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/users", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminUserAction proxies a user-targeted moderator action (moderator, ban,
// mute, warn, kleos) with the given JSON body to the cloud.
func (m *Manager) AdminUserAction(ctx context.Context, id, action string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	var reqBody any
	if len(body) > 0 {
		reqBody = body
	}
	if err := m.call(ctx, http.MethodPost, "/admin/users/"+id+"/"+action, reqBody, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// AdminUserDetail fetches the full admin detail for one user.
func (m *Manager) AdminUserDetail(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/admin/users/"+id+"/detail", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Shop fetches the cosmetics catalog + the user's owned/equipped/balance.
func (m *Manager) Shop(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/shop", nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ShopBuy spends Kleos to buy a cosmetic.
func (m *Manager) ShopBuy(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/shop/buy", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ShopEquip equips/unequips a cosmetic.
func (m *Manager) ShopEquip(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/shop/equip", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatList fetches a page of community chat messages (path includes any query).
func (m *Manager) ChatList(ctx context.Context, query string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodGet, "/chat/messages"+query, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatSend posts a community chat message.
func (m *Manager) ChatSend(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/chat/messages", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatShareMusic posts a music card to the music channel.
func (m *Manager) ChatShareMusic(ctx context.Context, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/chat/share-music", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatEdit edits the caller's own chat message.
func (m *Manager) ChatEdit(ctx context.Context, id string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPatch, "/chat/messages/"+id, body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatDelete deletes the caller's own chat message.
func (m *Manager) ChatDelete(ctx context.Context, id string) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodDelete, "/chat/messages/"+id, nil, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatReact toggles an emoji reaction on a chat message.
func (m *Manager) ChatReact(ctx context.Context, id string, body json.RawMessage) (string, error) {
	var raw json.RawMessage
	if err := m.call(ctx, http.MethodPost, "/chat/messages/"+id+"/reactions", body, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- protocol types (must match the enowxlabs server) ---

type item struct {
	ItemID    string `json:"id"`
	Type      string `json:"type"`
	Version   int64  `json:"version"`
	UpdatedAt int64  `json:"updated_at"`
	Deleted   bool   `json:"deleted"`
	Encrypted bool   `json:"encrypted"`
	Payload   string `json:"payload,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
}

type manifestEntry struct {
	ItemID    string `json:"id"`
	Type      string `json:"type"`
	Version   int64  `json:"version"`
	UpdatedAt int64  `json:"updated_at"`
	Deleted   bool   `json:"deleted"`
}

const typePlaylist = "playlist"

func playlistItemID(shareCode string) string { return typePlaylist + ":" + shareCode }

// Sync runs one full reconcile: push locally-newer items, pull server-newer
// ones, apply them. Returns the number pushed and pulled.
func (m *Manager) Sync(ctx context.Context) (pushed, pulled int, err error) {
	if !m.Configured(ctx) {
		return 0, 0, fmt.Errorf("sync not configured")
	}

	// Local snapshot keyed by item id.
	local, err := m.localPlaylistItems(ctx)
	if err != nil {
		return 0, 0, err
	}
	// Full sync (accounts/keys/aliases/custom providers) when entitled.
	m.fullSyncItems(ctx, local)

	// Server manifest keyed by item id.
	var man struct {
		Items []manifestEntry `json:"items"`
		Now   int64           `json:"now"`
	}
	if err := m.call(ctx, http.MethodGet, "/sync/manifest", nil, &man); err != nil {
		return 0, 0, err
	}
	remote := map[string]manifestEntry{}
	for _, e := range man.Items {
		remote[e.ItemID] = e
	}

	// Tombstone: gated items on the server that no longer exist locally (the user
	// deleted the account/key/provider) must be pushed as deletions, else they'd
	// linger on the cloud and re-appear on other devices. Guard rails:
	//   - only when entitled (a non-entitled snapshot omits gated types), and
	//   - only once this device has pulled before (cursor > 0). A fresh device
	//     has an empty local store and must PULL first, never conclude "deleted".
	if m.hasFullSync(ctx) && m.cursor(ctx) > 0 {
		for id, re := range remote {
			if _, stillLocal := local[id]; stillLocal || re.Deleted {
				continue
			}
			if !gatedItemID(id) {
				continue // leave playlists (and anything else) to their own logic
			}
			local[id] = item{ItemID: id, Type: re.Type, Version: re.Version + 1, UpdatedAt: nowMillis(), Deleted: true}
		}
	}

	// Push: local items the server lacks or that are strictly newer locally.
	var toPush []item
	for id, li := range local {
		re, ok := remote[id]
		if !ok || li.UpdatedAt > re.UpdatedAt {
			toPush = append(toPush, li)
		}
	}
	if len(toPush) > 0 {
		var resp struct {
			Accepted []string `json:"accepted"`
		}
		if err := m.call(ctx, http.MethodPost, "/sync/push", map[string]any{"items": toPush}, &resp); err != nil {
			return 0, 0, err
		}
		pushed = len(resp.Accepted)
	}

	// Pull: server items newer than our cursor, apply the ones that win locally.
	cursor := m.cursor(ctx)
	var pull struct {
		Items []item `json:"items"`
		Now   int64  `json:"now"`
	}
	if err := m.call(ctx, http.MethodGet, "/sync/pull?since="+fmt.Sprint(cursor), nil, &pull); err != nil {
		return pushed, 0, err
	}

	// Pre-build dedup lookup maps so each pulled item does O(1) checks
	// instead of scanning the entire store per item (O(n^2) at 1300+ accounts).
	var lk fullSyncLookup
	if m.hasFullSync(ctx) {
		if m.accounts != nil {
			if all, err := m.accounts.List(ctx, ""); err == nil {
				lk.accounts = make(map[string]bool, len(all))
				for _, a := range all {
					lk.accounts[shortHash(a.Secret+fmt.Sprint(a.Creds))] = true
				}
			}
		}
		if m.keys != nil {
			if all, err := m.keys.List(ctx); err == nil {
				lk.keys = make(map[string]bool, len(all))
				for _, k := range all {
					lk.keys[shortHash(k.Secret)] = true
				}
			}
		}
		if m.proxies != nil {
			if all, err := m.proxies.List(ctx); err == nil {
				lk.proxies = make(map[string]bool, len(all))
				for _, p := range all {
					lk.proxies[shortHash(p.Scheme+p.Host+fmt.Sprint(p.Port)+p.Username)] = true
				}
			}
		}
		if m.custom != nil {
			if all, err := m.custom.List(ctx); err == nil {
				lk.prefixes = make(map[string]bool, len(all))
				for _, cp := range all {
					lk.prefixes[cp.Prefix] = true
				}
			}
		}
	}

	for _, ri := range pull.Items {
		if ri.Type != typePlaylist {
			// Full-sync types (accounts/keys/aliases/custom providers).
			if m.applyFullItem(ctx, ri, &lk) {
				pulled++
			}
			continue
		}
		li, have := local[ri.ItemID]
		if have && li.UpdatedAt >= ri.UpdatedAt {
			continue // local is newer or equal — keep it
		}
		sp, perr := decodePlaylist(ri)
		if perr != nil {
			continue
		}
		if err := m.music.ApplySyncedPlaylist(ctx, sp); err != nil {
			return pushed, pulled, err
		}
		pulled++
	}

	// Advance cursor to the server's clock at manifest time.
	if pull.Now > 0 {
		_ = m.settings.Set(ctx, keyCursor, fmt.Sprint(pull.Now))
	}
	return pushed, pulled, nil
}

// reportUsage tells the server this device's cumulative successful output-token
// count, so the server can credit Kleos for the delta (idempotent). Best effort
// — failures are non-fatal and just retried on the next cycle.
func (m *Manager) reportUsage(ctx context.Context) {
	if m.logs == nil || !m.Configured(ctx) {
		return
	}
	total, err := m.logs.TotalOutTokens(ctx)
	if err != nil {
		return
	}
	// Also report the lifetime usage summary (requests + tokens) for the cloud
	// per-user stats. Ungated — sent by every logged-in user, not just premium.
	reqs, inTok, outTok, _ := m.logs.Totals(ctx)
	_ = m.call(ctx, http.MethodPost, "/usage/report", map[string]any{
		"out_tokens":      total,
		"device_id":       m.deviceID(ctx),
		"stat_requests":   reqs,
		"stat_in_tokens":  inTok,
		"stat_out_tokens": outTok,
	}, nil)
}

// Presence reports whether the user's dashboard is currently open (a browser tab
// is connected to the gateway). Only sent while active; the server marks the user
// offline when heartbeats stop. Best-effort.
func (m *Manager) Presence(ctx context.Context, active bool) {
	if !m.Configured(ctx) {
		return
	}
	_ = m.call(ctx, http.MethodPost, "/presence", map[string]any{"active": active}, nil)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) cursor(ctx context.Context) int64 {
	var c int64
	fmt.Sscan(m.get(ctx, keyCursor), &c)
	return c
}

// localPlaylistItems snapshots local playlists as sync items keyed by item id.
func (m *Manager) localPlaylistItems(ctx context.Context) (map[string]item, error) {
	if m.music == nil {
		return map[string]item{}, nil
	}
	pls, err := m.music.PlaylistsForSync(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]item{}
	for _, p := range pls {
		if p.ShareCode == "" {
			continue // can't address it without a stable id
		}
		payload, _ := json.Marshal(p)
		out[playlistItemID(p.ShareCode)] = item{
			ItemID:    playlistItemID(p.ShareCode),
			Type:      typePlaylist,
			Version:   p.Version,
			UpdatedAt: p.UpdatedAt,
			Deleted:   p.Deleted,
			Payload:   string(payload),
		}
	}
	return out, nil
}

func decodePlaylist(ri item) (store.SyncedPlaylist, error) {
	var sp store.SyncedPlaylist
	if err := json.Unmarshal([]byte(ri.Payload), &sp); err != nil {
		return store.SyncedPlaylist{}, err
	}
	// Trust the item's metadata as authoritative.
	sp.UpdatedAt, sp.Version, sp.Deleted = ri.UpdatedAt, ri.Version, ri.Deleted
	return sp, nil
}

// call performs an authenticated JSON request against the sync server.
func (m *Manager) call(ctx context.Context, method, path string, body any, out any) error {
	return m.do(ctx, method, path, body, out, true)
}

// OTPProxy forwards an authed request to the cloud's /otp/* API and returns the
// upstream status + raw body verbatim (the OTP endpoints return bare JSON and
// may relay non-2xx from Warpize, so we don't decode or wrap here).
func (m *Manager) OTPProxy(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.ServerURL(ctx)+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// callNoAuth is for the public OAuth endpoints (no token yet).
func (m *Manager) callNoAuth(ctx context.Context, method, path string, body any, out any) error {
	return m.do(ctx, method, path, body, out, false)
}

func (m *Manager) do(ctx context.Context, method, path string, body any, out any, auth bool) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.ServerURL(ctx)+path, rdr)
	if err != nil {
		return err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+m.get(ctx, keyToken))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("sync unauthorized (token invalid or revoked)")
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := strings.TrimSpace(string(b))
		// 4xx bodies are user-facing messages from the cloud ("you have
		// already redeemed this code"). Surface them verbatim — no HTTP
		// noise. 5xx are opaque server errors, so keep a little context.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && msg != "" {
			return errors.New(msg)
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("the server had a problem (%d): %s", resp.StatusCode, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
