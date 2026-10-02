package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
)

func main() {
	args := os.Args[1:]
	if len(args) == 2 && args[0] == "exec" {
		if err := execAll(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "jupytui:", err)
			os.Exit(1)
		}
		return
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jupytui <notebook.ipynb>\n       jupytui exec <notebook.ipynb>")
		os.Exit(1)
	}
	fmt.Println("jupytui:", args[0])
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
