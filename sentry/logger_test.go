package sentry

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ergo.services/ergo/gen"
	sentrygo "github.com/getsentry/sentry-go"
)

func newMockLogger(t *testing.T, opts Options) (*logger, *sentrygo.MockTransport) {
	t.Helper()
	mock := &sentrygo.MockTransport{}
	opts.transport = mock
	l, err := CreateLogger(opts)
	if err != nil {
		t.Fatalf("CreateLogger: %v", err)
	}
	return l.(*logger), mock
}

// waitEvents polls mock.Events() until it has at least n entries or the
// timeout fires. Returns the slice (possibly shorter than n on timeout).
func waitEvents(mock *sentrygo.MockTransport, n int, timeout time.Duration) []*sentrygo.Event {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev := mock.Events()
		if len(ev) >= n {
			return ev
		}
		time.Sleep(2 * time.Millisecond)
	}
	return mock.Events()
}

func TestShouldCaptureMatrix(t *testing.T) {
	cases := []struct {
		name   string
		opts   Options
		level  gen.LogLevel
		source any
		want   bool
	}{
		// Panic from every source is always captured.
		{"panic/node", Options{}, gen.LogLevelPanic, gen.MessageLogNode{}, true},
		{"panic/network", Options{}, gen.LogLevelPanic, gen.MessageLogNetwork{}, true},
		{"panic/application", Options{}, gen.LogLevelPanic, gen.MessageLogApplication{}, true},
		{"panic/meta", Options{}, gen.LogLevelPanic, gen.MessageLogMeta{}, true},
		{"panic/process", Options{}, gen.LogLevelPanic, gen.MessageLogProcess{}, true},

		// Error from node/network/application always.
		{"error/node", Options{}, gen.LogLevelError, gen.MessageLogNode{}, true},
		{"error/network", Options{}, gen.LogLevelError, gen.MessageLogNetwork{}, true},
		{"error/application", Options{}, gen.LogLevelError, gen.MessageLogApplication{}, true},

		// Error from meta is opt-in.
		{"error/meta/off", Options{}, gen.LogLevelError, gen.MessageLogMeta{}, false},
		{"error/meta/on", Options{CaptureMetaErrors: true}, gen.LogLevelError, gen.MessageLogMeta{}, true},

		// Error from process is opt-in.
		{"error/process/off", Options{}, gen.LogLevelError, gen.MessageLogProcess{}, false},
		{"error/process/on", Options{CaptureProcessErrors: true}, gen.LogLevelError, gen.MessageLogProcess{}, true},

		// Quieter levels never captured.
		{"warning/node", Options{}, gen.LogLevelWarning, gen.MessageLogNode{}, false},
		{"info/node", Options{}, gen.LogLevelInfo, gen.MessageLogNode{}, false},
		{"debug/node", Options{}, gen.LogLevelDebug, gen.MessageLogNode{}, false},
		{"trace/node", Options{}, gen.LogLevelTrace, gen.MessageLogNode{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newMockLogger(t, tc.opts)
			got := l.shouldCapture(gen.MessageLog{Level: tc.level, Source: tc.source})
			if got != tc.want {
				t.Fatalf("shouldCapture: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogSendsToTransport(t *testing.T) {
	l, mock := newMockLogger(t, Options{})

	l.Log(gen.MessageLog{
		Time:   time.Now(),
		Level:  gen.LogLevelError,
		Source: gen.MessageLogNode{Node: "n1@host", Creation: 1},
		Format: "boom",
	})

	events := waitEvents(mock, 1, time.Second)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Level != sentrygo.LevelError {
		t.Fatalf("level: got %s, want error", ev.Level)
	}
	if ev.Tags["source"] != "node" || ev.Tags["node"] != "n1@host" {
		t.Fatalf("tags: %v", ev.Tags)
	}
	if ev.Message != "boom" {
		t.Fatalf("message: got %q", ev.Message)
	}
	if ev.Exception != nil {
		t.Fatalf("non-panic event must not carry Exception, got %v", ev.Exception)
	}
}

func TestPanicEventCarriesStack(t *testing.T) {
	l, mock := newMockLogger(t, Options{SkipFrames: 1})

	// Trigger a real panic so runtime.Callers walks an actual unwinding
	// stack. The recover handler calls Log() while the panic frames are
	// still on the goroutine.
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.Log(gen.MessageLog{
					Time:   time.Now(),
					Level:  gen.LogLevelPanic,
					Source: gen.MessageLogProcess{Node: "n@h"},
					Format: "panic: %v",
					Args:   []any{r},
				})
			}
		}()
		causeThePanicHere()
	}()

	events := waitEvents(mock, 1, time.Second)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if len(ev.Exception) != 1 {
		t.Fatalf("expected Exception, got %v", ev.Exception)
	}
	exc := ev.Exception[0]
	if exc.Type != "panic" {
		t.Fatalf("Exception.Type: got %q, want panic", exc.Type)
	}
	if exc.Stacktrace == nil || len(exc.Stacktrace.Frames) == 0 {
		t.Fatalf("missing Stacktrace.Frames")
	}
	// The panicking function name must appear somewhere in the frames.
	found := false
	for _, f := range exc.Stacktrace.Frames {
		if strings.Contains(f.Function, "causeThePanicHere") {
			found = true
			break
		}
	}
	if found == false {
		t.Fatalf("panic origin frame causeThePanicHere not in stack: %+v",
			exc.Stacktrace.Frames)
	}
}

// causeThePanicHere is a separate top-level function so the test can
// assert by function name that it shows up in the captured stack.
func causeThePanicHere() {
	panic("intentional test panic")
}

func TestConcurrentLogProducers(t *testing.T) {
	l, mock := newMockLogger(t, Options{QueueLimit: 1024})

	const goroutines = 16
	const perG = 32
	want := goroutines * perG

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				l.Log(gen.MessageLog{
					Time:   time.Now(),
					Level:  gen.LogLevelError,
					Source: gen.MessageLogNode{Node: "n@h"},
					Format: "x",
				})
			}
		}()
	}
	wg.Wait()

	events := waitEvents(mock, want, 2*time.Second)
	if len(events) != want {
		t.Fatalf("events: got %d, want %d", len(events), want)
	}
}

