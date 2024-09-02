package colored

import (
	"fmt"
	"io"

	"ergo.services/ergo/gen"
	"github.com/fatih/color"
)

var (
	levelTrace *color.Color
	colorFaint *color.Color
)

func init() {
	color.NoColor = false

	levelTrace = color.New(color.FgWhite, color.Faint)
	colorFaint = color.New(color.FgWhite, color.Faint)
}

func CreateLogger(options Options) (gen.LoggerBehavior, error) {
	var c logger

	c.format = options.TimeFormat

	if options.ShortLevelName {
		c.levelTrace = "[TRC]"
		c.levelInfo = "[INF]"
		c.levelWarning = color.YellowString("[WRN]")
		c.levelError = color.New(color.FgRed, color.Bold).Sprintf("[ERR]")
		c.levelPanic = color.New(color.FgWhite, color.BgRed, color.Bold).Sprintf("[PNC]")
		c.levelDebug = color.MagentaString("[DBG]")
		return &c, nil

	}

	c.levelTrace = fmt.Sprintf("[%s]", gen.LogLevelTrace)
	c.levelInfo = fmt.Sprintf("[%s]", gen.LogLevelInfo)
	c.levelWarning = color.YellowString("[%s]", gen.LogLevelWarning)
	c.levelError = color.New(color.FgRed, color.Bold).Sprintf("[%s]", gen.LogLevelError)
	c.levelPanic = color.New(color.FgWhite, color.BgRed, color.Bold).Sprintf("[%s]", gen.LogLevelPanic)
	c.levelDebug = color.MagentaString("[%s]", gen.LogLevelDebug)

	c.includeBehavior = options.IncludeBehavior
	c.includeName = options.IncludeName

	return &c, nil
}

type Options struct {
	// TimeFormat enables output time in the defined format. See https://pkg.go.dev/time#pkg-constants
	// Not defined format makes output time as a timestamp in nanoseconds.
	TimeFormat string
	// IncludeBehavior includes process/meta behavior to the log message
	IncludeBehavior bool
	// IncludeName includes registered process name to the log message
	IncludeName bool
	// ShortLevelName enables shortnames for the log levels
	ShortLevelName bool
}

type logger struct {
	out             io.Writer
	format          string
	includeBehavior bool
	includeName     bool

	levelTrace   string
	levelInfo    string
	levelWarning string
	levelError   string
	levelPanic   string
	levelDebug   string
}

func (l *logger) Log(message gen.MessageLog) {
	var level, t, source, name, behavior string

	if l.format == "" {
		t = fmt.Sprintf("%d", message.Time.UnixNano())
	} else {
		t = message.Time.Format(l.format)
	}

	switch src := message.Source.(type) {
	case gen.MessageLogNode:
		source = color.GreenString(src.Node.CRC32())
	case gen.MessageLogNetwork:
		source = color.GreenString("%s-%s", src.Node.CRC32(), src.Peer.CRC32())
	case gen.MessageLogProcess:
		if l.includeBehavior {
			behavior = " " + src.Behavior
		}
		if l.includeName && src.Name != "" {
			name = " " + color.GreenString(src.Name.String())
		}
		source = fmt.Sprintf("%s%s%s", color.BlueString("%s", src.PID), name, behavior)
	case gen.MessageLogMeta:
		if l.includeBehavior {
			behavior = " " + src.Behavior
		}
		source = fmt.Sprintf("%s%s%s", color.CyanString("%s", src.Meta), name, behavior)
	default:
		panic(fmt.Sprintf("unknown log source type: %#v", message.Source))
	}

	switch message.Level {
	case gen.LogLevelInfo:
		level = l.levelInfo
	case gen.LogLevelWarning:
		level = l.levelWarning
	case gen.LogLevelError:
		level = l.levelError
	case gen.LogLevelPanic:
		level = l.levelPanic
	case gen.LogLevelDebug:
		level = l.levelDebug
	case gen.LogLevelTrace:
		msg := fmt.Sprintf(message.Format, message.Args...)
		colorFaint.Printf("%s %s %s: %s\n", t, l.levelTrace, source, msg)
		return

	default:
		level = fmt.Sprintf("[%s]", message.Level)
	}

	// we shouldn't modify message.Args (might be used in the other loggers)
	args := []any{}
	for i := range message.Args {
		switch message.Args[i].(type) {
		case gen.PID:
			args = append(args, color.BlueString("%s", message.Args[i]))
		case gen.ProcessID:
			args = append(args, color.BlueString("%s", message.Args[i]))
		case gen.Atom:
			args = append(args, color.GreenString("%s", message.Args[i]))
		case gen.Ref:
			args = append(args, color.CyanString("%s", message.Args[i]))
		case gen.Alias:
			args = append(args, color.CyanString("%s", message.Args[i]))
		case gen.Event:
			args = append(args, color.CyanString("%s", message.Args[i]))
		default:
			args = append(args, message.Args[i])
		}
	}

	msg := fmt.Sprintf(message.Format, args...)
	fmt.Printf("%s %s %s: %s\n", colorFaint.Sprintf("%s", t), level, source, msg)
}

func (l *logger) Terminate() {}
