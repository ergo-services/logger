package sentry

import (
	"runtime"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/lib"
	sentrygo "github.com/getsentry/sentry-go"
)

// CreateLogger constructs a Sentry-backed gen.LoggerBehavior.
//
// The logger captures LogLevelPanic events from every source and
// LogLevelError events from MessageLogNode, MessageLogNetwork and
// MessageLogApplication. Error events from MessageLogMeta and
// MessageLogProcess are opt-in via Options.CaptureMetaErrors and
// Options.CaptureProcessErrors.
//
// Panic stacks are captured synchronously inside Log(). At that point the
// framework's deferred recover handler is still on the goroutine, so
// runtime.Callers walks the panic origin. The captured PCs are pushed onto
// a bounded lock-free queue together with the log message; a single worker
// goroutine drains the queue, materialises the frames and hands the event
// to sentry-go (which has its own buffering transport).
//
// The logger uses its own sentrygo.Hub and sentrygo.Client, so an existing
// global sentrygo.Init() in the host application is left untouched.
func CreateLogger(options Options) (gen.LoggerBehavior, error) {
	if options.QueueLimit <= 0 {
		options.QueueLimit = defaultQueueLimit
	}
	if options.FlushTimeout <= 0 {
		options.FlushTimeout = defaultFlushTimeout
	}
	if options.SkipFrames <= 0 {
		options.SkipFrames = defaultSkipFrames
	}

	client, err := sentrygo.NewClient(sentrygo.ClientOptions{
		Dsn:         options.DSN,
		Environment: options.Environment,
		Release:     options.Release,
		ServerName:  options.ServerName,
		BeforeSend:  options.BeforeSend,
		Transport:   options.transport, // test hook; nil in production
	})
	if err != nil {
		return nil, err
	}

	return &logger{
		options: options,
		queue:   lib.NewQueueLimitMPSC(int64(options.QueueLimit), false),
		hub:     sentrygo.NewHub(client, sentrygo.NewScope()),
	}, nil
}

// Options configures the Sentry logger.
type Options struct {
	// DSN selects the destination Sentry project. Empty string falls back
	// to the SENTRY_DSN environment variable as handled by sentry-go.
	DSN string

	// Environment is attached to every event as the sentry environment tag.
	Environment string

	// Release identifies the running binary version.
	Release string

	// ServerName overrides the auto-detected hostname.
	ServerName string

	// CaptureMetaErrors enables capture of LogLevelError events from
	// MessageLogMeta. Panic-level events from meta processes are always
	// captured regardless of this flag.
	CaptureMetaErrors bool

	// CaptureProcessErrors enables capture of LogLevelError events from
	// MessageLogProcess. Panic-level events from processes are always
	// captured regardless of this flag.
	CaptureProcessErrors bool

	// QueueLimit caps the internal queue. Once full, further events are
	// dropped silently. The SDK's own transport buffer would do the same
	// past its own cap, this limit prevents memory growth ahead of that.
	// Zero means defaultQueueLimit.
	QueueLimit int

	// FlushTimeout bounds Terminate(): how long the worker is given to
	// drain the queue and how long sentry-go's transport is given to
	// flush in-flight events before the logger returns. Zero means
	// defaultFlushTimeout.
	FlushTimeout time.Duration

	// SkipFrames trims the captured stack from the top before forwarding
	// it to Sentry, removing the logger's own dispatch frames
	// (runtime.Callers, Log, node.dolog, log.write, log.Panic). Zero
	// means defaultSkipFrames; override if a custom wrapper layer changes
	// the noise count.
	SkipFrames int

	// BeforeSend is forwarded to sentrygo.ClientOptions and runs for every
	// outgoing event. Return nil to drop.
	BeforeSend func(*sentrygo.Event, *sentrygo.EventHint) *sentrygo.Event

	// transport is a private test hook. Tests substitute an in-memory
	// transport to assert on outgoing events without network I/O.
	transport sentrygo.Transport
}

const (
	defaultQueueLimit   = 1024
	defaultFlushTimeout = 5 * time.Second
	defaultSkipFrames   = 5
)

