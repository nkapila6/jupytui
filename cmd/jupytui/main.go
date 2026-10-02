package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
	"github.com/nkapila6/jupytui/internal/ui"
)

const usage = `usage: jupytui <notebook.ipynb>        open (or create) a notebook
       jupytui exec <notebook.ipynb>   run all cells headless and print outputs`

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "exec":
		err = execAll(args[1])
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
	nb, err := notebook.Load(abs)
	if errors.Is(err, fs.ErrNotExist) {
		nb, err = notebook.New(), nil
	}
	if err != nil {
		return err
	}

	m := ui.New(abs, nb, kernel.Options{Dir: filepath.Dir(abs)})
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
	return err
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
