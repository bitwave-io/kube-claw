package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	slackapi "github.com/slack-go/slack"
)

// TestNameResolution: id→name lookups hit Slack once per id (cached, including
// failures), and a nil Notifier / empty id resolve to "" without any call.
func TestNameResolution(t *testing.T) {
	ctx := context.Background()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.info":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"pat","real_name":"Pat White","profile":{"display_name":"pat"}}}`))
		case "/conversations.info":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C1","name":"kube-claw-mgmt"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
		}
	}))
	defer srv.Close()
	n := &Notifier{api: slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))}

	if got := n.UserName(ctx, "U1"); got != "pat" {
		t.Fatalf("UserName = %q", got)
	}
	if got := n.ChannelName(ctx, "C1"); got != "kube-claw-mgmt" {
		t.Fatalf("ChannelName = %q", got)
	}
	before := atomic.LoadInt64(&calls)
	// Cached: repeat lookups add no HTTP calls.
	_ = n.UserName(ctx, "U1")
	_ = n.ChannelName(ctx, "C1")
	if atomic.LoadInt64(&calls) != before {
		t.Fatalf("cached lookups still called Slack (%d → %d)", before, calls)
	}

	// Nil notifier and empty ids are safe no-ops.
	var nilN *Notifier
	if nilN.UserName(ctx, "U1") != "" || n.UserName(ctx, "") != "" || n.ChannelName(ctx, "") != "" {
		t.Fatal("nil notifier / empty id must resolve to \"\"")
	}
	if atomic.LoadInt64(&calls) != before {
		t.Fatal("empty-id resolution must not call Slack")
	}
}
