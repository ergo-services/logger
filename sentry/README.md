# sentry

Sentry-backed logger for the Ergo Framework. Implements `gen.LoggerBehavior`
and forwards panics (always) and errors (from node, network and
application sources) to a Sentry project, complete with stack traces from
the panic origin.

## What it captures

|         | Node | Network | Application | Meta | Process |
|---------|:----:|:-------:|:-----------:|:----:|:-------:|
| Panic   |  X   |    X    |      X      |  X   |    X    |
| Error   |  X   |    X    |      X      | opt  |   opt   |

* All panic-level events go to Sentry, regardless of which subsystem produced
  them.
* Error-level events from `MessageLogNode`, `MessageLogNetwork` and
  `MessageLogApplication` are sent by default.
* Error-level events from `MessageLogMeta` and `MessageLogProcess` are opt-in
  via `CaptureMetaErrors` and `CaptureProcessErrors`.
* Warning, Info, Debug, Trace and System levels are never captured.

## Stack traces at panic

The framework recovers panics in 27 different sites (actor callbacks,
supervisors, pools, web workers, meta processes, applications, network
acceptors, cron jobs and so on). Every site formats a `Log().Panic()`
message before letting the process terminate. The Sentry logger receives
that call synchronously, while the framework's deferred `recover()` is
still on the goroutine. At that exact moment `runtime.Callers` walks the
panic origin frames, so the captured stack points at the line that
actually panicked rather than at the framework's recovery wrapper.

The PCs are pushed onto a bounded, lock-free `lib.QueueMPSC` together with
the log message. A single worker goroutine drains the queue, materialises
frames with `runtime.CallersFrames`, marks the user-app frames `InApp` and
hands the event to `sentry-go`. Sentry-go's own HTTP transport batches
events into a buffered channel and ships them out, so the logger does not
batch on top.

## Usage

```go
import (
    "ergo.services/ergo"
    "ergo.services/ergo/gen"
    "ergo.services/logger/sentry"
)

sl, err := sentry.CreateLogger(sentry.Options{
    DSN:         "https://<key>@sentry.io/<project>",
    Environment: "production",
    Release:     "myapp@1.2.3",
})
if err != nil {
    panic(err)
}

opts := gen.NodeOptions{}
opts.Log.Loggers = []gen.Logger{{Name: "sentry", Logger: sl}}

node, err := ergo.StartNode("mynode@host", opts)
if err != nil {
    panic(err)
}
node.Wait()
```

If you only want Sentry to see errors and panics (without producing log
work for the rest of the levels) register the logger by hand with a
filter:

```go
node.LoggerAdd("sentry", sl, gen.LogLevelError, gen.LogLevelPanic)
```

DSN is also read from `$SENTRY_DSN` when `Options.DSN` is empty.

## Options

| Field                  | Default | Purpose                                                  |
|------------------------|---------|----------------------------------------------------------|
| `DSN`                  | env     | Sentry DSN. Empty string falls back to `$SENTRY_DSN`.    |
| `Environment`          | `""`    | Tag attached to every event.                             |
| `Release`              | `""`    | Tag for the running binary version.                      |
| `ServerName`           | auto    | Override the auto-detected hostname.                     |
| `CaptureMetaErrors`    | `false` | Send `LogLevelError` from `MessageLogMeta`.              |
| `CaptureProcessErrors` | `false` | Send `LogLevelError` from `MessageLogProcess`.           |
| `QueueLimit`           | `1024`  | Hard cap on the internal queue. Pushes past this drop.   |
| `FlushTimeout`         | `5s`    | Bound on `Terminate()` worker drain and Sentry flush.    |
| `SkipFrames`           | `5`     | Top frames trimmed from captured stacks (logger noise).  |
| `BeforeSend`           | `nil`   | Forwarded to `sentry.ClientOptions.BeforeSend`.          |

## Tags on Sentry events

Every event carries a `source` tag identifying the ergo subsystem that
produced it: `node`, `network`, `application`, `meta` or `process`.
Additional tags depend on the source:

* `node`, `network`: `node`, plus `peer` for network.
* `application`: `node`, `app`, `mode`.
* `meta`: `node`, `meta`, optionally `behavior`.
* `process`: `node`, `pid`, optionally `name` and `behavior`.

The `fields` set on the log message lands in `event.Extra`.

## Isolation

The logger creates its own `sentry.Client` and `sentry.Hub`. It does not
touch the global `sentry.CurrentHub()` and does not call `sentry.Init()`,
so an existing Sentry integration elsewhere in the host application keeps
working untouched.

## Shutdown

`Terminate()` flips the logger to a terminated state. The active worker
(or a freshly spawned one if no worker was running) drains anything left
in the queue, then calls `sentry-go`'s `Hub.Flush(FlushTimeout)` so events
already handed to the SDK leave the process before it exits. Subsequent
`Log()` calls after `Terminate()` become no-ops.

## Caveats

* `Log()` runs synchronously inside the framework's panic-recovery defer.
  Make sure no other registered logger blocks for long; this one keeps the
  call short (one `runtime.Callers`, one queue push) so it does not hold up
  panic handling.
* The default `SkipFrames` of 5 matches the current call layout
  (`runtime.Callers` -> `(*logger).Log` -> `(*node).dolog` -> `log.write`
  -> `log.Panic`). If a wrapper layer is inserted between user code and
  `Log().Panic`, increase `SkipFrames` so the noise still gets trimmed.
* Queue overflow drops new events silently. The cap exists to bound
  memory under a panic storm; if you see drops in practice, raise
  `QueueLimit` or look for the actor that keeps panicking.
