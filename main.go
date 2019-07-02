package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/go-yaml/yaml"
	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

func main() {
	configFile := kingpin.Arg("config", "config file").Default("./config.yaml").String()
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger, err := newLogger()
	if err != nil {
		panic(err)
	}

	mgrLog := logger.With(zap.String("process", "manager"))

	data, err := ioutil.ReadFile(*configFile)
	if err != nil {
		mgrLog.Fatal("",
			zap.String("log", "failed to read config file"), zap.String("filename", *configFile), zap.Error(err))
	}

	var cfg config

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		mgrLog.Fatal("",
			zap.String("log", "failed to read parse file"), zap.String("filename", *configFile), zap.Error(err))
	}

	go reapChildren()

	collector := &logCollector{
		logger: logger,
		mgrLog: mgrLog,
	}

	// we use a separate context for signals so that we can control
	// sending signal to children and use another context for sigkill
	signals := signalHandlingContext()

	errors := make(chan error, len(cfg.Procs))

	ctx, cancel := context.WithCancel(context.Background())

	for _, p := range cfg.Procs {
		p := p

		cmd := exec.CommandContext(ctx, p.Command, p.Arguments...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		stdout, stderr := collector.add(ctx, p)

		cmd.Stdout = stdout
		cmd.Stderr = stderr

		p.cmd = cmd

		go func() {
			errors <- cmd.Run()
		}()
	}

	// any of these conditions signal an exit
	select {
	case err := <-errors:
		if err != nil {
			mgrLog.Error("",
				zap.String("log", "process exited unexpectedly"), zap.Error(err))
		}
	// Log error? could be nil on normal exit
	case <-ctx.Done():
	// this should never be done until later, but check just in case
	case <-signals.Done():
	}

	gracePeriod := cfg.GracePeriod
	if gracePeriod == time.Duration(0) {
		gracePeriod = time.Second * 10
	}

	timeout, timeoutCancel := context.WithTimeout(ctx, gracePeriod)

	defer timeoutCancel()
	done := signalCommands(timeout, cfg.Procs, mgrLog)

	select {
	case <-done:
	case <-timeout.Done():
	}

	// will send sigkill to processes
	// https://golang.org/pkg/os/exec/#CommandContext
	cancel()
}

func signalCommands(ctx context.Context, procs []*procConfig, logger *zap.Logger) <-chan struct{} {
	done := make(chan struct{})

	var wg sync.WaitGroup

	for _, p := range procs {
		p := p

		wg.Add(1)
		go func() {
			defer wg.Done()

			if p.cmd.Process == nil {
				return
			}

			// process group should be the same as the pid since we set Setpgid
			// but check just to be sure

			pid := p.cmd.Process.Pid
			gid, err := syscall.Getpgid(pid)
			if err != nil {
				logger.Error("",
					zap.String("log", "failed to get process group"),
					zap.Int("pid", pid), zap.String("child", p.Name))
			} else {
				// signal all processes in group
				pid = -1 * gid
			}

			sig := syscall.Signal(p.Signal)
			if sig == syscall.Signal(0) {
				sig = syscall.SIGTERM
			}

			if err := syscall.Kill(pid, sig); err != nil {
				logger.Error("",
					zap.String("log", "failed to signal process"),
					zap.Int("pid", pid), zap.String("child", p.Name), zap.Stringer("signal", sig))
			}

			t := time.NewTicker(time.Millisecond * 100)
			defer t.Stop()
			select {

			case <-t.C:
				if !processIsRunning(p.cmd) {
					return
				}
			case <-ctx.Done():
				return
			}
		}()
	}

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	return done
}

func processIsRunning(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}

	if cmd.ProcessState == nil {
		return false
	}

	if cmd.ProcessState.Exited() {
		return false
	}

	err := cmd.Process.Signal(syscall.Signal(0))
	return err == nil

}

type logCollector struct {
	sync.Mutex
	logger *zap.Logger
	mgrLog *zap.Logger
	sinks  []*logSink
}

type logSink struct {
	name         string
	stdoutWriter *io.PipeWriter
	stdoutReader *io.PipeReader
	stderrWriter *io.PipeWriter
	stderrReader *io.PipeReader
	logCollector *logCollector
}

