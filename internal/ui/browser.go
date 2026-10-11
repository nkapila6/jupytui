package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nkapila6/jupytui/internal/notebook"
)

// Outputs that need JavaScript (plotly, altair, bokeh, folium...) can't
// run in a terminal. gx writes them out as a page and opens the browser.

// pinned so a page opened next year still renders the same
const (
	plotlyJS     = "https://cdn.jsdelivr.net/npm/plotly.js-dist-min@3.0.1/plotly.min.js"
	vegaJS       = "https://cdn.jsdelivr.net/npm/vega@5.30.0/build/vega.min.js"
	vegaLiteJS   = "https://cdn.jsdelivr.net/npm/vega-lite@5.21.0/build/vega-lite.min.js"
	vegaEmbedJS  = "https://cdn.jsdelivr.net/npm/vega-embed@6.26.0/build/vega-embed.min.js"
	plotlyMime   = "application/vnd.plotly.v1+json"
	vegaLitePref = "application/vnd.vegalite.v"
	vegaPref     = "application/vnd.vega.v"
)

// browserKind says whether an output only makes sense in a browser.
func browserKind(o *notebook.Output) (string, bool) {
	if _, ok := o.Data[plotlyMime]; ok {
		return "plotly figure", true
	}
	for k := range o.Data {
		if strings.HasPrefix(k, vegaLitePref) || strings.HasPrefix(k, vegaPref) {
			return "vega chart", true
		}
		if strings.HasPrefix(k, "application/vnd.bokehjs") {
			return "bokeh plot", true
		}
	}
	if h, ok := o.DataText("text/html"); ok && strings.Contains(strings.ToLower(h), "<script") {
		return "html with scripts", true
	}
	return "", false
}

// openOutput is gx: an interactive chart goes to the browser, otherwise
// the first image goes to the image viewer.
func (m *Model) openOutput() {
	for _, o := range m.cell().Outputs {
		if _, ok := browserKind(o); !ok {
			continue
		}
		path, err := m.tempFile("output-%d.html", []byte(m.page(o)))
		if err == nil {
			err = openPath(path)
		}
		if err != nil {
			m.msg = "gx: " + err.Error()
			return
		}
		m.msg = "opened " + filepath.Base(path) + " in the browser"
		return
	}
	m.openImage()
}

// page builds a standalone html page for an output.
func (m *Model) page(o *notebook.Output) string {
	var body string
	switch {
	case o.Data[plotlyMime] != nil:
		body = fmt.Sprintf(`<div id="fig" style="width:100%%;height:96vh"></div>
<script src="%s"></script>
<script>
const f = %s;
Plotly.newPlot("fig", f.data || [], f.layout || {}, Object.assign({responsive: true}, f.config || {}));
</script>`, plotlyJS, o.Data[plotlyMime])
	default:
		for k, v := range o.Data {
			if strings.HasPrefix(k, vegaLitePref) || strings.HasPrefix(k, vegaPref) {
				body = fmt.Sprintf(`<div id="vis"></div>
<script src="%s"></script><script src="%s"></script><script src="%s"></script>
<script>vegaEmbed("#vis", %s);</script>`, vegaJS, vegaLiteJS, vegaEmbedJS, v)
			}
		}
	}
	if body == "" {
		// plain html: bokeh and friends load their library in an earlier
		// output (output_notebook), so bring every script output along
		var b strings.Builder
	cells:
		for _, c := range m.nb.Cells {
			for _, prev := range c.Outputs {
				if prev == o {
					break cells
				}
				if js, ok := prev.DataText("application/javascript"); ok {
					b.WriteString("<script>\n" + js + "\n</script>\n")
				}
			}
		}
		if js, ok := o.DataText("application/javascript"); ok {
			b.WriteString("<script>\n" + js + "\n</script>\n")
		}
		h, _ := o.DataText("text/html")
		b.WriteString(h)
		body = b.String()
	}
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>` + filepath.Base(m.path) + `</title>
<style>body{font-family:system-ui,sans-serif;margin:16px}</style></head>
<body>
` + body + `
</body></html>
`
}

// tempFile writes data to a fresh file in the session's temp dir. name
// has a %d for a counter.
func (m *Model) tempFile(name string, data []byte) (string, error) {
	if m.imgDir == "" {
		dir, err := os.MkdirTemp("", "jupytui-img-")
		if err != nil {
			return "", err
		}
		m.imgDir = dir
	}
	m.imgN++
	path := filepath.Join(m.imgDir, fmt.Sprintf(name, m.imgN))
	return path, os.WriteFile(path, data, 0o600)
}

func openPath(path string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, path).Start()
}
