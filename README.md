# jupytui

A small terminal UI for Jupyter notebooks. Open an `.ipynb`, run cells, see output, save. No browser, no Jupyter server, no VS Code.

Built with Go and the [Charm](https://charm.sh) stack. Ships as a single static binary.

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
| `j` `k`, `5j` `3k`       | move between cells (counts match the gutter) |
| `gg` `G`, `12G`          | first / last / nth cell                      |
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
| `ctrl+s`                 | save                                         |
| `q`                      | quit (asks again if there are unsaved changes) |

**Inside a cell** it's vim: motions (`hjkl w b e W B E 0 ^ $ gg G f t F T ; , % { }`), operators with motions and counts (`d c y > <`, `dd cc yy >> <<`, `3dw`, `d2j`), text objects (`iw aw i" a" i( a( i[ i{ ...`), `x X s S D C Y p P J ~ r`, `u` / `ctrl+r` undo/redo, `.` repeat, visual `v` / `V`. Enter after a `:` indents in Python.

`esc` goes insert to normal, and normal back out to the notebook. `ctrl+enter` runs the cell, `shift+enter` runs and moves on, `ctrl+e` opens it in nvim.

**Line numbers** run across the whole notebook as if it were one buffer, relative by default like LazyVim. So `5j` inside a cell moves exactly the number of lines shown, crossing into other cells, and `:42` / `42G` go to notebook line 42. Outputs don't count as lines.

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

`ctrl+enter` and `shift+enter` need a terminal that supports the kitty keyboard protocol (Ghostty, kitty, WezTerm, recent iTerm2). In the notebook, `ctrl+r` runs and moves on everywhere.

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

## Status

Early but usable. Not there yet: images and HTML outputs show as placeholders, `input()` isn't supported, and only Python kernels (via uv) are wired up.
