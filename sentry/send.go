package sentry

import (
	"fmt"
	"runtime"
	"strings"

	"ergo.services/ergo/gen"
	sentrygo "github.com/getsentry/sentry-go"
)

// send converts a queued entry into a sentrygo.Event and hands it off to
// sentry-go's transport via the per-logger hub.
func (l *logger) send(e entry) {
	event := sentrygo.NewEvent()
	event.Timestamp = e.msg.Time
	event.Level = mapLevel(e.msg.Level)

	if len(e.msg.Args) > 0 {
		event.Message = fmt.Sprintf(e.msg.Format, e.msg.Args...)
	} else {
		event.Message = e.msg.Format
	}

	tagSource(event, e.msg.Source)

	for _, f := range e.msg.Fields {
		event.Extra[f.Name] = f.Value
	}

	if e.pcs != nil {
		// For panic events the first argument the framework passes to
		// Log().Panic() is the recovered value (see recover sites in
		// act/* and node/*). We expose it as the Exception.Value so
		// Sentry groups by the panic string rather than by the wrapper
		// format string.
		excValue := event.Message
		if len(e.msg.Args) > 0 {
			excValue = fmt.Sprintf("%v", e.msg.Args[0])
		}
		event.Exception = []sentrygo.Exception{{
			Type:       "panic",
			Value:      excValue,
			Stacktrace: buildStacktrace(e.pcs),
		}}
	}

	l.hub.CaptureEvent(event)
}

// mapLevel collapses gen.LogLevel onto sentrygo.Level. Panic maps to Fatal
// because sentry-go has no Panic level and Fatal is the closest "process
// went down" semantic.
func mapLevel(level gen.LogLevel) sentrygo.Level {
	switch level {
	case gen.LogLevelPanic:
		return sentrygo.LevelFatal
	case gen.LogLevelError:
		return sentrygo.LevelError
	case gen.LogLevelWarning:
		return sentrygo.LevelWarning
	case gen.LogLevelInfo:
		return sentrygo.LevelInfo
	case gen.LogLevelDebug, gen.LogLevelTrace:
		return sentrygo.LevelDebug
	}
	return sentrygo.LevelInfo
}

// tagSource attaches structured tags identifying which ergo subsystem
// produced the event. Sentry groups and filters use these tags.
func tagSource(event *sentrygo.Event, source any) {
	switch src := source.(type) {
	case gen.MessageLogNode:
		event.Tags["source"] = "node"
		event.Tags["node"] = string(src.Node)
	case gen.MessageLogNetwork:
		event.Tags["source"] = "network"
		event.Tags["node"] = string(src.Node)
		event.Tags["peer"] = string(src.Peer)
	case gen.MessageLogApplication:
		event.Tags["source"] = "application"
		event.Tags["node"] = string(src.Node)
		event.Tags["app"] = string(src.Name)
		event.Tags["mode"] = src.Mode.String()
	case gen.MessageLogMeta:
		event.Tags["source"] = "meta"
		event.Tags["node"] = string(src.Node)
		event.Tags["meta"] = src.Meta.String()
		if src.Behavior != "" {
			event.Tags["behavior"] = src.Behavior
		}
	case gen.MessageLogProcess:
		event.Tags["source"] = "process"
		event.Tags["node"] = string(src.Node)
		event.Tags["pid"] = src.PID.String()
		if src.Name != "" {
			event.Tags["name"] = src.Name.String()
		}
		if src.Behavior != "" {
			event.Tags["behavior"] = src.Behavior
		}
	}
}

// buildStacktrace materialises runtime PCs into a sentrygo.Stacktrace.
// runtime.CallersFrames yields callee-to-caller. Sentry expects the
// opposite order (caller first, panic site last) so we reverse before
// returning.
func buildStacktrace(pcs []uintptr) *sentrygo.Stacktrace {
	frames := make([]sentrygo.Frame, 0, len(pcs))
	cf := runtime.CallersFrames(pcs)
	for {
		f, more := cf.Next()
		sf := sentrygo.NewFrame(f)
		sf.InApp = isInApp(f.File, f.Function)
		frames = append(frames, sf)
		if more == false {
			break
		}
	}
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	return &sentrygo.Stacktrace{Frames: frames}
}

// isInApp marks frames the user is likely to care about. Go runtime and
// the Sentry SDK itself are never the user's bug; the ergo framework and
// this logger are framework code that the user did not write either.
// Everything else is considered application code.
func isInApp(file, fn string) bool {
	switch {
	case strings.Contains(file, "/runtime/"):
		return false
	case strings.HasPrefix(fn, "runtime."):
		return false
	case strings.Contains(file, "getsentry/sentry-go"):
		return false
	case strings.Contains(file, "ergo.services/"):
		return false
	}
	return true
}
