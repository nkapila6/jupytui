package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/marimo"
	"github.com/nkapila6/jupytui/internal/notebook"
	"github.com/nkapila6/jupytui/internal/session"
	"github.com/nkapila6/jupytui/internal/ui"
)

const usage = `usage: jupytui <notebook.ipynb>        open (or create) a notebook
       jupytui exec <notebook.ipynb>   run all cells headless and print outputs
       jupytui export [-f] [-o out.py] <notebook.ipynb>
                                       write a percent-format .py (# %% cells)
       jupytui sessions [kill <n>]           list (or stop) detached kernels
       jupytui diff [--outputs] a.ipynb b.ipynb
       jupytui clean [--stdin] nb.ipynb...    strip outputs
       jupytui git setup [--strip-outputs]    notebook diff/merge drivers for this repo
       jupytui convert [-f] in out            .ipynb <-> marimo .py
       jupytui --version`

// set by the Makefile; go install builds fall back to the module version
var version = "dev"

func getVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 1 && (args[0] == "-v" || args[0] == "--version"):
		fmt.Println("jupytui", getVersion())
		return
	case len(args) == 2 && args[0] == "exec":
		err = execAll(args[1])
	case len(args) >= 2 && args[0] == "export":
		err = export(args[1:])
	case len(args) == 2 && args[0] == "keep":
		// started by :detach, not meant to be run by hand
		err = session.RunKeeper(args[1])
	case len(args) >= 1 && args[0] == "sessions":
		err = sessions(args[1:])
	case len(args) >= 1 && args[0] == "diff":
		err = diffCmd(args[1:])
	case len(args) >= 1 && args[0] == "textconv":
		err = textconvCmd(args[1:])
	case len(args) >= 1 && args[0] == "merge":
		err = mergeCmd(args[1:])
	case len(args) >= 1 && args[0] == "clean":
		err = cleanCmd(args[1:])
	case len(args) >= 1 && args[0] == "git":
		err = gitCmd(args[1:])
	case len(args) >= 1 && args[0] == "convert":
		err = convertCmd(args[1:])
	case len(args) == 1 && args[0] != "-h" && args[0] != "--help":
		err = runTUI(args[0])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jupytui:", err)
		os.Exit(1)
	}
}

func runTUI(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// a kernel left running with :detach: take it back before loading,
	// since the keeper writes the latest outputs into the file on handover
	var attach *ui.Attach
	if s, ok := session.Load(abs); ok {
		if err := session.TakeOwnership(s, os.Getpid()); err != nil {
			return err
		}
		running, err := session.Release(s)
		if err != nil {
			return fmt.Errorf("reattaching: %w", err)
		}
		attach = &ui.Attach{Session: s, Running: running}
	}

	isMarimo := strings.EqualFold(filepath.Ext(abs), ".py")
	var nb *notebook.Notebook
	if isMarimo {
		nb, err = marimo.Load(abs)
	} else {
		nb, err = notebook.Load(abs)
	}
	if errors.Is(err, fs.ErrNotExist) {
		nb, err = notebook.New(), nil
	}
	if err != nil {
		return err
	}

	m := ui.New(abs, nb, kernel.Options{Dir: filepath.Dir(abs)}, attach)
	if isMarimo {
		m.SetMarimo()
	}
	p := tea.NewProgram(m)

	// terminal closed or we got killed politely: still clean up the kernel
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-sigs
		p.Quit()
	}()

	_, err = p.Run()
	m.Close()
	if err == nil && m.Detached() {
		fmt.Printf("kernel still running in the background. reopen with: jupytui %s\n", path)
	}
	return err
}

func sessions(args []string) error {
	list := session.List()
	if len(args) >= 2 && args[0] == "kill" {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > len(list) {
			return fmt.Errorf("no session %q, see: jupytui sessions", args[1])
		}
		session.Kill(list[n-1])
		fmt.Println("stopped", list[n-1].Notebook)
		return nil
	}
	if len(list) == 0 {
		fmt.Println("no detached kernels")
		return nil
	}
	for i, s := range list {
		state := "idle"
		if len(s.Running) > 0 {
			state = fmt.Sprintf("%d cells running at detach", len(s.Running))
		}
		fmt.Printf("%d  %s  (%s, %s, detached %s ago)\n", i+1, s.Notebook, s.Env, state, time.Since(s.Started).Round(time.Second))
	}
	return nil
}

func export(args []string) error {
	fset := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fset.String("o", "", "output path (default: notebook name with .py)")
	force := fset.Bool("f", false, "overwrite an existing file")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() != 1 {
		return errors.New("export needs exactly one notebook")
	}
	path := fset.Arg(0)
	nb, err := notebook.Load(path)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = notebook.PyPath(path)
	}
	if err := nb.ExportPercent(*out, *force); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	return nil
}

// execAll runs every code cell headless and prints outputs. Handy for
// debugging the kernel side without the TUI.
func execAll(path string) error {
	nb, err := notebook.Load(path)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	k, err := kernel.Start(kernel.Options{Dir: filepath.Dir(abs)})
	if err != nil {
		return err
	}
	defer k.Shutdown()

	for i, c := range nb.Cells {
		if c.Type != notebook.Code {
			continue
		}
		events, err := k.Execute(c.Source)
		if err != nil {
			return err
		}
		for ev := range events {
			switch ev.Kind {
			case kernel.EvStarted:
				fmt.Printf("--- cell %d [%d]\n", i, ev.ExecCount)
			case kernel.EvOutput:
				printOutput(ev.Output)
			case kernel.EvDone:
				if ev.Err != nil {
					return ev.Err
				}
				fmt.Printf("--- %s\n", ev.Status)
			}
		}
	}
	return nil
}

func printOutput(o *notebook.Output) {
	switch o.OutputType {
	case "stream":
		if o.Name == "stderr" {
			fmt.Fprint(os.Stderr, o.Text)
		} else {
			fmt.Print(o.Text)
		}
	case "error":
		for _, l := range o.Traceback {
			fmt.Println(l)
		}
	default:
		if s, ok := o.DataText("text/plain"); ok {
			fmt.Println(s)
		}
	}
}
