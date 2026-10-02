# Opening a notebook and running it: progress bars, a DataFrame table
# and a plot drawn inline. Run from the repo root: vhs demo/tour.tape
Output demo/tour.gif
Require uv
Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 820
Set Padding 12
Set Theme TokyoNight
Set TypingSpeed 70ms

Hide
Type "make build >/dev/null && export PATH=$PWD:$PATH COLORTERM=truecolor && cd demo && cp tour.ipynb scratch.ipynb && uv run python -c 'import pandas, matplotlib.pyplot' && clear"
Enter
Sleep 12s
Show

Type "jupytui scratch.ipynb"
Sleep 400ms
Enter
Sleep 3s

# run every cell, top to bottom
Type "j"
Sleep 600ms
Ctrl+R
Sleep 2.5s
Ctrl+R
Sleep 2s
Ctrl+R
Sleep 2s
Ctrl+R
Sleep 5s

# look at the table, then the plot
Type "k"
Sleep 400ms
Type "k"
Sleep 3s
Type "j"
Sleep 3.5s

Type "q"
Sleep 300ms
Type "q"
Sleep 1s
Hide
Type "rm -f scratch.ipynb"
Enter
