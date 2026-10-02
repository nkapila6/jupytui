"""Play the tour in a real terminal, for screen recordings.

VHS records through a browser terminal that can't show kitty or sixel
images, so this runs jupytui in a pty inside the terminal you're in
(kitty, Ghostty, WezTerm...) and types the keys itself. Output goes
straight to your terminal, so plots show up as real images.

    make build
    /usr/bin/python3 demo/play.py

Start your screen recording, press enter, and stop it once jupytui quits.
Stdlib only. Use a python that doesn't set PYTHONPATH for its children
(some homebrew setups do), or the kernel picks up the wrong packages.
"""

import fcntl
import os
import pty
import select
import shutil
import sys
import termios
import time
import tty

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(HERE, "..", "jupytui")
NB = os.path.join(HERE, "scratch_play.ipynb")

CTRL_R = b"\x12"

# (seconds to wait first, keys)
SCRIPT = [
    (1.5, b"j"),
    (0.8, CTRL_R),  # tqdm progress bar
    (2.5, CTRL_R),  # DataFrame table
    (2.0, CTRL_R),  # growth() + sorted table
    (2.0, CTRL_R),  # bar chart
    (5.0, b"k"),
    (0.5, b"k"),  # back to the table
    (3.0, b"j"),  # and the chart
    (5.0, b":q!\r"),
]


def main():
    if not os.path.exists(BIN):
        sys.exit("build first: make build")
    # keep a PYTHONPATH from the launching python away from the kernel
    for k in ("PYTHONPATH", "PYTHONHOME"):
        os.environ.pop(k, None)

    shutil.copy(os.path.join(HERE, "tour.ipynb"), NB)
    # warm imports so the first cells don't sit there compiling bytecode
    os.system(f"cd {HERE} && uv run --quiet python -c 'import pandas, matplotlib.pyplot'")
    input("start recording, then press enter ")
    os.system("clear")

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(HERE)
        os.execv(BIN, [BIN, NB])

    # same size as the real terminal, pixel size included (for images)
    ws = fcntl.ioctl(sys.stdout.fileno(), termios.TIOCGWINSZ, b"\0" * 8)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, ws)

    old = termios.tcgetattr(sys.stdin)
    tty.setraw(sys.stdin)
    steps = list(SCRIPT)
    due = time.monotonic() + steps[0][0]
    try:
        while True:
            timeout = max(0.0, due - time.monotonic()) if steps else 0.5
            r, _, _ = select.select([fd, sys.stdin], [], [], timeout)
            if fd in r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    break
                if not data:
                    break
                os.write(sys.stdout.fileno(), data)
            if sys.stdin in r:
                # the terminal's replies to jupytui's queries
                os.write(fd, os.read(sys.stdin.fileno(), 4096))
            if steps and time.monotonic() >= due:
                os.write(fd, steps.pop(0)[1])
                if steps:
                    due = time.monotonic() + steps[0][0]
    finally:
        termios.tcsetattr(sys.stdin, termios.TCSADRAIN, old)
        os.waitpid(pid, 0)
        os.remove(NB)


if __name__ == "__main__":
    main()