// entry carries one queued event from Log() to the worker goroutine.
// pcs is non-nil only for panic events; it is captured inside Log() while
// the framework's recover defer is still on the goroutine.
type entry struct {
	msg gen.MessageLog
	pcs []uintptr
}

type logger struct {
	options Options
	queue   lib.QueueMPSC
	hub     *sentrygo.Hub

	// state coordinates the single-consumer worker.
	//   0 = idle, no worker
	//   1 = worker running
	//   2 = terminated, worker (if any) finalises and exits
	state int32
}

const (
	stateIdle       int32 = 0
	stateRunning    int32 = 1
	stateTerminated int32 = 2
)

// Log captures the message if it falls within the configured matrix.
// For panics it snapshots the goroutine stack via runtime.Callers. That
// works because Log runs synchronously inside the framework's recover
// defer, so the panic frames are still walkable. The PCs are deferred to
// a worker goroutine for the comparatively expensive frame materialisation
// and Sentry envelope build.
func (l *logger) Log(msg gen.MessageLog) {
	if atomic.LoadInt32(&l.state) == stateTerminated {
		return
	}
	if l.shouldCapture(msg) == false {
		return
	}

	var pcs []uintptr
	if msg.Level == gen.LogLevelPanic {
		buf := make([]uintptr, 64)
		n := runtime.Callers(l.options.SkipFrames, buf)
		pcs = buf[:n]
	}

	if l.queue.Push(entry{msg: msg, pcs: pcs}) == false {
		return // queue full, drop new event
	}
	if atomic.CompareAndSwapInt32(&l.state, stateIdle, stateRunning) {
		go l.drain()
	}
}

// shouldCapture encodes the source/level matrix.
//
//	                       Node Net App Meta Process
//	  LogLevelPanic         X    X   X   X    X
//	  LogLevelError         X    X   X   opt  opt
func (l *logger) shouldCapture(msg gen.MessageLog) bool {
	switch msg.Level {
	case gen.LogLevelPanic:
		switch msg.Source.(type) {
		case gen.MessageLogNode,
			gen.MessageLogNetwork,
			gen.MessageLogApplication,
			gen.MessageLogMeta,
			gen.MessageLogProcess:
			return true
		}
	case gen.LogLevelError:
		switch msg.Source.(type) {
		case gen.MessageLogNode,
			gen.MessageLogNetwork,
			gen.MessageLogApplication:
			return true
		case gen.MessageLogMeta:
			return l.options.CaptureMetaErrors
		case gen.MessageLogProcess:
			return l.options.CaptureProcessErrors
		}
	}
	return false
}

// drain is the worker. The state machine guarantees exactly one drain
// goroutine runs at any moment. Two CAS operations compose the handover
// at the queue boundary, same protocol as in the rotate logger:
//
//  1. After the queue is empty, drain runs CAS(running -> idle). If it
//     fails the state is already terminated; flush the SDK and exit.
//  2. If a producer pushed between the queue going empty and this CAS,
//     queue.Item() is non-nil. drain re-acquires ownership via
//     CAS(idle -> running) and loops. If a producer's Log() did the
//     same CAS first (and spawned a new worker), drain's CAS fails and
//     drain exits, leaving the new worker as the sole consumer.
//
// Both paths preserve the single-consumer invariant.
func (l *logger) drain() {
next:
	for {
		m, ok := l.queue.Pop()
		if ok == false {
			break
		}
		l.send(m.(entry))
	}

	if atomic.CompareAndSwapInt32(&l.state, stateRunning, stateIdle) == false {
		// state has advanced to terminated. Flush SDK side, exit.
		l.hub.Flush(l.options.FlushTimeout)
		return
	}

	if l.queue.Item() == nil {
		return
	}

	if atomic.CompareAndSwapInt32(&l.state, stateIdle, stateRunning) {
		goto next
	}
	// CAS lost to a producer's own CAS(idle -> running). The producer
	// spawned a fresh worker; exit and yield ownership.
}

// Terminate flips state to terminated. If no worker was running, we kick
// one to drain any tail items and call Flush; otherwise the running worker
// will detect the state change on its next CAS and run Flush itself.
func (l *logger) Terminate() {
	prev := atomic.SwapInt32(&l.state, stateTerminated)
	if prev == stateIdle {
		go l.drain()
	}
}
