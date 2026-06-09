package runtime

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 决策三 SSE: writeSSEEvent + sseHub 单测.
//
// 真正的 SSE 长连接 e2e (建连 + replay + 30s keepalive + 60s TTL close)
// 走本地 stack smoke (§V), 因为 fleet 目前没 sqlmock+httptest 测试 harness
// (见 owner_regression_test.go 头部论证). 这里只覆盖 pure-Go 单元.

func TestWriteSSEEvent_WireFormat(t *testing.T) {
	var buf bytes.Buffer
	// rc=nil — test 直接调 writeSSEEvent 不必造 ResponseController, helper 内
	// 部 rc != nil 才 set deadline. production sseEvents handler 总传非 nil.
	if err := writeSSEEvent(&buf, nil, 42, "ping", `{"ping_id":"ping_123"}`); err != nil {
		t.Fatalf("writeSSEEvent: %v", err)
	}
	got := buf.String()
	want := "event: ping\nid: 42\ndata: {\"ping_id\":\"ping_123\"}\n\n"
	if got != want {
		t.Errorf("wire format mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestWriteSSEEvent_AllEventTypes(t *testing.T) {
	cases := []struct {
		eventType string
	}{
		{eventTypePing},
		{eventTypeUpgrade},
		{eventTypeBotProvision},
		{eventTypeManagedBotsChanged},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeSSEEvent(&buf, nil, 1, tc.eventType, `{}`); err != nil {
				t.Fatalf("writeSSEEvent: %v", err)
			}
			out := buf.String()
			if !strings.HasPrefix(out, "event: "+tc.eventType+"\n") {
				t.Errorf("expected event line prefix for %s, got %q", tc.eventType, out)
			}
			if !strings.HasSuffix(out, "\n\n") {
				t.Errorf("expected SSE frame terminator \\n\\n for %s, got %q", tc.eventType, out)
			}
		})
	}
}

func TestSseHub_RegisterPublishDeliver(t *testing.T) {
	hub := newSseHub()
	ch, cleanup := hub.register(101)
	defer cleanup()

	ev := eventEnvelope{ID: 1, Type: eventTypePing, PayloadJSON: `{"ping_id":"p"}`}
	hub.publish(101, ev)

	select {
	case got := <-ch:
		if got.ID != 1 || got.Type != eventTypePing {
			t.Errorf("delivered envelope mismatch: %+v", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("publish did not deliver to registered channel within 100ms")
	}
}

func TestSseHub_PublishUnregistered_NoOp(t *testing.T) {
	// 防 publish 到没注册的 runtime panic / 阻塞.
	hub := newSseHub()
	hub.publish(999, eventEnvelope{ID: 1, Type: eventTypePing})
	// 走到这里就 pass — 不应 panic / 阻塞.
}

func TestSseHub_PublishChannelFull_NonBlocking(t *testing.T) {
	// channel buffer 满了之后 publish 必须 non-blocking 直接 drop —
	// daemon 重连走 Last-Event-ID replay 补.
	hub := newSseHub()
	ch, cleanup := hub.register(202)
	defer cleanup()

	// 灌满 channel (sseChannelBuffer=16)
	for i := 0; i < sseChannelBuffer; i++ {
		hub.publish(202, eventEnvelope{ID: int64(i)})
	}

	// 多发一条应该不阻塞 (起 goroutine 测 deadline)
	done := make(chan struct{})
	go func() {
		hub.publish(202, eventEnvelope{ID: 9999})
		close(done)
	}()
	select {
	case <-done:
		// good — publish returned without blocking
	case <-time.After(50 * time.Millisecond):
		t.Error("publish on full channel blocked beyond 50ms — must be non-blocking")
	}

	// drain to keep channel reference live until cleanup
	for i := 0; i < sseChannelBuffer; i++ {
		<-ch
	}
}

func TestSseHub_CleanupDoesNotRemoveNewerRegistration(t *testing.T) {
	// 防 stale cleanup func 误删后注册的新 channel — 这是 plan v6 §3
	// 提到的 "load+compare in cleanup" 防御.
	hub := newSseHub()
	_, cleanup1 := hub.register(303)
	ch2, cleanup2 := hub.register(303) // 覆盖, 新 channel
	defer cleanup2()

	// 老 cleanup 不应该把新 channel 删掉.
	cleanup1()

	// 新 channel 仍能收 publish.
	hub.publish(303, eventEnvelope{ID: 7, Type: eventTypeUpgrade})
	select {
	case got := <-ch2:
		if got.ID != 7 {
			t.Errorf("expected new channel to still receive, got %+v", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("stale cleanup1 wrongly removed new channel — publish lost")
	}
}

func TestSseHub_ConcurrentRegisterPublish(t *testing.T) {
	// 并发 register + publish 不能 race panic. 跑 -race 才能真正捕获.
	hub := newSseHub()
	var wg sync.WaitGroup
	var received int64

	for runtimeID := int64(1); runtimeID <= 10; runtimeID++ {
		wg.Add(1)
		go func(rid int64) {
			defer wg.Done()
			ch, cleanup := hub.register(rid)
			defer cleanup()
			deadline := time.After(200 * time.Millisecond)
			for {
				select {
				case <-ch:
					atomic.AddInt64(&received, 1)
				case <-deadline:
					return
				}
			}
		}(runtimeID)
	}

	// publisher
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			for rid := int64(1); rid <= 10; rid++ {
				hub.publish(rid, eventEnvelope{ID: int64(i), Type: eventTypePing})
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	if atomic.LoadInt64(&received) == 0 {
		t.Error("no events received under concurrent register+publish")
	}
}
