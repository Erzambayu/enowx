package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enowdev/enowx/store"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// Sync item types beyond playlists. Must match the cloud's gated set.
const (
	typeCustomProvider = "custom_provider"
	typeAccount        = "account"
	typeAPIKey         = "apikey"
	typeAlias          = "alias"
	typeProxy          = "proxy"
	typeCombo          = "combo"
)

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// fullHash is the full sha256 hex of a string (non-reversible), used as the
// cloud-side lookup key for a gateway API key.
func fullHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// gatedItemID reports whether a sync item id belongs to a gated (full-sync) type,
// so tombstoning only touches those (not playlists).
func gatedItemID(id string) bool {
	for _, t := range []string{typeCustomProvider, typeAccount, typeAPIKey, typeAlias, typeProxy, typeCombo} {
		if strings.HasPrefix(id, t+":") {
			return true
		}
	}
	return false
}

// --- entitlement + key from cached /me ---

type cachedMe struct {
	SyncKey      string   `json:"sync_key"`
	KDFSalt      string   `json:"kdf_salt"`
	Entitlements []string `json:"entitlements"`
}

func (m *Manager) cachedMe(ctx context.Context) cachedMe {
	var me cachedMe
	_ = json.Unmarshal([]byte(m.get(ctx, keyUser)), &me)
	return me
}

// hasEntitlement reports whether the logged-in user has a capability, per the
// cached /me. The cloud is the source of truth; this is its last synced value.
func (m *Manager) hasEntitlement(ctx context.Context, cap string) bool {
	for _, e := range m.cachedMe(ctx).Entitlements {
		if e == cap {
			return true
		}
	}
	return false
}

// hasFullSync reports whether the logged-in user may sync the gated types.
func (m *Manager) hasFullSync(ctx context.Context) bool {
	return m.hasEntitlement(ctx, "cloud.sync.full")
}

// HasEntitlement is the exported check other packages use to gate features on the
// user's cloud entitlements (e.g. plugins.use for premium-only plugins).
func (m *Manager) HasEntitlement(ctx context.Context, cap string) bool {
	return m.hasEntitlement(ctx, cap)
}

// credKey derives the AES key for sealing credentials, or nil if unavailable.
func (m *Manager) credKey(ctx context.Context) []byte {
	me := m.cachedMe(ctx)
	key, err := deriveKey(me.SyncKey, me.KDFSalt)
	if err != nil {
		return nil
	}
	return key
}

// --- snapshot: local rows → sync items ---

