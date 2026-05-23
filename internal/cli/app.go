package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type App struct {
	stdout io.Writer
	stderr io.Writer
}

func New() *App {
	return &App{}
}

func (a *App) Run(_ context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("")
	}

	switch args[0] {
	case "commit-msg":
		return a.runCommitMsg(args[1:])
	case "release-note":
		return a.runReleaseNote(args[1:])
	case "-h", "--help", "help":
		return usageError("")
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func (a *App) runCommitMsg(args []string) error {
	fs := flag.NewFlagSet("commit-msg", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var amend bool
	var model string
	var baseURL string
	var timeout string
	var maxSteps int
	var guidanceFamily string
	var debug bool

	fs.BoolVar(&amend, "amend", false, "generate an amended commit message")
	fs.StringVar(&model, "model", "", "override OPENAI_MODEL")
	fs.StringVar(&baseURL, "base-url", "", "override OPENAI_BASE_URL")
	fs.StringVar(&timeout, "timeout", "", "override default request timeout")
	fs.IntVar(&maxSteps, "max-steps", 0, "override maximum agent steps")
	fs.StringVar(&guidanceFamily, "guidance-family", "", "force guidance family")
	fs.BoolVar(&debug, "debug", false, "enable debug output on stderr")

	if err := fs.Parse(args); err != nil {
		return err
	}

	_ = model
	_ = baseURL
	_ = timeout
	_ = maxSteps
	_ = guidanceFamily
	_ = debug

	if fs.NArg() != 0 {
		return errors.New("commit-msg does not accept positional arguments")
	}

	mode := "normal"
	if amend {
		mode = "amend"
	}
	return ErrNotImplemented("commit-msg " + mode)
}

func (a *App) runReleaseNote(args []string) error {
	fs := flag.NewFlagSet("release-note", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var model string
	var baseURL string
	var timeout string
	var maxSteps int
	var guidanceFamily string
	var debug bool

	fs.StringVar(&model, "model", "", "override OPENAI_MODEL")
	fs.StringVar(&baseURL, "base-url", "", "override OPENAI_BASE_URL")
	fs.StringVar(&timeout, "timeout", "", "override default request timeout")
	fs.IntVar(&maxSteps, "max-steps", 0, "override maximum agent steps")
	fs.StringVar(&guidanceFamily, "guidance-family", "", "force guidance family")
	fs.BoolVar(&debug, "debug", false, "enable debug output on stderr")

	if err := fs.Parse(args); err != nil {
		return err
	}

	_ = model
	_ = baseURL
	_ = timeout
	_ = maxSteps
	_ = guidanceFamily
	_ = debug

	if fs.NArg() != 2 {
		return errors.New("release-note requires <base> <release>")
	}

	return ErrNotImplemented("release-note")
}

func usageError(prefix string) error {
	var b strings.Builder
	if prefix != "" {
		b.WriteString(prefix)
		b.WriteString("\n\n")
	}
	b.WriteString("usage:\n")
	b.WriteString("  git-agent commit-msg [--amend] [flags]\n")
	b.WriteString("  git-agent release-note <base> <release> [flags]\n")
	return errors.New(b.String())
}
