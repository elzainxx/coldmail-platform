// Command contract-fixtures turns a harness log into fixture files: it reads the CONTRACT-FIXTURE blocks the harness printed in record mode (CONTRACT_RECORD=1) out of a log
// (a downloaded workflow log, with or without its timestamp prefixes) and writes each block to <out>/<file>. It prints the module and route of each block, and refuses to
// overwrite a file whose content differs unless -force is given.
//
//	contract-fixtures -log harness.log -out internal/verifyadapter/testdata/contract
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/coldmail-platform/platform/internal/contractcheck"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run returns 0 when every block was written or already identical, 1 when the log or a write was refused, 2 for a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("contract-fixtures", flag.ContinueOnError)
	fl.SetOutput(stderr)
	logPath := fl.String("log", "", "the log file holding CONTRACT-FIXTURE blocks")
	outDir := fl.String("out", "", "the directory to write the fixture files to")
	force := fl.Bool("force", false, "overwrite a file whose content differs")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *logPath == "" || *outDir == "" || fl.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: contract-fixtures -log <file> -out <dir> [-force]")
		return 2
	}
	data, err := os.ReadFile(*logPath)
	if err != nil {
		fmt.Fprintln(stderr, "contract-fixtures: cannot read the log")
		return 1
	}
	recs, err := contractcheck.ParseRecorded(data)
	if err != nil {
		fmt.Fprintln(stderr, "contract-fixtures:", err)
		return 1
	}
	if err := contractcheck.WriteRecorded(*outDir, recs, *force, stdout); err != nil {
		fmt.Fprintln(stderr, "contract-fixtures:", err)
		return 1
	}
	return 0
}
