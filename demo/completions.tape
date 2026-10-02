# Completions from the kernel (live objects) and basedpyright, signature
# help, diagnostics, hover and go to definition.
# Run from the repo root: vhs demo/completions.tape
Output demo/completions.gif
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

# run the setup cells so the kernel knows df
Type "j"
Sleep 300ms
Ctrl+R
Sleep 2s
Ctrl+R
Sleep 1.5s
Ctrl+R
Sleep 2s

# new cell at the end
Type "G"
Sleep 300ms
Type "o"
Sleep 3s

# kernel completions know df's real columns
Type "df."
Sleep 1.8s
Type "sa"
Sleep 1s
Enter
Sleep 600ms
Type ".me"
Sleep 1.5s
Enter
Type "()"
Sleep 800ms
Enter

# signature help from basedpyright
Type "growth("
Sleep 2s
Type "2)"
Enter
Sleep 400ms

# a typo shows up as a diagnostic
Type "print(totl)"
Sleep 400ms
Escape
Sleep 2.5s

# hover and go to definition on growth
Type "k0"
Sleep 400ms
Type "K"
Sleep 3s
Type "0"
Sleep 300ms
Type "gd"
Sleep 2.5s

Escape
Sleep 400ms
Type ":q!"
Sleep 300ms
Enter
Sleep 1s
Hide
Type "rm -f scratch.ipynb"
Enter
