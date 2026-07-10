import { useEffect, useState } from "react";
import { AnimatePresence } from "framer-motion";
import { LayoutGrid, SquareTerminal, BookOpen, Grid3x3, Bot, FlaskConical, ShieldCheck, Store } from "lucide-react";
import { buildApps } from "../apps";
import { SideDock } from "./SideDock";
import { SidePanel } from "./SidePanel";
import { TopBar } from "./TopBar";
import { Widgets } from "./Widgets";
import { CenterTerminal } from "./CenterTerminal";
import { TerminalLayer } from "./TerminalLayer";
import { AppsDrawer } from "./AppsDrawer";
import { Tooltip } from "../components/Tooltip";
import { ProfileViewer } from "../apps/ProfileViewer";
import { Lightbox } from "../components/Lightbox";
import { NotifBanner } from "./NotifBanner";
import { openMarketplaceThread } from "./marketplaceNav";
import { useNotifNav, consumeNotifNav } from "./notifNav";
import { openProfile } from "./profileViewer";
import { openPost } from "./postViewer";
import { findPost } from "./postsBus";
import { useChatUnread } from "./chatBus";
import { postsApi } from "../lib/api";
import { useProfile } from "./useProfile";
import { DailyCheckinModal } from "./DailyCheckinModal";
import { DocsApp } from "../apps/DocsApp";
import { AdminApp } from "../apps/AdminApp";
import { MarketplaceApp } from "../apps/MarketplaceApp";
import { AiChatApp } from "../apps/AiChatApp";
import { ApiTestApp } from "../apps/ApiTestApp";
import { usePanels } from "./usePanels";
import { usePersisted } from "./usePersisted";
import { useAppLocations } from "./useSides";
import { useShortcuts } from "./useShortcuts";
import { useTerminals, type TermLocation } from "./useTerminals";
import { usePluginApps } from "./usePluginApps";
import { useLayoutMode } from "./useLayoutMode";
import { FocusShell } from "./FocusShell";
import type { AppId, DesktopApp, Location, Side } from "./types";

type CenterView = "widget" | "terminal" | "chat" | "apitest" | "apps" | "marketplace" | "admin" | "docs";

// CENTER_VIEWS is the single source of truth for the top-bar views and their
// leader-key shortcuts. Order here assigns the number (1..N) — no hardcoded keys,
// so nav + shortcuts never drift. `gate` restricts a view to a capability; a
// gated view a user lacks is skipped entirely (no tab, no number, no shortcut),
// which keeps numbering flexible per role (e.g. Admin only appears for mods).
type CenterViewDef = { id: CenterView; label: string; icon: typeof LayoutGrid; gate?: string };
const CENTER_VIEWS: CenterViewDef[] = [
  { id: "widget", label: "Widget", icon: LayoutGrid },
  { id: "terminal", label: "Terminal", icon: SquareTerminal },
  { id: "chat", label: "Chat", icon: Bot },
  { id: "apitest", label: "API Test", icon: FlaskConical },
  { id: "apps", label: "Apps", icon: Grid3x3 },
  { id: "marketplace", label: "Market", icon: Store },
  { id: "docs", label: "Docs", icon: BookOpen },
  { id: "admin", label: "Admin", icon: ShieldCheck, gate: "chat.moderate" },
];

// visibleViews returns the views the user may see, each with its 1-based key.
function visibleViews(has: (cap: string) => boolean): (CenterViewDef & { key: string })[] {
  return CENTER_VIEWS.filter((v) => !v.gate || has(v.gate)).map((v, i) => ({ ...v, key: String(i + 1) }));
}

