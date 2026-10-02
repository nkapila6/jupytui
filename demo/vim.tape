# Vim inside cells, line numbers that run across the whole notebook,
# counted jumps across cells and flash-style s jumps.
# Run from the repo root: vhs demo/vim.tape
Output demo/vim.gif
Require uv
Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 820
Set Padding 12
Set Theme TokyoNight
Set TypingSpeed 90ms

Hide
Type "make build >/dev/null && export PATH=$PWD:$PATH COLORTERM=truecolor && cd demo && cp tour.ipynb scratch.ipynb && uv run python -c 'import pandas, matplotlib.pyplot' && clear"
Enter
Sleep 10s
Show

Type "jupytui scratch.ipynb"
Sleep 400ms
Enter
Sleep 3s

# into the growth() cell, vim normal mode
Type "jjj"
Sleep 800ms
Enter
Sleep 1.5s

# rename growth -> grow, here and where it's used
Type "w"
Sleep 500ms
Type "ciw"
Sleep 400ms
Type "grow"
Escape
Sleep 1s
Type "jjj"
Sleep 400ms
Type "f(l"
Sleep 500ms
Type "ciwgrow"
Escape
Sleep 1.2s

# undo / redo
Type "u"
Sleep 900ms
Ctrl+R
Sleep 1.2s

# relative numbers span cells: 9k lands in the DataFrame cell above
Type "9k"
Sleep 1.8s

# flash: s, type a few letters, then the label
Type "s"
Sleep 500ms
Type@150ms "plt.t"
Sleep 1.5s
Type "a"
Sleep 1.5s

Escape
Sleep 400ms
Escape
Sleep 400ms
Type ":q!"
Sleep 300ms
Enter
Sleep 1s
Hide
Type "rm -f scratch.ipynb"
Enter
