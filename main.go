package main

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
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
	configFile := kingpin.Arg("config", "config file").Default("./config.yaml").ExistingFile()
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger, err := newLogger()
	if err != nil {
		panic(err)
	}

	data, err := ioutil.ReadFile(*configFile)
	if err != nil {
		logger.Fatal("failed to read config file", zap.String("filename", *configFile), zap.Error(err))
	}

	var cfg config

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logger.Fatal("failed to read parse file", zap.String("filename", *configFile), zap.Error(err))
	}

	go reapChildren()

	collector := &logCollector{
		logger: logger,
	}

	// we use a separate context for signals so that we can control
	// sending signal to children and use another context for sigkill
	signals := signalHandlingContext()

	errors := make(chan error, len(cfg.Procs))

	var commands []*exec.Cmd

	ctx, cancel := context.WithCancel(context.Background())

	for _, p := range cfg.Procs {
		p := p

		cmd := exec.CommandContext(ctx, p.Command, p.Arguments...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		stdout, stderr := collector.add(ctx, &p)

		cmd.Stdout = stdout
		cmd.Stderr = stderr

		commands = append(commands, cmd)
		go func() {
			if err := cmd.Start(); err != nil {
				errors <- err
				return
			}

			if err := cmd.Wait(); err != nil {
				errors <- err
				return
			}
			errors <- nil
		}()
	}

	// any of these conditions signal an exit
	select {
	case <-errors:
	// Log error?
	case <-ctx.Done():
	case <-signals.Done():
	}

	done := signalCommands(syscall.SIGTERM, commands)

	t := time.NewTimer(time.Second * 10)

	select {
	case <-done:
	case <-t.C:
	}

	// will send sigkill to processes
	// https://golang.org/pkg/os/exec/#CommandContext
	cancel()
}

func signalCommands(sig syscall.Signal, commands []*exec.Cmd) <-chan struct{} {
	done := make(chan struct{})

	var wg sync.WaitGroup

	for _, cmd := range commands {
		cmd := cmd

		wg.Add(1)
		go func() {
			defer wg.Done()

			if cmd.Process == nil {
				return
			}

			// should we log any error we get here?
			cmd.Process.Signal(sig)

			t := time.NewTicker(time.Millisecond * 100)
			defer t.Stop()
			for {
				<-t.C
				if cmd.ProcessState == nil {
					return
				}

				if !processIsRunning(cmd) {
					return
				}
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
				l.logger.Error("failed to unmarshal log line",
					zap.String("child", s.name),
					zap.String("line", line),
					zap.String("process", "manager"),
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
		// do we care about scanner error?
	}()

	l.Lock()
	defer l.Unlock()
	l.sinks = append(l.sinks, &s)

	return s.stdoutWriter, s.stderrWriter
}

type config struct {
	Procs []procConfig `yaml:"processes"`
}

type procConfig struct {
	Name      string   `yaml:"name"`
	Command   string   `yaml:"command"`
	Arguments []string `yaml:"arguments"`
	ParseInto *string  `yaml:"parse_into"`
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
