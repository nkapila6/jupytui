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

## Keys

Press `?` inside jupytui for the full list.

**Normal mode**

| key                      | action                                  |
|--------------------------|-----------------------------------------|
| `j` `k` / arrows         | move between cells                      |
| `gg` `G`                 | first / last cell                       |
| `ctrl+d` `ctrl+u`        | scroll half a page                      |
| `enter` `i` / `A`        | edit cell, cursor at start / end        |
| `e`                      | edit cell in nvim (see below)           |
| `ctrl+r` `shift+enter`   | run cell and move to the next           |
| `ctrl+c`                 | interrupt, or quit when nothing runs    |
| `o` `O`                  | new cell below / above                  |
| `dd` `u`                 | delete cell / undo delete               |
| `yy` `p` `P`             | yank / paste below / paste above        |
| `J` `K`                  | move cell down / up                     |
| `M` `C` `R`              | make it markdown / code / raw           |
| `x`                      | clear cell output                       |
| `ctrl+s`                 | save                                    |
| `q`                      | quit (asks again if there are unsaved changes) |

**Edit mode**: `esc` back to normal, `ctrl+r` run, `ctrl+e` open in nvim, `ctrl+s` save.

**Commands**

| command                  | action                       |
|--------------------------|------------------------------|
| `:w` `:q` `:q!` `:wq`    | the usual                    |
| `:runall`                | run every code cell          |
| `:clear`                 | clear all outputs            |
| `:interrupt`             | interrupt the kernel         |
| `:restart`               | restart the kernel           |
| `:12`                    | jump to cell 12              |

`shift+enter` needs a terminal that supports the kitty keyboard protocol (Ghostty, kitty, WezTerm, recent iTerm2). `ctrl+r` works everywhere.

## Neovim (LazyVim)

Same idea as `<leader>gg` for lazygit. Drop this in `lua/config/keymaps.lua`:

```lua
vim.keymap.set("n", "<leader>jn", function()
  Snacks.terminal({ "jupytui", vim.fn.expand("%:p") }, { cwd = LazyVim.root() })
end, { desc = "jupytui (current notebook)" })
```

Open a notebook buffer, hit `<leader>jn`, and jupytui comes up in a float.

Pressing `e` on a cell while inside nvim opens it in your actual nvim, not a nested one: the cell lands in a new tab as a `.py` file so your LSP, treesitter and keymaps all work. Every `:w` syncs straight back into the notebook, and `:wq` drops you back in the jupytui float. This works because nvim sets `$NVIM` for its terminals.

Outside nvim, `e` opens `$VISUAL` / `$EDITOR` (falling back to `vi`) full screen.

## Status

Early but usable. Not there yet: images and HTML outputs show as placeholders, `input()` isn't supported, and only Python kernels (via uv) are wired up.
