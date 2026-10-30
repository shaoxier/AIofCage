package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/policy"
)

func TestEventStreamStopsAtUnacknowledgedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "old-run:7" {
			t.Errorf("续传游标被改写：%q", r.Header.Get("Last-Event-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for i := 8; i <= 10; i++ {
			data, _ := json.Marshal(audit.Event{ID: fmt.Sprintf("old-run:%d", i), Action: audit.ActionRequest})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer server.Close()
	client := &gateClient{base: server.URL, http: server.Client()}
	wantErr := errors.New("synthetic disk failure")
	var received []string
	err := client.events(context.Background(), "old-run:7", func(event audit.Event) error {
		received = append(received, event.ID)
		if event.ID == "old-run:9" {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(received, []string{"old-run:8", "old-run:9"}) {
		t.Fatalf("未确认后仍投递后续事件：%v %v", received, err)
	}
}

func TestEventStreamDoesNotSkipInvalidRecords(t *testing.T) {
	for _, invalid := range []string{`{"id":`, `{"id":"run:2"}`, `{"action":"net.request"}`} {
		t.Run(invalid, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, ": heartbeat\n\ndata: %s\n\ndata: %s\n\n", invalid, `{"id":"run:3","action":"net.request"}`)
			}))
			defer server.Close()
			client := &gateClient{base: server.URL, http: server.Client()}
			called := false
			err := client.events(context.Background(), "run:1", func(audit.Event) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("无效事件被静默跳过：called=%v err=%v", called, err)
			}
		})
	}
}

func TestEventStreamRequiresSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{}`)
	}))
	defer server.Close()
	client := &gateClient{base: server.URL, http: server.Client()}
	if err := client.events(context.Background(), "", func(audit.Event) error { return nil }); err == nil {
		t.Fatal("非事件响应被视为正常订阅")
	}
}

func TestEventStreamPreservesPreviousRunCursor(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	handlers, err := gatekeeper.NewHandlers(token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handlers.Management)
	defer server.Close()
	client := &gateClient{base: server.URL, token: token, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.putConfig(ctx, gatekeeper.Config{Policy: policy.Snapshot{Version: 1, SessionID: "sbx-resume"}}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("received replay")
	var received []audit.Event
	err = client.events(ctx, "previous-run:42", func(event audit.Event) error {
		received = append(received, event)
		if len(received) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || len(received) != 2 || received[0].Action != audit.ActionGap || received[0].ReasonCode != "cursor_from_other_run" || received[1].Action != audit.ActionPolicyChange {
		t.Fatalf("真实门卫未先报告跨运行缺口：%+v %v", received, err)
	}
}

func TestRuntimeSecretsCoverGateAndConnectorTokens(t *testing.T) {
	m := &Manager{gates: map[string]*gateClient{"sbx-a": newGateClient("1", "synthetic-admin-token-a")},
		connectorTokens: map[string]string{
			connectorName("sbx-a", "db"):  "synthetic-connector-token-a",
			connectorName("sbx-ab", "db"): "synthetic-connector-token-ab",
		}}
	got := map[string]bool{}
	for _, value := range m.RuntimeSecrets("sbx-a") {
		got[value] = true
	}
	if len(got) != 2 || !got["synthetic-admin-token-a"] || !got["synthetic-connector-token-a"] {
		t.Fatalf("runtime secrets of sbx-a: %v", got)
	}
}
