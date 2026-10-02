// Pure offline contract report for source CI; this is not an Archivist runtime command.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	p, err := mosaicevent.New()
	if err != nil {
		return err
	}
	report, err := p.Report()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
