package notebook

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// magic lines (%time, %%bash, !pip, ?obj) aren't valid python, so they
// get commented out like jupytext does
var magicLine = regexp.MustCompile(`^\s*(%|!|\?)`)

// Percent renders the notebook as a .py in the "percent" format
// (# %% cell markers) that jupytext, VS Code and the nvim notebook
// plugins understand. Markdown and raw cells become comments.
func (nb *Notebook) Percent() string {
	var b strings.Builder
	for i, c := range nb.Cells {
		if i > 0 {
			b.WriteString("\n")
		}
		switch c.Type {
		case Markdown:
			b.WriteString("# %% [markdown]\n")
			writeCommented(&b, c.Source)
		case Raw:
			b.WriteString("# %% [raw]\n")
			writeCommented(&b, c.Source)
		default:
			b.WriteString("# %%\n")
			if c.Source == "" {
				continue
			}
			for _, l := range strings.Split(c.Source, "\n") {
				if magicLine.MatchString(l) {
					l = "# " + l
				}
				b.WriteString(l + "\n")
			}
		}
	}
	return b.String()
}

func writeCommented(b *strings.Builder, src string) {
	if src == "" {
		return
	}
	for _, l := range strings.Split(src, "\n") {
		if l == "" {
			b.WriteString("#\n")
		} else {
			b.WriteString("# " + l + "\n")
		}
	}
}

// ExportPercent writes Percent() to path. Without force it refuses to
// replace an existing file, since that's likely someone's script.
func (nb *Notebook) ExportPercent(path string, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w, use force to overwrite", filepath.Base(path), fs.ErrExist)
		}
		return err
	}
	if _, err := f.WriteString(nb.Percent()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// PyPath is the default export path: same name, .py extension.
func PyPath(ipynb string) string {
	return strings.TrimSuffix(ipynb, filepath.Ext(ipynb)) + ".py"
}