func (l *logCollector) add(ctx context.Context, c *procConfig) (io.Writer, io.Writer) {
	s := logSink{
		name: c.Name,
	}

	s.stdoutReader, s.stdoutWriter = io.Pipe()
	s.stderrReader, s.stderrWriter = io.Pipe()

	// TODO: flush when context is done and exit?
	go func() {
		logger := l.logger.With(
			zap.String("stream", "stdout"),
			zap.String("process", s.name),
		)

		topLevel := false
		if c.ParseInto != nil && *c.ParseInto == "" {
			//special case, merge into top level
			topLevel = true
		}

		scanner := bufio.NewScanner(s.stdoutReader)
		for scanner.Scan() {
			line := scanner.Text()

			if c.ParseInto == nil {
				logger.Info("", zap.String("log", line))
				continue
			}

			var logFields map[string]interface{}
			if err := json.Unmarshal([]byte(line), &logFields); err != nil {
				l.mgrLog.Error("",
					zap.String("log", "failed to unmarshal log line"),
					zap.String("child", s.name),
					zap.String("line", line),
				)
				continue
			}

			fields := make([]zapcore.Field, 0, len(logFields)+2)
			if !topLevel {
				fields = append(fields, zap.Namespace(*c.ParseInto))
			}

			for k, v := range logFields {
				fields = append(fields, zap.Any(k, v))
			}

			if *c.ParseInto != "log" {
				fields = append(fields, zap.String("log", line))
			}

			logger.Info("", fields...)

		}
		if err := scanner.Err(); err != nil {
			l.mgrLog.Error("",
				zap.String("log", "error scanning output"),
				zap.String("process", s.name),
				zap.String("stream", "stderr"),
			)
		}
	}()

	go func() {
		logger := l.logger.With(
			zap.String("process", s.name),
			zap.String("stream", "stderr"),
		)
		scanner := bufio.NewScanner(s.stderrReader)
		for scanner.Scan() {
			line := scanner.Text()
			logger.Info(line)
		}
		if err := scanner.Err(); err != nil {
			l.mgrLog.Error("",
				zap.String("log", "error scanning output"),
				zap.String("process", s.name),
				zap.String("stream", "stderr"),
			)
		}
	}()

	l.Lock()
	defer l.Unlock()
	l.sinks = append(l.sinks, &s)

	return s.stdoutWriter, s.stderrWriter
}

type config struct {
	GracePeriod time.Duration `yaml:"grace_period"`
	Procs       []*procConfig `yaml:"processes"`
}

type procConfig struct {
	Name      string     `yaml:"name"`
	Command   string     `yaml:"command"`
	Arguments []string   `yaml:"arguments"`
	ParseInto *string    `yaml:"parse_into"`
	Signal    confSignal `yaml:"signal"`
	cmd       *exec.Cmd
}

func newLogger() (*zap.Logger, error) {
	encConfig := zapcore.EncoderConfig{
		TimeKey:    "time",
		NameKey:    "process",
		LineEnding: zapcore.DefaultLineEnding,
		EncodeTime: rfc3339NanoTimeEncoder,
	}

	config := zap.Config{
		Development:       false,
		DisableCaller:     true,
		DisableStacktrace: true,
		EncoderConfig:     encConfig,
		Encoding:          "json",
		ErrorOutputPaths:  []string{"stderr"},
		Level:             zap.NewAtomicLevel(),
		OutputPaths:       []string{"stdout"},
	}

	l, err := config.Build()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create logger")
	}

	return l, nil

}

func rfc3339NanoTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.Format(time.RFC3339Nano))
}

func signalHandlingContext() context.Context {
	sigs := make(chan os.Signal, 1)
	// can't really catch sigkill, but include it anyway
	signal.Notify(sigs, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-sigs
		cancel()
	}()

	return ctx
}

// based on https://github.com/ramr/go-reaper
// MIT License 2015 ramr
func reapChildren() {
	// wait4 blocks, so buffer the channel
	var notifications = make(chan os.Signal, 1)

	go sigChildHandler(notifications)

	for {
		<-notifications
	WAIT:
		for {
			var wstatus syscall.WaitStatus
			_, err := syscall.Wait4(-1, &wstatus, 0, nil)

			switch err {
			case nil:
				break WAIT
			case syscall.ECHILD:
				// no child process. something else reaped it
				break WAIT
			default:
				// loop will retry
			}
		}
	}
}

func sigChildHandler(notifications chan os.Signal) {
	// unsure why go-reaper chose 3 here
	var sigs = make(chan os.Signal, 3)
	signal.Notify(sigs, syscall.SIGCHLD)

	for {
		var sig = <-sigs
		select {
		case notifications <- sig:
		default:
		}
	}
}

type confSignal syscall.Signal

func (s *confSignal) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var v string
	if err := unmarshal(&v); err != nil {
		return err
	}

	sig := syscall.SIGTERM
	switch strings.ToUpper(v) {
	case "", "TERM":
		sig = syscall.SIGTERM
	case "INT":
		sig = syscall.SIGINT
	case "QUIT":
		sig = syscall.SIGQUIT
	case "USR1":
		sig = syscall.SIGUSR1
	case "USR2":
		sig = syscall.SIGUSR1
	default:
		return fmt.Errorf("unsupported or uknown signal: %s", v)
	}

	*s = confSignal(sig)
	return nil
}
