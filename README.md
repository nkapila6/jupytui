# jupytui

A small terminal UI for Jupyter notebooks. Open an `.ipynb`, run cells, see output, save. No browser, no Jupyter server, no VS Code.

Built with Go and the [Charm](https://charm.sh) stack. Ships as a single static binary.

![running a notebook: progress bar, DataFrame table and an inline plot](demo/tour.gif)

## How it works

jupytui starts an `ipykernel` in your project with `uv` and talks to it directly over ZMQ (pure Go, no cgo). Your project's `.venv` packages are available in the kernel, and `ipykernel` itself is pulled in on the fly, so you don't need to add it to your project.

Notebooks are saved in the same format Jupyter writes, so diffs stay clean and the file still opens fine in Jupyter or VS Code.

## Install

Grab a binary for macOS or Linux (arm64/amd64) from [releases](https://github.com/nkapila6/jupytui/releases), untar it, and put `jupytui` somewhere on your `PATH`.

Or with Go:

```sh
go install github.com/nkapila6/jupytui/cmd/jupytui@latest
```

Or from a clone: `make install` (static, stripped build). `make dist` builds the release tarballs.

You also need [uv](https://docs.astral.sh/uv/) on your `PATH`.

## Usage

```sh
jupytui notebook.ipynb
```

If the file doesn't exist it gets created on first save. The kernel starts in the background, so you can move around while uv sets it up.

`jupytui exec notebook.ipynb` runs every code cell headless and prints the output, mostly useful for debugging.

## Python environments

By default the kernel runs in your project's environment (`uv run` in the notebook's folder). `:env` lists everything else it found:

- venvs (anything with a `pyvenv.cfg`) in the notebook's folder and its parents, its subfolders, `$VIRTUAL_ENV` and `~/.virtualenvs`
- conda / mamba / micromamba envs, if you have one of them
- Pythons uv knows about (`uv python list`)

Pick one and the kernel restarts in it. `ipykernel` gets layered on by uv, so the env doesn't need it installed and nothing is written into it. The choice is saved in the notebook's metadata (`metadata.jupytui.python`, relative to the notebook when it lives in the same folder) and used next time you open it. Jupyter and VS Code ignore that key.

![picking a Python environment and exporting to a percent .py](demo/env-export.gif)

## Export to .py

```sh
jupytui export notebook.ipynb            # writes notebook.py
jupytui export -o out.py -f notebook.ipynb
```

or `:export` / `:export other.py` inside jupytui (`:export!` to overwrite).

The output is the "percent" format: every cell starts with `# %%`, markdown cells become `# %% [markdown]` with the text as comments, and magics like `%matplotlib` or `!pip` are commented out so the file is plain Python. jupytext, VS Code and the nvim notebook plugins all read it, and `jupytext --to ipynb` turns it back into the same cells.

## Keys

Press `?` inside jupytui for the full list.

There are two levels, like jupyterlab-vim: the **notebook** (moving between cells) and **inside a cell** (a small vim). `enter` goes into a cell, `esc` comes back out.

**Notebook mode**

| key                      | action                                       |
|--------------------------|----------------------------------------------|
| `j` `k`                  | move between cells                           |
| `5j` `3k` `12G`          | jump by the line numbers, opening that cell  |
| `gg` `G`                 | first / last cell                            |
| `ctrl+d` `ctrl+u`        | scroll half a page                           |
| `enter` / `i` / `A`      | open cell in vim normal / insert / append    |
| `s`                      | flash jump (see below)                       |
| `e`                      | edit cell in nvim (see below)                |
| `ctrl+enter`             | run cell                                     |
| `shift+enter` `ctrl+r`   | run cell and move to the next                |
| `ctrl+c`                 | interrupt, or quit when nothing runs         |
| `o` `O`                  | new cell below / above                       |
| `dd` `u`                 | delete cell / undo delete                    |
| `yy` `p` `P`             | yank / paste below / paste above             |
| `J` `K`                  | move cell down / up                          |
| `M` `C` `R`              | make it markdown / code / raw                |
| `x`                      | clear cell output                            |
| `gx`                     | open the cell's image in the system viewer   |
| `ctrl+s`                 | save                                         |
| `q`                      | quit (asks again if there are unsaved changes) |

![vim editing, relative line numbers across cells and flash jumps](demo/vim.gif)

**Inside a cell** it's vim: motions (`hjkl w b e W B E 0 ^ $ gg G f t F T ; , % { }`), operators with motions and counts (`d c y > <`, `dd cc yy >> <<`, `3dw`, `d2j`), text objects (`iw aw i" a" i( a( i[ i{ ...`), `x X s S D C Y p P J ~ r`, `u` / `ctrl+r` undo/redo, `.` repeat, visual `v` / `V`. Enter after a `:` indents in Python.

`esc` goes insert to normal, and normal back out to the notebook. `ctrl+enter` runs the cell, `shift+enter` runs and moves on, `ctrl+e` opens it in nvim.

**Completions** pop up in insert mode after `.` or a couple of letters (or on `tab` / `ctrl+space`). They come from two places: the running kernel, which knows what's actually in memory (`df.` lists your real columns), and [basedpyright](https://github.com/DetachHead/basedpyright), which knows code that hasn't run yet. Kernel results come first. `ctrl+n` / `ctrl+p` (or `tab`, arrows) to move, `enter` to accept, `esc` to close the menu. `tab` still indents when there's nothing to complete.

![kernel and LSP completions, signature help, diagnostics, hover and go to definition](demo/completions.gif)

**LSP**: basedpyright starts the first time you open a code cell, via `uvx` (first run downloads it once; nothing goes into your project, and you don't need node). It sees all code cells as one file and uses the same Python env as the kernel. You get:

- diagnostics: the line number turns red/yellow, the range is underlined, the message shows in the footer when your cursor is on the line, and the footer counts them. `]d` / `[d` jump between them across cells
- `K` hover docs, `gd` go to definition (jumps to the cell, or opens library code in your host nvim), signature help after `(` and `,`

It runs basedpyright's `basic` checks with "unused expression" off, since a bare `df.head()` at the end of a cell is the point of a notebook. `:set nolsp` turns it off, `:set nodiag` just hides diagnostics.

**Line numbers** run across the whole notebook as if it were one buffer, relative by default like LazyVim. There's one numbering for everything: `5j` moves exactly the number of lines shown, crossing into other cells, in a cell or from the notebook view (where it opens the cell at that line), and `:42` / `42G` go to notebook line 42. In the notebook view the numbers count from the selected cell's first line. Outputs don't count as lines.

**Flash jump**: press `s`, type a couple of characters, and every match on screen gets a label; type the label to land there (inside the cell, in vim normal mode). Like flash.nvim, labels never use a letter that could continue your search, `enter` takes the closest match, `esc` cancels.

**Commands**

| command                  | action                              |
|--------------------------|-------------------------------------|
| `:w` `:q` `:q!` `:wq`    | the usual                           |
| `:42`                    | go to notebook line 42              |
| `:runall`                | run every code cell                 |
| `:clear`                 | clear all outputs                   |
| `:export[!] [file.py]`   | export as a `# %%` .py              |
| `:interrupt`             | interrupt the kernel                |
| `:restart`               | restart the kernel                  |
| `:env`                   | pick the Python environment         |
| `:set nu` `:set nonu`    | line numbers on / off               |
| `:set rnu` `:set nornu`  | relative line numbers on / off      |
| `:set novim` `:set vim`  | plain editing in cells instead of vim |
| `:set nolsp` `:set lsp`  | language server off / on            |
| `:set nodiag` `:set diag`| hide / show diagnostics             |

`ctrl+enter` and `shift+enter` need a terminal that supports the kitty keyboard protocol (Ghostty, kitty, WezTerm, recent iTerm2). In the notebook, `ctrl+r` runs and moves on everywhere.

## Outputs

- stdout/stderr stream in live; `\r` progress bars (tqdm) redraw in place
- pandas and polars DataFrames are drawn as real tables (index, dtypes, MultiIndex, the `...` rows); wide ones keep readable columns and drop the rest behind `…`
- tracebacks keep their colours
- other HTML shows as text, markdown output is rendered
- plots and images (matplotlib, seaborn, PIL, anything that outputs PNG/JPEG) show inline:
  - **kitty and Ghostty**: full resolution, via the kitty graphics protocol
  - **WezTerm, iTerm2, foot**: full resolution, via sixel
  - **everywhere else**, including inside nvim's terminal and tmux (neither passes images through): half-block characters, two pixels per cell. Low-res but readable.

  `gx` on the cell opens the full image in your system viewer (also how you see SVG output). jupytui picks the mode from your terminal; force one with `JUPYTUI_IMAGES=kitty|sixel|blocks` or `:set images=...`.

## Neovim (LazyVim)

Same idea as `<leader>gg` for lazygit. Drop this in `lua/config/keymaps.lua`:

```lua
vim.keymap.set("n", "<leader>jn", function()
  Snacks.terminal({ "jupytui", vim.fn.expand("%:p") }, { cwd = LazyVim.root() })
end, { desc = "jupytui (current notebook)" })
```

Open a notebook buffer, hit `<leader>jn`, and jupytui comes up in a float.

Pressing `e` on a cell (or `ctrl+e` inside one) while in nvim opens it in your actual nvim, not a nested one: the cell lands in a new tab as a `.py` file so your LSP, treesitter and keymaps all work. Every `:w` syncs straight back into the notebook, and `:wq` drops you back in the jupytui float. This works because nvim sets `$NVIM` for its terminals.

Outside nvim, `e` opens `$VISUAL` / `$EDITOR` (falling back to `vi`) full screen.

## Recordings

The GIFs are made with [VHS](https://github.com/charmbracelet/vhs) from the tapes in `demo/`. `make demos` re-records them (needs `vhs` and `uv`; the demo notebook has its own uv project in `demo/`).

## Status

Early but usable. Not there yet: images and HTML outputs show as placeholders, `input()` isn't supported, and only Python kernels (via uv) are wired up.