// fullSyncItems adds accounts/keys/aliases/custom-providers to the snapshot when
// the user is entitled. Plaintext for definitions/aliases, AES-GCM for creds.
func (m *Manager) fullSyncItems(ctx context.Context, out map[string]item) {
	if !m.hasFullSync(ctx) {
		return
	}
	key := m.credKey(ctx)

	// Custom providers (plaintext) — keyed by prefix (stable across devices).
	if m.custom != nil {
		if list, err := m.custom.List(ctx); err == nil {
			for _, cp := range list {
				payload, _ := json.Marshal(cp)
				id := typeCustomProvider + ":" + cp.Prefix
				out[id] = item{ItemID: id, Type: typeCustomProvider, Version: 1, UpdatedAt: nowMillis(), Payload: string(payload)}
			}
		}
	}

	// Accounts (encrypted) — keyed by provider + a hash of the credentials, so
	// the same account maps to the same id on every device.
	if m.accounts != nil && key != nil {
		if list, err := m.accounts.List(ctx, ""); err == nil {
			for _, a := range list {
				raw, _ := json.Marshal(syncAccount{Provider: a.Provider, Label: a.Label, Secret: a.Secret, Creds: a.Creds, Status: a.Status, Disabled: a.Disabled})
				payload, nonce, err := seal(key, raw)
				if err != nil {
					continue
				}
				id := typeAccount + ":" + a.Provider + ":" + shortHash(a.Secret+fmt.Sprint(a.Creds))
				out[id] = item{ItemID: id, Type: typeAccount, Version: 1, UpdatedAt: nowMillis(), Encrypted: true, Payload: payload, Nonce: nonce}
			}
		}
	}

	// Gateway API keys (encrypted) — keyed by a hash of the secret.
	if m.keys != nil && key != nil {
		if list, err := m.keys.List(ctx); err == nil {
			for _, k := range list {
				raw, _ := json.Marshal(syncKey{Label: k.Label, Secret: k.Secret, TokenLimit: k.TokenLimit, MaxConcurrent: k.MaxConcurrent, Enabled: k.Enabled})
				payload, nonce, err := seal(key, raw)
				if err != nil {
					continue
				}
				id := typeAPIKey + ":" + shortHash(k.Secret)
				out[id] = item{ItemID: id, Type: typeAPIKey, Version: 1, UpdatedAt: nowMillis(), Encrypted: true, Payload: payload, Nonce: nonce}
			}
		}
	}

	// Proxies (encrypted — they carry credentials) — keyed by endpoint identity.
	if m.proxies != nil && key != nil {
		if list, err := m.proxies.List(ctx); err == nil {
			for _, p := range list {
				raw, _ := json.Marshal(syncProxy{
					Label: p.Label, Scheme: p.Scheme, Host: p.Host, Port: p.Port,
					Username: p.Username, Password: p.Password, Enabled: p.Enabled,
				})
				payload, nonce, err := seal(key, raw)
				if err != nil {
					continue
				}
				id := typeProxy + ":" + shortHash(p.Scheme+p.Host+fmt.Sprint(p.Port)+p.Username)
				out[id] = item{ItemID: id, Type: typeProxy, Version: 1, UpdatedAt: nowMillis(), Encrypted: true, Payload: payload, Nonce: nonce}
			}
		}
	}

	// Model aliases (plaintext) — keyed by alias.
	if m.aliases != nil {
		if list, err := m.aliases.List(ctx); err == nil {
			for _, al := range list {
				payload, _ := json.Marshal(al)
				id := typeAlias + ":" + al.Alias
				out[id] = item{ItemID: id, Type: typeAlias, Version: 1, UpdatedAt: nowMillis(), Payload: string(payload)}
			}
		}
	}

	// Model combos (plaintext) — keyed by name. last_index (the round-robin
	// cursor) is device-local runtime state and is intentionally not synced.
	if m.combos != nil {
		if list, err := m.combos.List(ctx); err == nil {
			for _, c := range list {
				payload, _ := json.Marshal(syncCombo{Name: c.Name, Targets: c.Targets, Strategy: c.Strategy})
				id := typeCombo + ":" + c.Name
				out[id] = item{ItemID: id, Type: typeCombo, Version: 1, UpdatedAt: nowMillis(), Payload: string(payload)}
			}
		}
	}
}

// syncAccount / syncKey are the on-the-wire (encrypted) shapes.
type syncAccount struct {
	Provider string            `json:"provider"`
	Label    string            `json:"label"`
	Secret   string            `json:"secret"`
	Creds    map[string]string `json:"creds"`
	Status   string            `json:"status"`
	Disabled bool              `json:"disabled"`
}

