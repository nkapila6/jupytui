package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nkapila6/jupytui/internal/nbdiff"
	"github.com/nkapila6/jupytui/internal/notebook"
)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// jupytui diff a.ipynb b.ipynb
func diffCmd(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	outputs := fs.Bool("outputs", false, "show output changes in full")
	color := fs.String("color", "auto", "auto, always or never")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: jupytui diff [--outputs] a.ipynb b.ipynb")
	}
	a, err := notebook.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := notebook.Load(fs.Arg(1))
	if err != nil {
		return err
	}
	useColor := *color == "always" || *color == "auto" && isTerminal(os.Stdout)
	if nbdiff.Render(os.Stdout, a, b, useColor, *outputs) {
		os.Exit(1) // like diff(1): 1 means they differ
	}
	return nil
}

// jupytui textconv nb.ipynb, for git's diff driver
func textconvCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: jupytui textconv nb.ipynb")
	}
	nb, err := notebook.Load(args[0])
	if err != nil {
		// not a notebook we can read: let git diff the raw file
		b, rerr := os.ReadFile(args[0])
		if rerr != nil {
			return err
		}
		_, werr := os.Stdout.Write(b)
		return werr
	}
	_, err = io.WriteString(os.Stdout, nbdiff.Text(nb))
	return err
}

// jupytui merge BASE OURS THEIRS [PATH], git's merge driver (%O %A %B %P).
// The result goes into OURS; exit 1 when there were conflicts.
func mergeCmd(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: jupytui merge base ours theirs [path]")
	}
	base, err := notebook.Load(args[0])
	if err != nil {
		return err
	}
	ours, err := notebook.Load(args[1])
	if err != nil {
		return err
	}
	theirs, err := notebook.Load(args[2])
	if err != nil {
		return err
	}
	merged, conflicts := nbdiff.Merge(base, ours, theirs)
	if err := merged.Save(args[1]); err != nil {
		return err
	}
	if conflicts > 0 {
		name := args[1]
		if len(args) > 3 {
			name = args[3]
		}
		fmt.Fprintf(os.Stderr, "jupytui: %d conflicting cell%s in %s, look for <<<<<<< in the cells\n", conflicts, map[bool]string{true: "s"}[conflicts != 1], name)
		os.Exit(1)
	}
	return nil
}

// jupytui clean [--stdin] [files...]: drop outputs and execution counts
func cleanCmd(args []string) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	stdin := fs.Bool("stdin", false, "read a notebook from stdin, write it to stdout (git filter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		nb, err := notebook.Parse(b)
		if err != nil {
			// a git filter must never eat a file it doesn't understand
			_, werr := os.Stdout.Write(b)
			return werr
		}
		out, err := nb.Stripped().Marshal()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: jupytui clean [--stdin] [notebook.ipynb ...]")
	}
	for _, p := range fs.Args() {
		nb, err := notebook.Load(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := nb.Stripped().Save(p); err != nil {
			return err
		}
		fmt.Println("cleaned", p)
	}
	return nil
}

// jupytui git setup [--strip-outputs]: wire the diff/merge drivers (and
// optionally the output-stripping filter) into the current repo.
func gitCmd(args []string) error {
	if len(args) == 0 || args[0] != "setup" {
		return errors.New("usage: jupytui git setup [--strip-outputs]")
	}
	fs := flag.NewFlagSet("git setup", flag.ContinueOnError)
	strip := fs.Bool("strip-outputs", false, "commit notebooks without outputs (they stay in your working copy)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return errors.New("not inside a git repository")
	}
	root := strings.TrimSpace(string(top))
	self := "jupytui"
	cfg := [][2]string{
		{"diff.jupytui.textconv", self + " textconv"},
		{"merge.jupytui.name", "jupytui notebook merge"},
		{"merge.jupytui.driver", self + " merge %O %A %B %P"},
	}
	attr := "*.ipynb diff=jupytui merge=jupytui"
	if *strip {
		cfg = append(cfg, [2]string{"filter.jupytui.clean", self + " clean --stdin"}, [2]string{"filter.jupytui.smudge", "cat"})
		attr += " filter=jupytui"
	}
	for _, kv := range cfg {
		if out, err := exec.Command("git", "-C", root, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %v %s", kv[0], err, out)
		}
	}
	// one line for *.ipynb: replace ours if it's there (so --strip-outputs
	// can be added later), otherwise append
	path := filepath.Join(root, ".gitattributes")
	existing, _ := os.ReadFile(path)
	var lines []string
	replaced := false
	for _, l := range strings.Split(strings.TrimRight(string(existing), "\n"), "\n") {
		if strings.HasPrefix(l, "*.ipynb ") && strings.Contains(l, "=jupytui") {
			if !replaced {
				lines = append(lines, attr)
				replaced = true
			}
			continue
		}
		if l != "" || len(lines) > 0 {
			lines = append(lines, l)
		}
	}
	if !replaced {
		lines = append(lines, attr)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Println("set up git config and .gitattributes in", root)
	fmt.Println("commit .gitattributes; everyone else runs `jupytui git setup` once (git config isn't shared)")
	return nil
}
