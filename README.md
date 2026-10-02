# jupytui

A small terminal UI for Jupyter notebooks. Open an `.ipynb`, run cells, see output, save. No browser, no Jupyter server, no VS Code.

Built with Go and the [Charm](https://charm.sh) stack. Ships as a single static binary.

## How it works

jupytui starts an `ipykernel` in your project with `uv` and talks to it directly over ZMQ (pure Go, no cgo). Your project's `.venv` packages are available in the kernel, and `ipykernel` itself is pulled in on the fly, so you don't need to add it to your project.

## Install

```sh
go install github.com/nkapila6/jupytui/cmd/jupytui@latest
```

You also need [uv](https://docs.astral.sh/uv/) on your `PATH`.

## Usage

```sh
jupytui notebook.ipynb
```

## Neovim (LazyVim)

Same idea as `<leader>gg` for lazygit. Drop this in `lua/config/keymaps.lua`:

```lua
vim.keymap.set("n", "<leader>jn", function()
  Snacks.terminal({ "jupytui", vim.fn.expand("%:p") }, { cwd = LazyVim.root() })
end, { desc = "jupytui (current notebook)" })
```

## Status

Early. Things are being built step by step.