type syncKey struct {
	Label         string `json:"label"`
	Secret        string `json:"secret"`
	TokenLimit    int64  `json:"token_limit"`
	MaxConcurrent int64  `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
}

type syncProxy struct {
	Label    string `json:"label"`
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Enabled  bool   `json:"enabled"`
}

type syncCombo struct {
	Name     string              `json:"name"`
	Targets  []string            `json:"targets"`
	Strategy store.ComboStrategy `json:"strategy"`
}

// --- apply: pulled items → local rows ---

type fullSyncLookup struct {
	accounts map[string]bool // shortHash(secret+creds) → true
	keys     map[string]bool // shortHash(secret) → true
	proxies  map[string]bool // shortHash(scheme+host+port+username) → true
	prefixes map[string]bool // custom provider prefix → true
}

// applyFullItem applies one non-playlist pulled item. Returns true if handled.
// lookup maps, when non-nil, provide O(1) dedup instead of scanning the store.
func (m *Manager) applyFullItem(ctx context.Context, ri item, lk *fullSyncLookup) bool {
	switch ri.Type {
	case typeCustomProvider:
		return m.applyCustomProvider(ctx, ri, lk)
	case typeAccount:
		return m.applyAccount(ctx, ri, lk)
	case typeAPIKey:
		return m.applyAPIKey(ctx, ri, lk)
	case typeProxy:
		return m.applyProxy(ctx, ri, lk)
	case typeAlias:
		return m.applyAlias(ctx, ri)
	case typeCombo:
		return m.applyCombo(ctx, ri)
	}
	return false
}

func (m *Manager) applyCustomProvider(ctx context.Context, ri item, lk *fullSyncLookup) bool {
	if m.custom == nil {
		return false
	}
	existing, _ := m.custom.List(ctx)

	// Deletion: the id is custom_provider:<prefix>. Delete + unregister the local
	// provider with that prefix. Tombstones carry no payload.
	if ri.Deleted {
		prefix, ok := strings.CutPrefix(ri.ItemID, typeCustomProvider+":")
		if !ok {
			return false
		}
		for _, e := range existing {
			if e.Prefix == prefix {
				_ = m.custom.Delete(ctx, e.ID)
				if m.onCustomDelete != nil {
					m.onCustomDelete(e.Prefix, e.Name) // unregister live
				}
			}
		}
		return true
	}

	var cp store.CustomProvider
	if json.Unmarshal([]byte(ri.Payload), &cp) != nil {
		return false
	}
	if lk != nil && lk.prefixes != nil {
		if lk.prefixes[cp.Prefix] {
			return true
		}
	} else {
		// Upsert by prefix: skip if we already have this prefix, else create + register.
		for _, e := range existing {
			if e.Prefix == cp.Prefix {
				return true // already present (LWW: keep local)
			}
		}
	}
	id, err := m.custom.Create(ctx, cp)
	if err != nil {
		return false
	}
	cp.ID = id
	if m.onCustomProvider != nil {
		m.onCustomProvider(cp) // register live
	}
	return true
}

func (m *Manager) applyAccount(ctx context.Context, ri item, lk *fullSyncLookup) bool {
	if m.accounts == nil {
		return false
	}
	// Deletion: the id is account:<provider>:<hash(secret+creds)>. Delete the
	// local account whose content hash matches. Tombstones carry no payload.
	if ri.Deleted {
		prov, hash, ok := parseAccountID(ri.ItemID)
		if !ok {
			return false
		}
		existing, _ := m.accounts.List(ctx, prov)
		for _, e := range existing {
			if shortHash(e.Secret+fmt.Sprint(e.Creds)) == hash {
				_ = m.accounts.Delete(ctx, e.ID)
			}
		}
		return true
	}
	if !ri.Encrypted {
		return false
	}
	// Fast-path: skip decryption for accounts we already have locally.
	// The item id embeds shortHash(secret+creds) which matches the dedup
	// map key, so we check before any crypto or SQLite work.
	if lk != nil && lk.accounts != nil {
		if _, hash, ok := parseAccountID(ri.ItemID); ok && lk.accounts[hash] {
			return true
		}
	}
	key := m.credKey(ctx)
	if key == nil {
		return false
	}
	raw, err := open(key, ri.Payload, ri.Nonce)
	if err != nil {
		return false
	}
	var sa syncAccount
	if json.Unmarshal(raw, &sa) != nil {
		return false
	}
	// Fallback dedup when no lookup map was provided (legacy path).
	if lk == nil || lk.accounts == nil {
		target := shortHash(sa.Secret + fmt.Sprint(sa.Creds))
		existing, _ := m.accounts.List(ctx, sa.Provider)
		for _, e := range existing {
			if shortHash(e.Secret+fmt.Sprint(e.Creds)) == target {
				return true
			}
		}
	}
	_, _ = m.accounts.Add(ctx, store.Account{Provider: sa.Provider, Label: sa.Label, Secret: sa.Secret, Creds: sa.Creds, Status: sa.Status, Disabled: sa.Disabled})
	return true
}

func (m *Manager) applyProxy(ctx context.Context, ri item, lk *fullSyncLookup) bool {
	if m.proxies == nil {
		return false
	}
	proxyHash := func(p store.Proxy) string {
		return shortHash(p.Scheme + p.Host + fmt.Sprint(p.Port) + p.Username)
	}
	// Deletion: id is proxy:<hash>. Delete the local proxy whose identity matches.
	if ri.Deleted {
		hash := strings.TrimPrefix(ri.ItemID, typeProxy+":")
		existing, _ := m.proxies.List(ctx)
		for _, e := range existing {
			if proxyHash(e) == hash {
				_ = m.proxies.Delete(ctx, e.ID)
			}
		}
		return true
	}
	if !ri.Encrypted {
		return false
	}
	key := m.credKey(ctx)
	if key == nil {
		return false
	}
	raw, err := open(key, ri.Payload, ri.Nonce)
	if err != nil {
		return false
	}
	var sp syncProxy
	if json.Unmarshal(raw, &sp) != nil {
		return false
	}
	target := shortHash(sp.Scheme + sp.Host + fmt.Sprint(sp.Port) + sp.Username)
	if lk != nil && lk.proxies != nil {
		if lk.proxies[target] {
			return true
		}
	}
	// Add upserts on identity, so this is idempotent whether or not it exists.
	_, _ = m.proxies.Add(ctx, store.Proxy{
		Label: sp.Label, Scheme: sp.Scheme, Host: sp.Host, Port: sp.Port,
		Username: sp.Username, Password: sp.Password, Enabled: sp.Enabled,
	})
	return true
}

// parseAccountID splits "account:<provider>:<hash>" (provider may contain no colon).
func parseAccountID(id string) (provider, hash string, ok bool) {
	rest, found := strings.CutPrefix(id, typeAccount+":")
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

func (m *Manager) applyAPIKey(ctx context.Context, ri item, lk *fullSyncLookup) bool {
	if m.keys == nil {
		return false
	}
	// Deletion: the id is apikey:<hash(secret)>. Delete the local key whose
	// secret hashes to it.
	if ri.Deleted {
		hash, ok := strings.CutPrefix(ri.ItemID, typeAPIKey+":")
		if !ok {
			return false
		}
		keys, _ := m.keys.List(ctx)
		for _, e := range keys {
			if shortHash(e.Secret) == hash {
				_ = m.keys.Delete(ctx, e.ID)
			}
		}
		return true
	}
	if !ri.Encrypted {
		return false
	}
	key := m.credKey(ctx)
	if key == nil {
		return false
	}
	raw, err := open(key, ri.Payload, ri.Nonce)
	if err != nil {
		return false
	}
	var sk syncKey
	if json.Unmarshal(raw, &sk) != nil {
		return false
	}
	if lk != nil && lk.keys != nil {
		if lk.keys[shortHash(sk.Secret)] {
			return true
		}
	} else {
		if existing, _ := m.keys.BySecret(ctx, sk.Secret); existing != nil {
			return true // already have it
		}
	}
	_, _ = m.keys.Add(ctx, store.APIKey{Label: sk.Label, Secret: sk.Secret, TokenLimit: sk.TokenLimit, MaxConcurrent: sk.MaxConcurrent, Enabled: sk.Enabled})
	return true
}

func (m *Manager) applyAlias(ctx context.Context, ri item) bool {
	if m.aliases == nil {
		return false
	}
	var al store.ModelAlias
	if json.Unmarshal([]byte(ri.Payload), &al) != nil {
		return false
	}
	if ri.Deleted {
		_ = m.aliases.Delete(ctx, al.Alias)
		return true
	}
	if m.combos != nil {
		if combos, err := m.combos.List(ctx); err == nil {
			for _, c := range combos {
				if c.Name == al.Alias {
					return true // name collides with an existing combo locally — skip, don't clobber
				}
			}
		}
	}
	_ = m.aliases.Set(ctx, al.Alias, al.Target)
	return true
}

func (m *Manager) applyCombo(ctx context.Context, ri item) bool {
	if m.combos == nil {
		return false
	}
	var c syncCombo
	if json.Unmarshal([]byte(ri.Payload), &c) != nil {
		return false
	}
	if ri.Deleted {
		_ = m.combos.DeleteByName(ctx, c.Name)
		return true
	}
	if m.aliases != nil {
		if aliases, err := m.aliases.List(ctx); err == nil {
			for _, a := range aliases {
				if a.Alias == c.Name {
					return true // name collides with an existing alias locally — skip, don't clobber
				}
			}
		}
	}
	_ = m.combos.SetByName(ctx, c.Name, c.Targets, c.Strategy)
	return true
}
