package rotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/lib"
)

func CreateLogger(options Options) (gen.LoggerBehavior, error) {
	var l logger

	if options.Path == "" {
		dir := filepath.Dir(os.Args[0])
		options.Path = filepath.Join(dir, "logs")
	}

	if strings.HasPrefix(options.Path, "~") {
		// home dir
		home, _ := os.UserHomeDir()
		path := strings.TrimPrefix(options.Path, "~")
		options.Path = filepath.Join(home, path)
	}

	if err := os.MkdirAll(options.Path, 0755); err != nil {
		return nil, fmt.Errorf("unable to create %s: %s", options.Path, err)
	}

	if options.Prefix == "" {
		options.Prefix = filepath.Base(os.Args[0])
	}

	fname := filepath.Join(options.Path, options.Prefix+".log")
	file, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	l.file = file

	l.format = "200601021504"

	if options.Period < time.Minute {
		options.Period = time.Minute
	}

	l.switchTime = time.Now().Truncate(options.Period).Add(options.Period)

	l.suffix = ".log"
	if options.Compress {
		l.suffix = ".log.gz"
	}

	l.tformat = options.TimeFormat
	l.queue = lib.NewQueueMPSC()
	l.options = options

	if options.ShortLevelName {
		l.levelTrace = "[TRC]"
		l.levelInfo = "[INF]"
		l.levelWarning = "[WRN]"
		l.levelError = "[ERR]"
		l.levelPanic = "[PNC]"
		l.levelDebug = "[DBG]"

		return &l, nil
	}

	l.levelTrace = fmt.Sprintf("[%s]", gen.LogLevelTrace)
	l.levelInfo = fmt.Sprintf("[%s]", gen.LogLevelInfo)
	l.levelWarning = fmt.Sprintf("[%s]", gen.LogLevelWarning)
	l.levelError = fmt.Sprintf("[%s]", gen.LogLevelError)
	l.levelPanic = fmt.Sprintf("[%s]", gen.LogLevelPanic)
	l.levelDebug = fmt.Sprintf("[%s]", gen.LogLevelDebug)

	return &l, nil
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
	// Path directory for the log files
	Path string
	// Prefix
	Prefix string
	// Period rotation period
	Period time.Duration
	// Compress enables gzipping for the log files
	Compress bool
	// Depth how many log files in the rotation
	Depth int
}

type logger struct {
	queue lib.QueueMPSC
	depth []string

	options Options

	file *os.File

	tformat string // time format
	suffix  string

	switchTime time.Time

	format string

	levelTrace   string
	levelInfo    string
	levelWarning string
	levelError   string
	levelPanic   string
	levelDebug   string

	state int32 // 0 - wait, 1 - run, 2 - close
}

func (l *logger) Log(message gen.MessageLog) {
	if atomic.LoadInt32(&l.state) == 2 {
		// terminated
		return
	}
	l.queue.Push(message)
	if atomic.CompareAndSwapInt32(&l.state, 0, 1) == true {
		go l.write()
	}
}

func (l *logger) Terminate() {
	if prev := atomic.SwapInt32(&l.state, 2); prev == 0 {
		//handle the queue in case something is left there
		go l.write()
	}
}

func (l *logger) write() {
	var level, t, source, name, behavior string

next:
	if time.Now().After(l.switchTime) {
		l.switchFile()
	}

	for {
		m, ok := l.queue.Pop()
		if ok == false {
			break
		}
		message := m.(gen.MessageLog)
		if l.tformat == "" {
			t = fmt.Sprintf("%d", message.Time.UnixNano())
		} else {
			t = message.Time.Format(l.tformat)
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
			level = l.levelTrace
		default:
			level = fmt.Sprintf("[%s]", message.Level)
		}

		switch src := message.Source.(type) {
		case gen.MessageLogNode:
			source = src.Node.CRC32()
		case gen.MessageLogNetwork:
			source = fmt.Sprintf("%s-%s", src.Node, src.Peer)
		case gen.MessageLogProcess:
			if l.options.IncludeBehavior {
				behavior = " " + src.Behavior
			}
			if l.options.IncludeName && src.Name != "" {
				name = " " + src.Name.String()
			}
			source = src.PID.String()
		case gen.MessageLogMeta:
			source = src.Meta.String()
		default:
			panic(fmt.Sprintf("unknown log source type: %#v", message.Source))

		}
		msg := fmt.Sprintf(message.Format, message.Args...)
		fmt.Fprintf(l.file, "%s %s %s%s%s: %s\n", t, level, source, name, behavior, msg)
	}

	if atomic.CompareAndSwapInt32(&l.state, 1, 0) == false {
		// terminated
		l.file.Sync()
		l.file.Close()
		return
	}

	if l.queue.Item() == nil {
		return
	}

	// got some in the queue
	if atomic.CompareAndSwapInt32(&l.state, 0, 1) == true {
		goto next
	}

	// another goroutine is running
}

func (l *logger) switchFile() {

	name := l.options.Prefix + "." + l.switchTime.Add(-l.options.Period).Format(l.format) + l.suffix

	// set the next switch time
	if l.switchTime.Add(l.options.Period).Before(time.Now()) {
		// had no logs longer than the period
		l.switchTime = time.Now().Truncate(l.options.Period)
	}

	l.switchTime = l.switchTime.Add(l.options.Period)

	fname := filepath.Join(l.options.Path, name)
	out, err := os.Create(fname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to rotate log file (%s): %s", fname, err)
		return
	}

	// copy current file
	l.file.Sync()
	l.file.Seek(0, io.SeekStart)
	if l.options.Compress {
		zout := gzip.NewWriter(out)
		io.Copy(zout, l.file)
		zout.Close()
	} else {
		// copy current file
		io.Copy(out, l.file)
	}
	out.Close()

	// truncate it
	l.file.Truncate(0)
	l.file.Seek(0, io.SeekStart)

	if l.options.Depth > 0 {
		l.depth = append(l.depth, fname)
		if len(l.depth) > l.options.Depth {
			os.Remove(l.depth[0])
			l.depth = l.depth[1:]
		}
	}
}