export function Desktop() {
  const profile = useProfile();
  const isMod = profile.has("chat.moderate");
  const pluginApps = usePluginApps();
  const chatUnread = useChatUnread();
  // Admin is a center view (not a docked app), so it's excluded from buildApps.
  // Badge the Community (chat) icon with a red dot when there are unread messages.
  const apps = [...buildApps(), ...pluginApps].map((a) =>
    a.id === "chat" && chatUnread ? { ...a, notify: true } : a,
  );
  const { active, toggle, close } = usePanels();
  const [view, setView] = usePersisted<CenterView>("center-view", "widget");
  const [layoutMode] = useLayoutMode();
  // In Focus mode, a single app takes over full view. Persisted so it survives a
  // reload like the classic panels do.
  const [focusApp, setFocusApp] = usePersisted<AppId | null>("focus-app", null);

  const defaults = Object.fromEntries(apps.map((a) => [a.id, a.home])) as Record<AppId, Location>;
  const { locations, move } = useAppLocations(defaults);
  const term = useTerminals();

  // Per-location DOM hosts the terminal instances are portaled into.
  const [centerHost, setCenterHost] = useState<HTMLElement | null>(null);
  const [leftHost, setLeftHost] = useState<HTMLElement | null>(null);
  const [rightHost, setRightHost] = useState<HTMLElement | null>(null);
  const hosts: Record<TermLocation, HTMLElement | null> = { center: centerHost, left: leftHost, right: rightHost };

  const locationOf = (id: AppId): Location => locations[id] ?? "drawer";
  const appsOn = (side: Side) => apps.filter((a) => locationOf(a.id) === side);
  const drawerApps = apps.filter((a) => locationOf(a.id) === "drawer");
  const termsOn = (side: Side) => term.terms.filter((t) => t.location === side);
  const findApp = (id: AppId | null) => apps.find((a) => a.id === id);

  // A dock side may have an app panel open OR a terminal panel open.
  const [openTerm, setOpenTerm] = usePersisted<Record<Side, number | null>>("open-term", { left: null, right: null });
  const openTermOn = (side: Side) => term.terms.find((t) => t.location === side && t.id === openTerm[side]) ?? null;

  // Open an app from anywhere: if docked, toggle its panel; if in the drawer,
  // pin it to the left dock and open it.
  const openApp = (id: AppId) => {
    const loc = locationOf(id);
    if (loc === "drawer") {
      move(id, "left");
      toggle("left", id);
    } else {
      toggle(loc, id);
    }
  };

  // Leader-key shortcuts: 1..4 switch the center view; a letter opens an app.
  const appShortcuts: Record<string, AppId> = {
    p: "providers",
    a: "accounts",
    s: "statistics",
    g: "settings",
    f: "files",
    r: "requests",
    w: "warmup-logs",
    k: "api-keys",
    m: "music",
    t: "tunnel",
    c: "profile",
  };
  const leaderActive = useShortcuts((k) => {
    // Number keys map to the views this user can actually see (role-aware), so a
    // non-mod's Ctrl+7 never reaches Admin and numbers stay gap-free.
    const view = visibleViews(profile.has).find((v) => v.key === k);
    if (view) {
      setView(view.id);
      return;
    }
    if (appShortcuts[k]) openApp(appShortcuts[k]);
  });

  // Clicking a notification routes to its context. Desktop owns view/app state,
  // so it dispatches by ref_type: post/comment → open the post; chat → chat view;
  // rekber → marketplace deal thread; order → orders; else the actor's profile.
  const notifNav = useNotifNav();
  useEffect(() => {
    const n = consumeNotifNav();
    if (!n) return;
    (async () => {
      switch (n.ref_type) {
        case "post":
        case "comment": {
          // The post overlay only renders inside PostsApp, so open the app first,
          // then open the post (from cache or a fetch).
          openApp("posts");
          const existing = findPost(n.ref_id);
          if (existing) { openPost(existing); break; }
          try {
            const list = await postsApi.list();
            const p = list.posts.find((x) => x.id === n.ref_id);
            if (p) openPost(p);
          } catch { /* ignore */ }
          break;
        }
        case "chat":
          openApp("chat"); // community chat app (the center "chat" view is AI chat)
          break;
        case "rekber":
          setView("marketplace");
          if (n.ref_id) openMarketplaceThread(n.ref_id);
          break;
        case "order":
          setView("marketplace");
          break;
        default:
          if (n.actor_id) openProfile(n.actor_id);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [notifNav]);

  const renderPanel = (side: Side) => {
    const openT = openTermOn(side);
    if (openT) {
      return (
        <SidePanel
          side={side}
          title={openT.title}
          onClose={() => {
            term.close(openT.id);
            setOpenTerm((p) => ({ ...p, [side]: null }));
          }}
          hostRef={side === "left" ? setLeftHost : setRightHost}
        />
      );
    }
    const id = active[side];
    const app = findApp(id);
    return app && id && locationOf(id) === side ? (
      <SidePanel side={side} title={app.label} onClose={() => close(side)}>
        {app.render()}
      </SidePanel>
    ) : null;
  };

  // --- Focus mode ---------------------------------------------------------
  // Former centre views (terminal, AI chat, api test, market, docs, admin)
  // become full-view "apps" in the bottom dock. Widget stays the home board.
  // buildApps() and Classic mode are untouched.
  const viewApps: DesktopApp[] = [
    { id: "view:terminal", label: "Terminal", icon: <SquareTerminal />, accent: "from-zinc-600 to-zinc-800", home: "drawer", render: () => <CenterTerminal term={term} setHost={setCenterHost} /> },
    { id: "view:chat", label: "AI Chat", icon: <Bot />, accent: "from-teal-500 to-emerald-700", home: "drawer", render: () => <AiChatApp /> },
    { id: "view:apitest", label: "API Test", icon: <FlaskConical />, accent: "from-orange-500 to-amber-700", home: "drawer", render: () => <ApiTestApp /> },
    { id: "view:marketplace", label: "Market", icon: <Store />, accent: "from-yellow-500 to-orange-600", home: "drawer", render: () => <MarketplaceApp /> },
    { id: "view:docs", label: "Docs", icon: <BookOpen />, accent: "from-blue-500 to-indigo-700", home: "drawer", render: () => <DocsApp /> },
    ...(isMod ? [{ id: "view:admin" as AppId, label: "Admin", icon: <ShieldCheck />, accent: "from-red-500 to-rose-700", home: "drawer" as Location, render: () => <AdminApp /> }] : []),
  ];
  const openFocusApp = (id: AppId) => setFocusApp((cur) => (cur === id ? null : id));
  // In Focus there's no Workspace dock — every real app + view app is reachable
  // from the "Apps" launcher (a full-screen grid). The bottom dock shows the
  // apps pinned to a dock (left or right, set by drag-and-drop), plus an always-
  // present "Apps" button to reach everything else.
  const focusAllApps: DesktopApp[] = [...apps, ...viewApps];
  const appsLauncher: DesktopApp = {
    id: "view:apps",
    label: "Apps",
    icon: <Grid3x3 />,
    accent: "from-slate-500 to-slate-700",
    home: "drawer",
    render: () => (
      <div className="p-4">
        <p className="mb-3 text-[11px] text-white/40">Tap to open. Drag an app onto the bottom dock to pin it there.</p>
        <div className="grid gap-2" style={{ gridTemplateColumns: "repeat(auto-fill, minmax(88px, 1fr))" }}>
          {focusAllApps.map((a) => {
            // Every app (real + view) can be pinned — locations are keyed by id.
            const pinned = locationOf(a.id) === "left" || locationOf(a.id) === "right";
            return (
              <button
                key={a.id}
                onClick={() => setFocusApp(a.id)}
                draggable
                onDragStart={(e) => {
                  e.dataTransfer.setData("text/app-id", a.id);
                  // Drag image = just the icon (the first child span), not the
                  // whole bordered card + label.
                  const icon = e.currentTarget.firstElementChild as HTMLElement | null;
                  if (icon) e.dataTransfer.setDragImage(icon, icon.offsetWidth / 2, icon.offsetHeight / 2);
                }}
                className="relative flex flex-col items-center gap-1.5 rounded-xl border border-white/10 bg-white/[0.02] p-2 hover:bg-white/[0.06]"
                title="Tap to open · drag to the dock to pin"
              >
                <span className={`flex h-11 w-11 items-center justify-center rounded-xl bg-gradient-to-br text-white shadow [&>svg]:!h-5 [&>svg]:!w-5 ${a.accent}`}>{a.icon}</span>
                <span className="w-full truncate text-center text-[10px] text-white/70">{a.label}</span>
                {pinned && <span className="absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full bg-emerald-400" title="Pinned to the dock" />}
              </button>
            );
          })}
        </div>
      </div>
    ),
  };
  // The dock: the Apps launcher + every app (real or view) pinned to a dock side.
  const focusDockApps: DesktopApp[] = [
    appsLauncher,
    ...focusAllApps.filter((a) => locationOf(a.id) === "left" || locationOf(a.id) === "right"),
  ];

  if (layoutMode === "focus") {
    return (
      <div className="wallpaper fixed inset-0 select-none overflow-hidden">
        <TopBar nav={null} />
        <FocusShell
          apps={focusDockApps}
          allApps={[...focusAllApps, appsLauncher]}
          activeApp={focusApp}
          onOpenApp={openFocusApp}
          onCloseApp={() => setFocusApp(null)}
          onDropApp={(id, side) => move(id, side)}
          home={
            <div className="flex h-full flex-col px-4 pb-2 pt-3">
              <div className="min-h-0 flex-1 overflow-auto">
                <Widgets onOpen={openFocusApp} />
              </div>
            </div>
          }
        />
        <ProfileViewer />
        <Lightbox />
        <NotifBanner />
        <TerminalLayer terms={term.terms} activeCenter={term.activeCenter} hosts={hosts} />
      </div>
    );
  }

  return (
    <div className="wallpaper fixed inset-0 select-none overflow-hidden">
      <div className="pointer-events-none absolute inset-x-0 top-7 bottom-3">
        <div className="pointer-events-auto mx-auto flex h-full max-w-3xl flex-col px-5 pb-2 pt-3">
          <div className="relative min-h-0 flex-1 overflow-hidden">
            <div className={`absolute inset-0 overflow-auto ${view === "widget" ? "" : "hidden"}`}>
              <Widgets onOpen={openApp} />
            </div>
            <div className={`absolute inset-0 ${view === "terminal" ? "" : "hidden"}`}>
              <CenterTerminal term={term} setHost={setCenterHost} />
            </div>
            <div className={`absolute inset-0 ${view === "chat" ? "" : "hidden"}`}>
              <AiChatApp />
            </div>
            {view === "apitest" && (
              <div className="absolute inset-0">
                <ApiTestApp />
              </div>
            )}
            <div className={`absolute inset-0 ${view === "apps" ? "" : "hidden"}`}>
              <AppsDrawer apps={drawerApps} onOpen={openApp} onDropToDrawer={(id) => move(id, "drawer")} />
            </div>
            {view === "marketplace" && (
              <div className="absolute inset-0 overflow-hidden rounded-2xl border border-white/10 bg-[var(--window-bg)]/80">
                <MarketplaceApp />
              </div>
            )}
            {isMod && view === "admin" && (
              <div className="absolute inset-0 overflow-hidden rounded-2xl border border-white/10 bg-[var(--window-bg)]/80">
                <AdminApp />
              </div>
            )}
            {view === "docs" && (
              <div className="absolute inset-0 overflow-hidden rounded-2xl border border-white/10 bg-[var(--window-bg)]/80">
                <DocsApp />
              </div>
            )}
          </div>
        </div>
      </div>

      <TopBar nav={<CenterNav view={view} onView={setView} has={profile.has} />} />

      {/* Full-page profile overlay (opened via openProfile from anywhere). */}
      <ProfileViewer />
      {/* Image lightbox overlay (opened from any thumbnail). */}
      <Lightbox />
      {/* macOS-style notification banners (top-right). */}
      <NotifBanner />
      {/* Daily Kleos check-in — auto-pops once per day. */}
      <DailyCheckinModal />

      {leaderActive && (
        <div className="pointer-events-none fixed left-1/2 top-9 z-[10000] -translate-x-1/2 rounded-lg border border-emerald-500/30 bg-black/80 px-3 py-1.5 font-mono text-[11px] text-emerald-300 shadow-lg">
          hold + press: 1 widget · 2 terminal · 3 chat · 4 api · 5 apps · 6 docs · p a s g f r w k m t c apps
        </div>
      )}

      <SideDock
        side="left"
        apps={appsOn("left")}
        terms={termsOn("left")}
        activeId={active.left}
        openTermId={openTerm.left}
        onOpen={toggle}
        onOpenTerm={(id) => setOpenTerm((p) => ({ ...p, left: p.left === id ? null : id }))}
        onCloseTerm={term.close}
        onDropApp={(id) => move(id, "left")}
        onDropTerm={(id) => term.moveTo(id, "left")}
      />
      <SideDock
        side="right"
        apps={appsOn("right")}
        terms={termsOn("right")}
        activeId={active.right}
        openTermId={openTerm.right}
        onOpen={toggle}
        onOpenTerm={(id) => setOpenTerm((p) => ({ ...p, right: p.right === id ? null : id }))}
        onCloseTerm={term.close}
        onDropApp={(id) => move(id, "right")}
        onDropTerm={(id) => term.moveTo(id, "right")}
      />

      <AnimatePresence>{renderPanel("left")}</AnimatePresence>
      <AnimatePresence>{renderPanel("right")}</AnimatePresence>

      <TerminalLayer terms={term.terms} activeCenter={term.activeCenter} hosts={hosts} />
    </div>
  );
}

// Compact center-view switch that lives in the top bar. The leader-key hint is
// shown in each tab's tooltip (tap Ctrl/Alt then the number).
function CenterNav({ view, onView, has }: { view: CenterView; onView: (v: CenterView) => void; has: (cap: string) => boolean }) {
  const tabs = visibleViews(has);
  return (
    <div className="flex items-center gap-0.5">
      {tabs.map((t) => {
        const Icon = t.icon;
        return (
          <Tooltip key={t.id} label={`${t.label} · hold Ctrl/Alt + ${t.key}`} place="bottom">
            <button
              onClick={() => onView(t.id)}
              className={`flex items-center gap-1.5 rounded-md px-2 py-0.5 text-[11px] font-medium transition-colors ${
                view === t.id ? "bg-white/12 text-white" : "text-white/45 hover:text-white/80"
              }`}
            >
              <Icon className="h-3.5 w-3.5" />
              {t.label}
            </button>
          </Tooltip>
        );
      })}
    </div>
  );
}