func TestQueueLimitDropsExtras(t *testing.T) {
	// Block sends inside the worker via BeforeSend so we can fill the
	// queue past its limit with events that never get drained out.
	gate := make(chan struct{})
	release := make(chan struct{})
	opts := Options{
		QueueLimit: 2,
		BeforeSend: func(ev *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
			select {
			case gate <- struct{}{}:
			default:
			}
			<-release
			return ev
		},
	}
	l, mock := newMockLogger(t, opts)

	// Push 1: starts the worker. Wait for the worker to actually pop the
	// event and park in BeforeSend so the queue is provably empty before
	// the next pushes (otherwise the tight loop races the worker's first
	// Pop and a different number of events get dropped).
	l.Log(gen.MessageLog{
		Time:   time.Now(),
		Level:  gen.LogLevelError,
		Source: gen.MessageLogNode{Node: "n@h"},
		Format: "x",
	})
	select {
	case <-gate:
	case <-time.After(time.Second):
		t.Fatal("worker never reached BeforeSend")
	}

	// Now the queue is empty and the worker is parked. Pushes 2 and 3
	// fill the queue (cap = 2). Pushes 4 and 5 hit the limit and drop.
	for i := 0; i < 4; i++ {
		l.Log(gen.MessageLog{
			Time:   time.Now(),
			Level:  gen.LogLevelError,
			Source: gen.MessageLogNode{Node: "n@h"},
			Format: "x",
		})
	}
	// Release all blocking calls. Subsequent send()s also call BeforeSend;
	// drain by spinning release until no more sends happen.
	go func() {
		for {
			release <- struct{}{}
		}
	}()

	events := waitEvents(mock, 3, 2*time.Second)
	if len(events) != 3 {
		t.Fatalf("expected 3 events (1 in-flight + 2 queued, 2 dropped); got %d", len(events))
	}
}

