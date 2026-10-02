package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: jupytui <notebook.ipynb>")
		os.Exit(1)
	}
	fmt.Println("jupytui:", os.Args[1])
}
