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
	var r rotate

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
	r.file = file

	r.format = "200601021504"

	if options.Period < time.Minute {
		options.Period = time.Minute
	}

	r.switchTime = time.Now().Truncate(options.Period).Add(options.Period)

	r.suffix = ".log"
	if options.Compress {
		r.suffix = ".log.gz"
	}

	r.tformat = options.TimeFormat

	if options.ShortLevelName {
		r.levelTrace = "[TRC]"
		r.levelInfo = "[INF]"
		r.levelWarning = "[WRN]"
		r.levelError = "[ERR]"
		r.levelPanic = "[PNC]"
		r.levelDebug = "[DBG]"

	} else {
		r.levelTrace = fmt.Sprintf("[%s]", gen.LogLevelTrace)
		r.levelInfo = fmt.Sprintf("[%s]", gen.LogLevelInfo)
		r.levelWarning = fmt.Sprintf("[%s]", gen.LogLevelWarning)
		r.levelError = fmt.Sprintf("[%s]", gen.LogLevelError)
		r.levelPanic = fmt.Sprintf("[%s]", gen.LogLevelPanic)
		r.levelDebug = fmt.Sprintf("[%s]", gen.LogLevelDebug)
	}

	r.queue = lib.NewQueueMPSC()
	r.options = options

	return &r, nil
}

type Options struct {
	// TimeFormat enables output time in the defined format. See https://pkg.go.dev/time#pkg-constants
	// Not defined format makes output time as a timestamp in nanoseconds.
	TimeFormat string
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

type rotate struct {
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

func (r *rotate) Log(message gen.MessageLog) {
	if atomic.LoadInt32(&r.state) == 2 {
		// terminated
		return
	}
	r.queue.Push(message)
	if atomic.CompareAndSwapInt32(&r.state, 0, 1) == true {
		go r.write()
	}
}

func (r *rotate) Terminate() {
	atomic.StoreInt32(&r.state, 2)
	r.queue.Push(nil)
	if atomic.CompareAndSwapInt32(&r.state, 0, 1) == true {
		go r.write()
	}
}

func (r *rotate) write() {
	var level, t, source string

next:
	if time.Now().After(r.switchTime) {
		r.switchFile()
	}

	for {
		m, ok := r.queue.Pop()
		if ok == false {
			break
		}
		message := m.(gen.MessageLog)
		if r.tformat == "" {
			t = fmt.Sprintf("%d", message.Time.UnixNano())
		} else {
			t = message.Time.Format(r.tformat)
		}

		switch message.Level {
		case gen.LogLevelInfo:
			level = r.levelInfo
		case gen.LogLevelWarning:
			level = r.levelWarning
		case gen.LogLevelError:
			level = r.levelError
		case gen.LogLevelPanic:
			level = r.levelPanic
		case gen.LogLevelDebug:
			level = r.levelDebug
		case gen.LogLevelTrace:
			level = r.levelTrace
		default:
			level = fmt.Sprintf("[%s]", message.Level)
		}

		switch src := message.Source.(type) {
		case gen.MessageLogNode:
			source = src.Node.CRC32()
		case gen.MessageLogNetwork:
			source = fmt.Sprintf("%s-%s", src.Node, src.Peer)
		case gen.MessageLogProcess:
			source = src.PID.String()
		case gen.MessageLogMeta:
			source = src.Meta.String()
		default:
			panic(fmt.Sprintf("unknown log source type: %#v", message.Source))

		}
		msg := fmt.Sprintf(message.Format, message.Args...)
		fmt.Fprintf(r.file, "%s %s %s: %s\n", t, level, source, msg)
	}

	if atomic.CompareAndSwapInt32(&r.state, 1, 0) == false {
		// terminated
		r.file.Sync()
		r.file.Close()
		return
	}

	if r.queue.Item() == nil {
		return
	}

	// got some in the queue
	if atomic.CompareAndSwapInt32(&r.state, 0, 1) == true {
		goto next
	}

	// another goroutine is running
}

func (r *rotate) switchFile() {

	name := r.options.Prefix + "." + r.switchTime.Add(-r.options.Period).Format(r.format) + r.suffix

	// set the next switch time
	if r.switchTime.Add(r.options.Period).Before(time.Now()) {
		// had no logs longer than the period
		r.switchTime = time.Now().Truncate(r.options.Period)
	}

	r.switchTime = r.switchTime.Add(r.options.Period)

	fname := filepath.Join(r.options.Path, name)
	out, err := os.Create(fname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to rotate log file (%s): %s", fname, err)
		return
	}

	// copy current file
	r.file.Sync()
	r.file.Seek(0, io.SeekStart)
	if r.options.Compress {
		zout := gzip.NewWriter(out)
		io.Copy(zout, r.file)
		zout.Close()
	} else {
		// copy current file
		io.Copy(out, r.file)
	}
	out.Close()

	// truncate it
	r.file.Truncate(0)
	r.file.Seek(0, io.SeekStart)

	if r.options.Depth > 0 {
		r.depth = append(r.depth, fname)
		if len(r.depth) > r.options.Depth {
			os.Remove(r.depth[0])
			r.depth = r.depth[1:]
		}
	}
}