func TestTerminateStopsAcceptingLogs(t *testing.T) {
	l, mock := newMockLogger(t, Options{})

	l.Log(gen.MessageLog{
		Time:   time.Now(),
		Level:  gen.LogLevelError,
		Source: gen.MessageLogNode{Node: "n@h"},
		Format: "before",
	})
	waitEvents(mock, 1, time.Second)

	l.Terminate()

	// Post-Terminate Log() must be a no-op.
	l.Log(gen.MessageLog{
		Time:   time.Now(),
		Level:  gen.LogLevelError,
		Source: gen.MessageLogNode{Node: "n@h"},
		Format: "after",
	})
	// Give a beat for any (incorrect) goroutine to deliver.
	time.Sleep(50 * time.Millisecond)
	if got := len(mock.Events()); got != 1 {
		t.Fatalf("Terminate did not stop new logs: got %d events, want 1", got)
	}
	if got := atomic.LoadInt32(&l.state); got != stateTerminated {
		t.Fatalf("state after Terminate: got %d, want %d", got, stateTerminated)
	}
}

func TestSourceTagsPopulated(t *testing.T) {
	cases := []struct {
		name   string
		source any
		want   map[string]string
	}{
		{
			"network",
			gen.MessageLogNetwork{Node: "a@h", Peer: "b@h", Creation: 1},
			map[string]string{"source": "network", "node": "a@h", "peer": "b@h"},
		},
		{
			"application",
			gen.MessageLogApplication{
				Node: "a@h", Name: "myapp", Mode: gen.ApplicationModePermanent,
			},
			map[string]string{
				"source": "application", "node": "a@h",
				"app": "myapp", "mode": "permanent",
			},
		},
		{
			"meta",
			gen.MessageLogMeta{
				Node: "a@h",
				Meta: gen.Alias{Node: "a@h", ID: [3]uint64{1, 2, 3}},
			},
			map[string]string{"source": "meta", "node": "a@h"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// CaptureMetaErrors covers meta-source; the rest pass with
			// the default config. Setting it here is harmless for those.
			l, mock := newMockLogger(t, Options{CaptureMetaErrors: true})
			l.Log(gen.MessageLog{
				Time:   time.Now(),
				Level:  gen.LogLevelError,
				Source: tc.source,
				Format: "x",
			})
			events := waitEvents(mock, 1, time.Second)
			if len(events) != 1 {
				t.Fatalf("no event")
			}
			for k, v := range tc.want {
				if got := events[0].Tags[k]; got != v {
					t.Fatalf("tag %q: got %q, want %q (all=%v)",
						k, got, v, events[0].Tags)
				}
			}
		})
	}
}

func TestIsInAppFilter(t *testing.T) {
	cases := []struct {
		file, fn string
		want     bool
	}{
		{"/go/src/runtime/panic.go", "runtime.gopanic", false},
		{"/home/x/code/main.go", "runtime.Callers", false},
		{"/home/x/code/main.go", "main.userPanic", true},
		{"/go/pkg/mod/github.com/getsentry/sentry-go/client.go", "sentry.NewClient", false},
		{"/go/src/ergo.services/ergo/act/actor.go", "act.(*Actor).ProcessRun", false},
	}
	for _, tc := range cases {
		if got := isInApp(tc.file, tc.fn); got != tc.want {
			t.Errorf("isInApp(%q, %q): got %v, want %v", tc.file, tc.fn, got, tc.want)
		}
	}
}

// TestRuntimeCallersSeesPanicOrigin exists to prove the load-bearing
// assumption that runtime.Callers, invoked synchronously inside a
// deferred recover, walks the panic origin frames. If this ever breaks
// (Go runtime change), the sentry logger silently loses every stack.
func TestRuntimeCallersSeesPanicOrigin(t *testing.T) {
	var pcs []uintptr
	func() {
		defer func() {
			if recover() != nil {
				buf := make([]uintptr, 32)
				n := runtime.Callers(0, buf)
				pcs = buf[:n]
			}
		}()
		panicEntry()
	}()

	found := false
	cf := runtime.CallersFrames(pcs)
	for {
		f, more := cf.Next()
		if strings.Contains(f.Function, "panicEntry") {
			found = true
		}
		if more == false {
			break
		}
	}
	if found == false {
		t.Fatal("runtime.Callers inside recover defer no longer walks the panic origin; sentry logger stacks will be empty")
	}
}

func panicEntry() {
	panic("origin")
}
