package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPlaybackActiveStreamRenewsSessionAndPreservesFlush(t *testing.T) {
	app, session := historyFixtureApp(t)
	session.expires = time.Now().Add(-time.Second)
	recorder := httptest.NewRecorder()
	writer := &playbackActivityWriter{ResponseWriter: recorder, app: app, run: &playbackRunContext{id: session.id, session: session, run: session.run, ctx: context.Background()}}
	if _, err := writer.Write([]byte("synthetic media bytes")); err != nil {
		t.Fatal(err)
	}
	if err := http.NewResponseController(writer).Flush(); err != nil || !recorder.Flushed {
		t.Fatal("streaming flush was lost", err)
	}
	app.expirePlayback(session.id)
	if _, exists := app.playbackStatus(session.id, false); !exists || time.Until(session.expires) < 9*time.Minute {
		t.Fatal("an active stream expired without a browser timer")
	}
}

func TestPlaybackOldOrCanceledStreamCannotRenewSession(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		app, session := historyFixtureApp(t)
		before := time.Now().Add(time.Minute)
		session.expires = before
		ctx, cancel := context.WithCancel(context.Background())
		run := &playbackRunContext{id: session.id, session: session, run: session.run, ctx: ctx}
		if stopped {
			cancel()
		} else {
			session.run++
		}
		writer := &playbackActivityWriter{ResponseWriter: httptest.NewRecorder(), app: app, run: run}
		_, _ = writer.Write([]byte("late bytes"))
		cancel()
		if !session.expires.Equal(before) {
			t.Fatal("superseded stream extended the current session")
		}
	}
}

func TestPlaybackValidProgressKeepsSessionAliveAndInvalidProgressDoesNot(t *testing.T) {
	app, session := historyFixtureApp(t)
	before := time.Now().Add(time.Minute)
	session.expires = before
	invalid := historyProgress(1, 20)
	invalid.Run = 99
	if _, _, err := app.recordPlaybackProgress(session.viewer, session.id, invalid); err != nil {
		t.Fatal(err)
	}
	if !session.expires.Equal(before) {
		t.Fatal("unknown playback progress extended the session")
	}
	if _, saved, err := app.recordPlaybackProgress(session.viewer, session.id, historyProgress(2, 24)); err != nil || !saved {
		t.Fatal(saved, err)
	}
	if time.Until(session.expires) < 9*time.Minute {
		t.Fatal("valid viewing progress did not renew the session")
	}
}

func TestPlaybackIdleExpiryStillReleasesStreamAndSlot(t *testing.T) {
	app, session := historyFixtureApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.expires = time.Now().Add(-time.Second)
	app.expirePlayback(session.id)
	if ctx.Err() == nil {
		t.Fatal("idle expiry left the media process alive")
	}
	if _, exists := app.playbackStatus(session.id, false); exists {
		t.Fatal("idle expiry retained a playback slot")
	}
	writer := historyJSONRequest(t, app, app.handlePlaybackControl, "/api/ui/playback/control", map[string]string{"session": session.id, "action": "heartbeat"})
	if writer.Code != http.StatusGone {
		t.Fatal("expired sessions must be recoverable by the client", writer.Code)
	}
}

func TestPlaybackSessionCapCanBeDisabled(t *testing.T) {
	app, _ := historyFixtureApp(t)
	zero := defaultPlaybackSettings()
	zero.MaxSessions = 0
	if err := zero.validate(); err != nil {
		t.Fatal("maxSessions=0 should be a valid setting", err)
	}
	zero.MaxSessions = -1
	if err := zero.validate(); err == nil {
		t.Fatal("negative session cap should be rejected")
	}
	resources := app.mediaResources()
	resources.settings.MaxSessions = 0
	app.playbackMu.Lock()
	disabled := !app.playbackSessionFullLocked()
	app.playbackMu.Unlock()
	if !disabled {
		t.Fatal("maxSessions=0 should disable the concurrent playback cap")
	}
	resources.settings.MaxSessions = 1
	app.playbackMu.Lock()
	reached := app.playbackSessionFullLocked()
	app.playbackMu.Unlock()
	if !reached {
		t.Fatal("maxSessions=1 should be reached by the existing session")
	}
}

func TestPlaybackReclaimsOwnIdleSessionAtCap(t *testing.T) {
	app, active := historyFixtureApp(t)
	app.mediaResources().settings.MaxSessions = 1
	stale := &playbackSession{id: "stale-tab", viewer: active.viewer, dramaID: active.dramaID,
		openedAt: time.Now().Add(-20 * time.Minute), state: "ready", expires: time.Now().Add(playbackIdleTimeout)}
	stale.viewer.retain()
	stale.timer = time.AfterFunc(time.Hour, func() {})
	app.playbackMu.Lock()
	app.playbacks[stale.id] = stale
	app.playbackMu.Unlock()
	app.playbackMu.Lock()
	reclaimed := app.reclaimPlaybackSlotLocked(active.viewer)
	app.playbackMu.Unlock()
	if reclaimed == nil || reclaimed.id != stale.id {
		t.Fatal("the oldest idle session of the same viewer should be reclaimed")
	}
	app.finishPlaybackSession(reclaimed)
	app.playbackMu.Lock()
	_, activeLeft := app.playbacks[active.id]
	_, staleLeft := app.playbacks[stale.id]
	app.playbackMu.Unlock()
	if !activeLeft || staleLeft {
		t.Fatal("reclaim must keep the current session and drop the stale one", activeLeft, staleLeft)
	}
}

func TestPlaybackCleanupClearsAllSessions(t *testing.T) {
	app, session := historyFixtureApp(t)
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/ui/admin/playback/cleanup", nil)
	app.handlePlaybackCleanup(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatal("cleanup failed", writer.Code, writer.Body.String())
	}
	var result struct {
		Cleared int `json:"cleared"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &result); err != nil || result.Cleared != 1 {
		t.Fatal("cleanup count mismatch", err, writer.Body.String())
	}
	if _, exists := app.playbackStatus(session.id, false); exists {
		t.Fatal("cleanup left a playback session behind")
	}
}

func TestPlaybackCleanupRouteIsAdminOnly(t *testing.T) {
	app, admin := administratorFixture(t)
	app.playbackMu.Lock()
	if app.playbacks == nil {
		app.playbacks = make(map[string]*playbackSession)
	}
	app.playbacks["stale-tab"] = &playbackSession{id: "stale-tab", state: "ready", expires: time.Now().Add(playbackIdleTimeout)}
	app.playbackMu.Unlock()
	guest := newAccountTestBrowser(t, app)
	if result := guest.request(t, http.MethodPost, "/api/ui/admin/playback/cleanup", map[string]any{}); result.Code != http.StatusForbidden {
		t.Fatal("guest cleared playback sessions", result.Code, result.Body.String())
	}
	response := admin.request(t, http.MethodPost, "/api/ui/admin/playback/cleanup", map[string]any{})
	if response.Code != http.StatusOK {
		t.Fatal("admin cleanup failed", response.Code, response.Body.String())
	}
	app.playbackMu.Lock()
	remaining := len(app.playbacks)
	app.playbackMu.Unlock()
	if remaining != 0 {
		t.Fatal("cleanup route left sessions behind", remaining)
	}
}
