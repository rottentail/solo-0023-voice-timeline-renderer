package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"voice-timeline-renderer/internal/checkflow"
)

func main() {
	workflow := flag.String("workflow", "", "workflow identifier to execute")
	flag.Parse()
	if *workflow == "" {
		fmt.Fprintln(os.Stderr, "-workflow is required")
		os.Exit(2)
	}
	if err := checkflow.Run(context.Background(), *workflow); err != nil {
		fmt.Fprintf(os.Stderr, "workflow %s failed: %v\n", *workflow, err)
		os.Exit(1)
	}
	fmt.Printf("workflow %s passed\n", *workflow)
}
