import { useEffect, useRef, useState } from "react";
import { Play, Square, Trash2, Plus, X, ScrollText, ExternalLink, Puzzle, AlertTriangle, RefreshCw, FolderOpen, UploadCloud, Store, Download, Loader2, Crown } from "lucide-react";
import { AppShell, Empty } from "./shell";
import { Tooltip } from "../components/Tooltip";
import { useDialog } from "../os/dialog";
import { notifyPluginsChanged } from "../os/pluginBus";
import { subscribeLive } from "../os/liveBus";
import { useProfile } from "../os/useProfile";
import { SignInGate } from "../components/SignInGate";
import { pluginsApi, marketApi, type PluginManifest, type PluginRuntime, type MarketPlugin } from "../lib/api";
import { compareVersions } from "../lib/semver";

const RUNTIMES = [
  { id: "python", label: "Python" },
  { id: "node", label: "Node.js" },
  { id: "go", label: "Go" },
  { id: "static", label: "Static (HTML/JS)" },
];

export function PluginsApp() {
  const [plugins, setPlugins] = useState<PluginManifest[]>([]);
  const [runtimes, setRuntimes] = useState<PluginRuntime[]>([]);
  const [marketCatalog, setMarketCatalog] = useState<MarketPlugin[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [logsFor, setLogsFor] = useState<string | null>(null);
  const [openFor, setOpenFor] = useState<PluginManifest | null>(null);
  const dialog = useDialog();
  const isPremium = useProfile().has("plugins.use");

  const load = async () => {
    try {
      const r = await pluginsApi.list();
      setPlugins(r.plugins ?? []);
      setRuntimes(r.runtimes ?? []);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to load");
    }
  };
  useEffect(() => {
    load();
  }, []);
  useEffect(() => {
    marketApi.list().then((r) => setMarketCatalog(r?.plugins ?? [])).catch(() => {});
  }, []);

  // static + bin need no toolchain (bin ships prebuilt per-OS binaries).
  const runtimeOk = (id: string) => id === "static" || id === "bin" || runtimes.find((r) => r.id === id)?.available;

  const act = async (fn: () => Promise<unknown>, id: string) => {
    setBusy(id);
    setError("");
    try {
      await fn();
      await load();
      notifyPluginsChanged(); // update the desktop's plugin apps immediately
    } catch (e) {
      setError(e instanceof Error ? e.message : "action failed");
    } finally {
      setBusy(null);
    }
  };

  const remove = async (p: PluginManifest) => {
    const ok = await dialog.confirm({ title: "Delete plugin?", message: `${p.name} and its folder will be removed. This cannot be undone.`, confirmLabel: "Delete", danger: true });
    if (ok) act(() => pluginsApi.remove(p.id), p.id);
  };

  const publish = async (p: PluginManifest) => {
    const ver = p.version || "1.0.0";
    const ok = await dialog.confirm({
      title: "Publish to marketplace?",
      message: `${p.name} v${ver} will be uploaded and security-scanned. If it passes, it's listed publicly for others to install. Re-publishing an id you already own updates it in place — bump "version" in plugin.json first.`,
      confirmLabel: "Publish",
    });
    if (!ok) return;
    setBusy(p.id);
    setError("");
    try {
      const r = await marketApi.publish(p.id);
      if (r.status === "approved") {
        await dialog.alert({ title: r.updated ? "Updated" : "Published", message: `${p.name} v${ver} passed the scan and is now ${r.updated ? "updated in" : "listed in"} the marketplace.` });
      } else if (r.status === "pending") {
        await dialog.alert({ title: "Pending review", message: r.reason || `${p.name} is queued for manual moderator review and will be listed once approved.` });
      } else {
        await dialog.alert({ title: "Rejected", message: r.reason ? `${r.reason}${r.file ? ` (${r.file})` : ""}` : "The security scan rejected this plugin." });
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "publish failed");
    } finally {
      setBusy(null);
    }
  };

  const [tab, setTab] = useState<"mine" | "market">("mine");

  const marketBySlug = new Map(marketCatalog.map((m) => [m.slug, m]));

  return (
    <AppShell title="Plugins" subtitle="Build and run your own apps & automations">
      {!isPremium && (
        <div className="mb-3 flex items-center gap-2.5 rounded-xl border border-amber-400/25 bg-amber-400/[0.06] px-3 py-2.5">
          <Crown className="h-4 w-4 shrink-0 text-amber-300" />
          <div className="flex-1 text-[11px] leading-snug text-amber-100/90">
            Plugins are a <span className="font-semibold">Premium</span> feature. You can browse, but installing and running plugins needs Premium — the gateway enforces it, so shared plugins won't run without it.
          </div>
        </div>
      )}
      <div className="mb-3 flex gap-1">
        <button onClick={() => setTab("mine")} className={`flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium ${tab === "mine" ? "bg-white/12 text-white" : "text-white/50 hover:bg-white/5"}`}><Puzzle className="h-3.5 w-3.5" /> My Plugins</button>
        <button onClick={() => setTab("market")} className={`flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium ${tab === "market" ? "bg-white/12 text-white" : "text-white/50 hover:bg-white/5"}`}><Store className="h-3.5 w-3.5" /> Marketplace</button>
      </div>

      {tab === "market" ? <Marketplace onInstalled={load} installedVersions={new Map(plugins.map((p) => [p.id, p.version ?? ""]))} /> : (
      <>
      <div className="mb-3 flex items-center gap-2">
        <button onClick={() => setCreating(true)} className="flex items-center gap-1.5 rounded-lg bg-white px-3 py-1.5 text-xs font-medium text-black hover:opacity-90">
          <Plus className="h-3.5 w-3.5" /> New plugin
        </button>
        <button onClick={load} title="Refresh" className="rounded-lg border border-white/10 p-1.5 text-white/50 hover:bg-white/5 hover:text-white"><RefreshCw className="h-3.5 w-3.5" /></button>
        <div className="ml-auto flex items-center gap-1.5 text-[11px] text-white/40">
          {runtimes.map((r) => (
            <span key={r.id} className={`rounded px-1.5 py-0.5 ${r.available ? "bg-emerald-500/15 text-emerald-300" : "bg-white/5 text-white/30"}`} title={r.version || (r.available ? "" : "not installed")}>
              {r.id}
            </span>
          ))}
        </div>
      </div>

      <div className="mb-3 flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-[11px] text-amber-200/90">
        <AlertTriangle className="mt-[1px] h-3.5 w-3.5 shrink-0" />
        Plugins run on your machine with full access. Only run plugins you trust.
      </div>

      {error && <div className="mb-3 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">{error}</div>}

      {plugins.length === 0 ? (
        <Empty message="No plugins yet. Create one to get a starter you can edit in ~/.enowx/plugins." />
      ) : (
        <div className="space-y-2">
          {plugins.map((p) => (
            <div key={p.id} className="group flex items-center gap-3 rounded-xl border border-white/10 bg-white/[0.02] p-3">
              <div className="flex h-9 w-9 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-white/5 text-white/60">
                {p.has_icon ? <img src={pluginsApi.iconUrl(p.id)} alt="" className="h-full w-full object-cover" /> : <Puzzle className="h-4 w-4" />}
              </div>
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5">
                  <span className="truncate text-sm font-medium text-white">{p.name}</span>
                  <span className="rounded bg-white/10 px-1 text-[9px] uppercase text-white/50">{p.runtime}</span>
                  {p.version && <span className="rounded bg-white/10 px-1 text-[9px] text-white/50">v{p.version}</span>}
                  {p.running && <span className="rounded bg-emerald-500/20 px-1 text-[9px] text-emerald-300">running</span>}
                  {!runtimeOk(p.runtime) && <span className="rounded bg-red-500/20 px-1 text-[9px] text-red-300">{p.runtime} missing</span>}
                </div>
                <div className="mt-0.5 truncate font-mono text-[10px] text-white/35">{p.id} · {p.error || p.description || p.entry}</div>
              </div>
              <div className="flex shrink-0 items-center gap-1">
                <Tooltip label="Open">
                  <button onClick={() => setOpenFor(p)} disabled={p.runtime !== "static" && !p.running} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white disabled:opacity-40"><ExternalLink className="h-3.5 w-3.5" /></button>
                </Tooltip>
                {p.runtime !== "static" && (p.running ? (
                  <Tooltip label="Stop"><button onClick={() => act(() => pluginsApi.stop(p.id), p.id)} disabled={busy === p.id} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white disabled:opacity-40"><Square className="h-3.5 w-3.5" /></button></Tooltip>
                ) : (
                  <Tooltip label="Start"><button onClick={() => act(() => pluginsApi.start(p.id), p.id)} disabled={busy === p.id || !runtimeOk(p.runtime)} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white disabled:opacity-40"><Play className="h-3.5 w-3.5" /></button></Tooltip>
                ))}
                <Tooltip label="Open folder (edit in your IDE)"><button onClick={() => pluginsApi.reveal(p.id).catch(() => {})} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white"><FolderOpen className="h-3.5 w-3.5" /></button></Tooltip>
                {(() => {
                  const listing = marketBySlug.get(p.id);
                  if (!listing || compareVersions(listing.version, p.version ?? "0") <= 0) return null;
                  return (
                    <Tooltip label={`Update to v${listing.version}`}>
                      <button onClick={() => act(() => marketApi.update(listing.id), p.id)} disabled={busy === p.id} className="rounded-lg border border-emerald-500/30 bg-emerald-500/10 p-1.5 text-emerald-300 hover:bg-emerald-500/20 disabled:opacity-40"><Download className="h-3.5 w-3.5" /></button>
                    </Tooltip>
                  );
                })()}
                <Tooltip label="Publish to marketplace"><button onClick={() => publish(p)} disabled={busy === p.id} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white disabled:opacity-40"><UploadCloud className="h-3.5 w-3.5" /></button></Tooltip>
                <Tooltip label="Logs"><button onClick={() => setLogsFor(p.id)} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-white/10 hover:text-white"><ScrollText className="h-3.5 w-3.5" /></button></Tooltip>
                <Tooltip label="Delete"><button onClick={() => remove(p)} disabled={busy === p.id} className="rounded-lg border border-white/10 bg-white/[0.03] p-1.5 text-white/55 hover:bg-red-500/30 hover:text-red-200 disabled:opacity-40"><Trash2 className="h-3.5 w-3.5" /></button></Tooltip>
              </div>
            </div>
          ))}
        </div>
      )}
      </>
      )}

      {creating && <CreateModal runtimes={runtimes} onClose={() => setCreating(false)} onCreated={() => { setCreating(false); load(); }} />}
      {logsFor && <LogsModal id={logsFor} onClose={() => setLogsFor(null)} />}
      {openFor && <PluginWindow plugin={openFor} onClose={() => setOpenFor(null)} />}
    </AppShell>
  );
}

function CreateModal({ runtimes, onClose, onCreated }: { runtimes: PluginRuntime[]; onClose: () => void; onCreated: () => void }) {
  const [name, setName] = useState("");
  const [runtime, setRuntime] = useState("static");
  const [starter, setStarter] = useState(true);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<{ id: string; path: string } | null>(null);
  const [icon, setIcon] = useState<File | null>(null);
  const [iconPreview, setIconPreview] = useState("");
  const id = name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 48);
  const available = (r: string) => r === "static" || runtimes.find((x) => x.id === r)?.available;

  const pickIcon = (f: File | null) => {
    setIcon(f);
    setIconPreview(f ? URL.createObjectURL(f) : "");
  };

  const submit = async () => {
    if (!id) { setErr("Enter a name"); return; }
    setErr("");
    setBusy(true);
    try {
      const r = await pluginsApi.create(id, name.trim(), runtime, starter);
      if (icon) await pluginsApi.uploadIcon(id, icon).catch(() => {});
      setDone({ id, path: r.path });
      notifyPluginsChanged(); // show the new plugin app immediately
    } catch (e) {
      setErr(e instanceof Error ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/50 p-4 backdrop-blur-sm" onClick={() => { onCreated(); }}>
        <div className="w-full max-w-sm rounded-2xl border border-white/10 bg-[#11131a] p-4 shadow-2xl" onClick={(e) => e.stopPropagation()}>
          <p className="mb-2 text-sm font-semibold text-white">Plugin created</p>
          <p className="mb-2 text-xs text-white/55">Code it in your own IDE. Your plugin lives at:</p>
          <div className="mb-3 break-all rounded-lg border border-white/10 bg-black/30 px-3 py-2 font-mono text-[10px] text-white/70">{done.path}</div>
          <p className="mb-3 text-[11px] text-white/40">Structure it however you like (multi-file, modules). Your entry serves the UI + endpoints on <code className="rounded bg-white/10 px-1">$PORT</code>. Static plugins just need <code className="rounded bg-white/10 px-1">public/index.html</code>.</p>
          <div className="flex gap-2">
            <button onClick={() => pluginsApi.reveal(done.id).catch(() => {})} className="flex flex-1 items-center justify-center gap-1.5 rounded-lg border border-white/10 px-4 py-2 text-sm text-white/80 hover:bg-white/5"><FolderOpen className="h-4 w-4" /> Open folder</button>
            <button onClick={onCreated} className="rounded-lg bg-white px-4 py-2 text-sm font-medium text-black hover:opacity-90">Done</button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/50 p-4 backdrop-blur-sm" onClick={onClose}>
      <div className="w-full max-w-sm rounded-2xl border border-white/10 bg-[#11131a] p-4 shadow-2xl" onClick={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center justify-between">
          <p className="text-sm font-semibold text-white">New plugin</p>
          <button onClick={onClose} className="rounded p-1 text-white/40 hover:bg-white/10 hover:text-white"><X className="h-4 w-4" /></button>
        </div>
        <label className="mb-1 block text-[11px] text-white/50">Name</label>
        <input value={name} onChange={(e) => setName(e.target.value)} autoFocus placeholder="My Plugin" className="mb-1 w-full rounded-lg border border-white/10 bg-black/30 px-3 py-2 text-sm text-white outline-none focus:border-white/25" />
        {id && <p className="mb-3 font-mono text-[10px] text-white/35">id: {id}</p>}
        <label className="mb-1 block text-[11px] text-white/50">Runtime</label>
        <div className="mb-3 grid grid-cols-2 gap-1.5">
          {RUNTIMES.map((r) => (
            <button key={r.id} onClick={() => setRuntime(r.id)} disabled={!available(r.id)} className={`rounded-lg border px-2.5 py-1.5 text-xs disabled:opacity-40 ${runtime === r.id ? "border-white/30 bg-white/10 text-white" : "border-white/10 text-white/60 hover:bg-white/5"}`}>
              {r.label}{!available(r.id) && r.id !== "static" ? " (missing)" : ""}
            </button>
          ))}
        </div>
        <label className="mb-1 block text-[11px] text-white/50">Icon (optional)</label>
        <div className="mb-3 flex items-center gap-2.5">
          <label className="flex h-12 w-12 shrink-0 cursor-pointer items-center justify-center overflow-hidden rounded-xl border border-dashed border-white/15 bg-white/[0.03] hover:border-white/30">
            {iconPreview ? <img src={iconPreview} alt="" className="h-full w-full object-cover" /> : <Plus className="h-4 w-4 text-white/40" />}
            <input type="file" accept="image/png,image/jpeg,image/webp,image/svg+xml" className="hidden" onChange={(e) => pickIcon(e.target.files?.[0] ?? null)} />
          </label>
          <span className="text-[11px] text-white/40">PNG/JPG/WebP/SVG. It's auto-fit to the app icon.{icon && <button onClick={() => pickIcon(null)} className="ml-2 text-white/50 underline">remove</button>}</span>
        </div>
        <label className="mb-3 flex cursor-pointer items-center gap-2 text-xs text-white/60">
          <input type="checkbox" checked={starter} onChange={(e) => setStarter(e.target.checked)} className="accent-indigo-500" />
          Include a starter example (uncheck for an empty folder + manifest only)
        </label>
        {err && <div className="mb-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">{err}</div>}
        <button onClick={submit} disabled={busy || !id} className="w-full rounded-lg bg-white px-4 py-2 text-sm font-medium text-black hover:opacity-90 disabled:opacity-50">{busy ? "Creating…" : "Create plugin"}</button>
      </div>
    </div>
  );
}

function LogsModal({ id, onClose }: { id: string; onClose: () => void }) {
  const [lines, setLines] = useState<string[]>([]);
  const timer = useRef<number | null>(null);
  useEffect(() => {
    const poll = () => pluginsApi.logs(id).then((r) => setLines(r.lines ?? [])).catch(() => {});
    poll();
    timer.current = window.setInterval(poll, 1500);
    return () => { if (timer.current) window.clearInterval(timer.current); };
  }, [id]);
  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/50 p-4 backdrop-blur-sm" onClick={onClose}>
      <div className="flex h-[70%] w-full max-w-lg flex-col rounded-2xl border border-white/10 bg-[#11131a] shadow-2xl" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-white/5 px-4 py-3">
          <p className="text-sm font-semibold text-white">Logs · <span className="font-mono text-white/50">{id}</span></p>
          <button onClick={onClose} className="rounded p-1 text-white/40 hover:bg-white/10 hover:text-white"><X className="h-4 w-4" /></button>
        </div>
        <pre className="min-h-0 flex-1 overflow-auto p-3 font-mono text-[11px] leading-relaxed text-white/70">{lines.length ? lines.join("\n") : "No output yet. Start the plugin to see logs."}</pre>
      </div>
    </div>
  );
}

function PluginWindow({ plugin, onClose }: { plugin: PluginManifest; onClose: () => void }) {
  return (
    <div className="absolute inset-0 z-50 flex flex-col bg-black/70 backdrop-blur-sm">
      <div className="flex items-center gap-2 border-b border-white/10 bg-[#11131a] px-3 py-2">
        <Puzzle className="h-4 w-4 text-white/60" />
        <span className="text-sm font-medium text-white">{plugin.name}</span>
        <button onClick={onClose} className="ml-auto rounded p-1 text-white/40 hover:bg-white/10 hover:text-white"><X className="h-4 w-4" /></button>
      </div>
      <iframe
        title={plugin.name}
        src={`/plugins/${plugin.id}/`}
        className="min-h-0 flex-1 bg-[#0b0d12]"
        sandbox="allow-scripts allow-forms allow-same-origin allow-popups"
      />
    </div>
  );
}

// Marketplace browses published plugins and installs/updates them locally.
function Marketplace({ onInstalled, installedVersions }: { onInstalled: () => void; installedVersions: Map<string, string> }) {
  const profile = useProfile();
  const [items, setItems] = useState<MarketPlugin[]>([]);
  const [q, setQ] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

  const load = async (query = "") => {
    setLoading(true);
    try {
      const r = await marketApi.list(query);
      setItems(r?.plugins ?? []);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to load (are you signed in?)");
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => { if (profile.loggedIn) load(); }, [profile.loggedIn]);

  // Live: a newly published plugin shows up without a manual refresh.
  useEffect(() => subscribeLive(["plugin_published"], () => load()), []);

  if (!profile.loading && !profile.loggedIn) {
    return <SignInGate reason="Sign in to browse plugins" />;
  }

  const install = async (p: MarketPlugin) => {
    setBusy(p.id);
    setError("");
    try {
      const r = await marketApi.install(p.id);
      setNote(`Installed ${p.name}. Find it in My Plugins.`);
      onInstalled();
      notifyPluginsChanged(); // surface the installed plugin app immediately
      void r;
    } catch (e) {
      const msg = e instanceof Error ? e.message : "install failed";
      if (msg.includes("already exists")) setNote(`${p.name} is already installed.`);
      else setError(msg);
    } finally {
      setBusy(null);
    }
  };

  const update = async (p: MarketPlugin) => {
    setBusy(p.id);
    setError("");
    try {
      await marketApi.update(p.id);
      setNote(`Updated ${p.name} to v${p.version}.`);
      onInstalled();
    } catch (e) {
      setError(e instanceof Error ? e.message : "update failed");
    } finally {
      setBusy(null);
    }
  };

  return (
    <div>
      <div className="mb-3 flex items-center gap-2">
        <input value={q} onChange={(e) => setQ(e.target.value)} onKeyDown={(e) => e.key === "Enter" && load(q)} placeholder="Search plugins…" className="flex-1 rounded-lg border border-white/10 bg-black/30 px-3 py-1.5 text-xs text-white outline-none focus:border-white/25" />
        <button onClick={() => load(q)} className="rounded-lg border border-white/10 p-1.5 text-white/50 hover:bg-white/5 hover:text-white"><RefreshCw className="h-3.5 w-3.5" /></button>
      </div>
      {error && <div className="mb-3 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">{error}</div>}
      {note && <div className="mb-3 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-3 py-2 text-xs text-emerald-300">{note}</div>}
      {loading ? (
        <div className="flex justify-center py-10"><Loader2 className="h-5 w-5 animate-spin text-white/40" /></div>
      ) : items.length === 0 ? (
        <Empty message="No published plugins yet. Publish one from My Plugins." />
      ) : (
        <div className="space-y-2">
          {items.map((p) => (
            <div key={p.id} className="flex items-center gap-3 rounded-xl border border-white/10 bg-white/[0.02] p-3">
              <div className="flex h-9 w-9 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-white/5 text-white/60">
                {p.icon_url ? <img src={p.icon_url} alt="" className="h-full w-full object-cover" /> : <Puzzle className="h-4 w-4" />}
              </div>
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5">
                  <span className="truncate text-sm font-medium text-white">{p.name}</span>
                  <span className="rounded bg-white/10 px-1 text-[9px] uppercase text-white/50">{p.runtime}</span>
                </div>
                <div className="mt-0.5 truncate text-[11px] text-white/45">{p.description || "No description"}</div>
                <div className="mt-0.5 text-[10px] text-white/30">by {p.display_name || p.username} · {p.install_count} installs</div>
              </div>
              {(() => {
                const installed = installedVersions.get(p.slug);
                if (installed === undefined) {
                  return (
                    <button onClick={() => install(p)} disabled={busy === p.id} className="flex shrink-0 items-center gap-1.5 rounded-lg border border-white/10 bg-white/[0.03] px-3 py-1.5 text-xs text-white/70 hover:bg-white/10 disabled:opacity-40">
                      {busy === p.id ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Download className="h-3.5 w-3.5" />} Install
                    </button>
                  );
                }
                if (compareVersions(p.version, installed) > 0) {
                  return (
                    <button onClick={() => update(p)} disabled={busy === p.id} className="flex shrink-0 items-center gap-1.5 rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-3 py-1.5 text-xs text-emerald-300 hover:bg-emerald-500/20 disabled:opacity-40">
                      {busy === p.id ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Download className="h-3.5 w-3.5" />} Update
                    </button>
                  );
                }
                return <span className="shrink-0 rounded-lg border border-white/5 bg-white/[0.02] px-3 py-1.5 text-xs text-white/35">Installed</span>;
              })()}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
