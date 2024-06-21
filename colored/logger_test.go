package colored

import (
	"testing"
	"time"

	"ergo.services/ergo"
	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/lib"
)

func TestColoredFull(t *testing.T) {
	node := gen.Atom("test@localhost")
	peer := gen.Atom("peer@localhost")
	copt := Options{
		TimeFormat:     time.DateTime,
		ShortLevelName: true,
	}
	c, _ := CreateLogger(copt)
	mln := gen.MessageLog{
		Format: "PID %s ProcessID %s Ref %s Alias %s Event %s Atom %s",
	}
	mln.Args = append(mln.Args, gen.PID{Node: node, ID: 1234})
	mln.Args = append(mln.Args, gen.ProcessID{Name: "example", Node: node})
	mln.Args = append(mln.Args, gen.Ref{Node: node, ID: [3]uint64{1234, 5678, 9101}})
	mln.Args = append(mln.Args, gen.Alias{Node: node, ID: [3]uint64{1234, 5678, 9101}})
	mln.Args = append(mln.Args, gen.Event{Name: "example", Node: node})
	mln.Args = append(mln.Args, gen.Atom("test atom"))

	levels := []gen.LogLevel{
		gen.LogLevelTrace,
		gen.LogLevelDebug,
		gen.LogLevelInfo,
		gen.LogLevelWarning,
		gen.LogLevelError,
		gen.LogLevelPanic,
	}
	sourcePID := gen.MessageLogProcess{
		Node: node,
		PID:  gen.PID{Node: node, ID: 45678},
		Name: "prc",
	}
	sourceMeta := gen.MessageLogMeta{
		Parent: gen.PID{Node: node, ID: 45678},
		Meta:   gen.Alias{Node: node, ID: [3]uint64{1234, 5678, 9101}},
	}
	sourceNode := gen.MessageLogNode{
		Node:     node,
		Creation: 123,
	}
	sourceNetwork := gen.MessageLogNetwork{
		Node:     node,
		Peer:     peer,
		Creation: 345,
	}
	sources := []any{sourcePID, sourceMeta, sourceNode, sourceNetwork}

	for _, lev := range levels {
		for _, src := range sources {
			mln.Time = time.Now()
			mln.Level = lev
			mln.Source = src
			c.Log(mln)
		}
	}
}

func TestColoredQuick(t *testing.T) {
	nopt := gen.NodeOptions{}
	nopt.Log.DefaultLogger.Disable = true
	nopt.Log.Level = gen.LogLevelDebug
	nopt.Network.Mode = gen.NetworkModeHidden
	nopt.Network.Cookie = lib.RandomString(10)

	loggerOptions := Options{
		TimeFormat:      time.DateTime,
		IncludeBehavior: true,
		IncludeName:     true,
	}
	l, _ := CreateLogger(loggerOptions)
	logger := gen.Logger{
		Name:   "colored",
		Logger: l,
	}
	nopt.Log.Loggers = []gen.Logger{logger}

	node, err := ergo.StartNode("testlog@localhost", nopt)
	if err != nil {
		panic(err)
	}
	node.Log().Info("node started")
	node.Log().Warning("example Ref %s", node.MakeRef())
	node.Log().Debug("example debug message. node virtual core PID %s", node.PID())
	node.Log().Panic("example panic message")
	node.SpawnRegister("example", quick_factory, gen.ProcessOptions{})
	node.Wait()
}

func quick_factory() gen.ProcessBehavior {
	return &quick{}
}

type quick struct {
	act.Actor
}

func (q *quick) Init(args ...any) error {
	q.Log().Info("process started")
	q.Send(q.PID(), "")
	return nil
}

func (q *quick) HandleMessage(from gen.PID, message any) error {
	q.Log().Debug("example %s message", gen.Atom("debug"))
	q.Log().Panic("example %s message", gen.Atom("panic"))
	go q.Node().Stop()
	return nil
}
