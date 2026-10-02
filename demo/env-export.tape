# Picking the Python environment with :env, then :export to a
# percent-format .py. Run from the repo root: vhs demo/env-export.tape
Output demo/env-export.gif
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

Type ":env"
Sleep 300ms
Enter
Sleep 3s
Type "j"
Sleep 800ms
Enter
Sleep 4s

Type ":export"
Sleep 300ms
Enter
Sleep 1.5s
Type ":q!"
Sleep 300ms
Enter
Sleep 1s

Type "head -22 scratch.py"
Sleep 300ms
Enter
Sleep 4s
Hide
Type "rm -f scratch.ipynb scratch.py"
Enter
