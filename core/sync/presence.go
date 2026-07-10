package sync

import (
	"context"
	stdsync "sync"
	"time"
)

// Presence: "online" means a browser dashboard tab is open, NOT that the proxy
// is running. The gateway increments a browser counter whenever a browser opens
// its live stream (Sync.ChatStream) and decrements on disconnect. While at least
// one browser is connected, we heartbeat the cloud every 30s; when the last one
// disconnects we send a final inactive ping so the user goes offline promptly.
const presenceInterval = 30 * time.Second

var (
	presenceMu    stdsync.Mutex
	browserCount  int
	presenceStop  chan struct{}
	presenceOwner *Manager
)

// BrowserConnected is called when a browser opens the live stream.
func (m *Manager) BrowserConnected() {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	browserCount++
	if browserCount == 1 {
		// First browser → start the heartbeat loop.
		presenceOwner = m
		presenceStop = make(chan struct{})
		go m.presenceLoop(presenceStop)
	}
}

// BrowserDisconnected is called when a browser closes the live stream.
func (m *Manager) BrowserDisconnected() {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	if browserCount > 0 {
		browserCount--
	}
	if browserCount == 0 && presenceStop != nil {
		close(presenceStop)
		presenceStop = nil
		// Best-effort "going offline" so the server doesn't wait for the TTL.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		go func() {
			defer cancel()
			m.Presence(ctx, false)
		}()
	}
}

func (m *Manager) presenceLoop(stop <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	m.Presence(ctx, true) // immediate first beat
	cancel()
	t := time.NewTicker(presenceInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			m.Presence(ctx, true)
			cancel()
		}
	}
}
