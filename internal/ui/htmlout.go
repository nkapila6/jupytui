package ui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/net/html"
)

// HTML outputs: pandas and polars DataFrames (both use
// class="dataframe") are drawn as terminal tables; any other HTML-only
// output falls back to its text.

type tcell struct {
	text   string
	header bool // <th>: column names and the index
}

type htmlTable struct {
	pre, post []string // text around the table: polars' shape, pandas' "n rows × m columns"
	head      [][]tcell
	body      [][]tcell
}

func isDataFrameHTML(s string) bool {
	return strings.Contains(s, `class="dataframe"`)
}

func parseDataFrame(src string) (htmlTable, bool) {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return htmlTable{}, false
	}
	var t htmlTable
	found := false
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "style", "script":
				return
			case "table":
				if !found && hasClass(n, "dataframe") {
					found = true
					t.head, t.body = tableRows(n)
					return
				}
			}
		}
		if n.Type == html.TextNode {
			if s := strings.Join(strings.Fields(n.Data), " "); s != "" {
				if found {
					t.post = append(t.post, s)
				} else {
					t.pre = append(t.pre, s)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return t, found && len(t.head)+len(t.body) > 0
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

// tableRows flattens thead/tbody rows, expanding colspan and rowspan
// (pandas uses both for MultiIndex) into blank cells.
func tableRows(table *html.Node) (head, body [][]tcell) {
	spans := map[int]int{} // column -> rows still covered by a rowspan above
	var walk func(n *html.Node, inHead bool)
	walk = func(n *html.Node, inHead bool) {
		if n.Type == html.ElementNode && n.Data == "thead" {
			inHead = true
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			var row []tcell
			col := 0
			skip := func() {
				for spans[col] > 0 {
					spans[col]--
					row = append(row, tcell{})
					col++
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type != html.ElementNode || (c.Data != "td" && c.Data != "th") {
					continue
				}
				skip()
				cs, rs := attrInt(c, "colspan"), attrInt(c, "rowspan")
				row = append(row, tcell{text: nodeText(c), header: c.Data == "th"})
				if rs > 1 {
					spans[col] = rs - 1
				}
				col++
				for range cs - 1 {
					row = append(row, tcell{header: c.Data == "th"})
					col++
				}
			}
			skip()
			if inHead {
				head = append(head, row)
			} else {
				body = append(body, row)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inHead)
		}
	}
	walk(table, false)
	return head, body
}

func attrInt(n *html.Node, key string) int {
	for _, a := range n.Attr {
		if a.Key == key {
			if v, err := strconv.Atoi(a.Val); err == nil && v > 0 {
				return v
			}
		}
	}
	return 1
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func isNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return err == nil || s == "NaN" || s == "nan" || s == "inf" || s == "-inf"
}

// renderTable draws t at most width columns wide, shrinking the widest
// columns first and dropping columns off the right if it still doesn't fit.
func (m *Model) renderTable(t htmlTable, width int) string {
	rows := append(append([][]tcell{}, t.head...), t.body...)
	ncols := 0
	for _, r := range rows {
		ncols = max(ncols, len(r))
	}
	if ncols == 0 {
		return ""
	}
	for i := range rows {
		for len(rows[i]) < ncols {
			rows[i] = append(rows[i], tcell{})
		}
	}
	colW := make([]int, ncols)
	numeric := make([]bool, ncols)
	for c := range ncols {
		numeric[c] = true
		seen := false
		for i, r := range rows {
			colW[c] = max(colW[c], min(ansi.StringWidth(r[c].text), 40))
			if i >= len(t.head) && r[c].text != "" && r[c].text != "..." && r[c].text != "…" {
				seen = true
				numeric[c] = numeric[c] && isNumeric(r[c].text)
			}
		}
		numeric[c] = numeric[c] && seen
		colW[c] = max(colW[c], 1)
	}
	total := func(n int) int {
		t := 1
		for _, w := range colW[:n] {
			t += w + 3
		}
		return t
	}
	// shrink the widest columns, but not below something readable;
	// past that it's better to drop columns than show "col…" everywhere
	floor := make([]int, ncols)
	for c := range ncols {
		floor[c] = min(colW[c], 10)
	}
	for total(ncols) > width {
		widest := -1
		for c := range ncols {
			if colW[c] > floor[c] && (widest < 0 || colW[c] > colW[widest]) {
				widest = c
			}
		}
		if widest < 0 {
			break
		}
		colW[widest]--
	}
	// still too wide: keep what fits plus a "…" column
	shown := ncols
	for shown > 1 && total(shown)+4 > width && total(ncols) > width {
		shown--
	}
	dropped := shown < ncols

	border := m.st.faint
	line := func(l, mid, r string) string {
		var b strings.Builder
		b.WriteString(l)
		for c := range shown {
			if c > 0 {
				b.WriteString(mid)
			}
			b.WriteString(strings.Repeat("─", colW[c]+2))
		}
		if dropped {
			b.WriteString(mid + "───")
		}
		return border.Render(b.String() + r)
	}
	bar := border.Render("│")

	var out []string
	for _, p := range t.pre {
		out = append(out, m.st.dim.Render(p))
	}
	out = append(out, line("╭", "┬", "╮"))
	for i, r := range rows {
		var b strings.Builder
		b.WriteString(bar)
		for c := range shown {
			text := ansi.Truncate(r[c].text, colW[c], "…")
			pad := colW[c] - ansi.StringWidth(text)
			if numeric[c] && i >= len(t.head) {
				text = strings.Repeat(" ", pad) + text
			} else {
				text += strings.Repeat(" ", pad)
			}
			switch {
			case i < len(t.head) && !r[c].header:
				text = m.st.dim.Render(text) // polars dtype row
			case r[c].header:
				text = lipglossBold(text)
			}
			b.WriteString(" " + text + " " + bar)
		}
		if dropped {
			b.WriteString(m.st.dim.Render(" … ") + bar)
		}
		out = append(out, b.String())
		if i == len(t.head)-1 && len(t.body) > 0 {
			out = append(out, line("├", "┼", "┤"))
		}
	}
	out = append(out, line("╰", "┴", "╯"))
	for _, p := range t.post {
		out = append(out, m.st.dim.Render(p))
	}
	return strings.Join(out, "\n")
}

func lipglossBold(s string) string { return "\x1b[1m" + s + "\x1b[22m" }

// htmlText is the readable text of an arbitrary HTML output.
func htmlText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "style", "script", "head":
				return
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "pre":
				defer b.WriteString("\n")
			case "td", "th":
				defer b.WriteString("  ")
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	var lines []string
	for _, l := range strings.Split(b.String(), "\n") {
		if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
