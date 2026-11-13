package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"voice-timeline-renderer/internal/repro"
)

// reproverify is the third-party entry point. It reads only a bundle directory
// and never needs the producer's original working directory.
func main() {
	action := flag.String("action", "verify", "verify | audit | fingerprint")
	bundle := flag.String("bundle", "", "bundle directory")
	timelinePath := flag.String("timeline", "", "optional substitute timeline JSON")
	sourceOverride := flag.String("source", "", "optional substitute source in the form id=path/to/source.wav")
	writeRecreated := flag.Bool("write-recreated", false, "write a new output.wav/receipt.json beside the bundle")
	recreatedDir := flag.String("recreated-dir", "", "directory for re-created artifacts (must not exist)")
	capturedBy := flag.String("captured-by", "third-party", "identity recorded in the verification report")
	outputPath := flag.String("report", "", "write the JSON report here instead of stdout")
	flag.Parse()

	ctx := context.Background()
	switch *action {
	case "fingerprint":
		runFingerprint(*outputPath)
	case "audit", "verify":
		if *bundle == "" {
			fail("-bundle is required for %s", *action)
		}
		if *action == "audit" {
			runAudit(ctx, *bundle, *outputPath)
			return
		}
		runVerify(ctx, *bundle, *timelinePath, *sourceOverride, *writeRecreated, *recreatedDir, *capturedBy, *outputPath)
	default:
		fail("unknown action %q", *action)
	}
}

func runFingerprint(path string) {
	toolchain := repro.CurrentToolchain("voice-timeline-renderer", "", "", false)
	payload, err := repro.MarshalJSONForCLI(toolchain)
	if err != nil {
		fail("encode fingerprint: %v", err)
	}
	report(payload, path)
}

func runAudit(ctx context.Context, dir, path string) {
	findings, err := repro.AuditBundle(ctx, dir)
	if err != nil {
		fail("audit: %v", err)
	}
	payload, err := repro.MarshalJSONForCLI(findings)
	if err != nil {
		fail("encode audit report: %v", err)
	}
	report(payload, path)
	if !findings.Passed {
		os.Exit(1)
	}
}

func runVerify(ctx context.Context, dir, timelinePath, sourceOverride string, writeRecreated bool, recreatedDir, capturedBy, path string) {
	inputs := repro.ReplayInputs{Label: "historical"}
	if timelinePath != "" {
		raw, err := os.ReadFile(timelinePath)
		if err != nil {
			fail("read timeline: %v", err)
		}
		inputs.Timeline = raw
		inputs.Label = "substituted-timeline"
	}
	if sourceOverride != "" {
		id, filePath, err := splitSourceOverride(sourceOverride)
		if err != nil {
			fail("%v", err)
		}
		raw, err := os.ReadFile(filePath)
		if err != nil {
			fail("read source: %v", err)
		}
		inputs.SourceMedia = map[string][]byte{id: raw}
		inputs.Label = "substituted-source-" + id
	}
	reportValue, err := repro.Verify(ctx, dir, repro.VerifyOptions{
		CapturedBy: capturedBy, Inputs: inputs,
		WriteRecreated: writeRecreated, RecreatedDir: recreatedDir,
	})
	if err != nil {
		fail("verify: %v", err)
	}
	payload, err := repro.MarshalJSONForCLI(reportValue)
	if err != nil {
		fail("encode verify report: %v", err)
	}
	report(payload, path)
	if reportValue.Verdict == repro.VerdictSignalDivergent {
		os.Exit(1)
	}
}

func splitSourceOverride(value string) (string, string, error) {
	for index := 0; index < len(value); index++ {
		if value[index] == '=' {
			return value[:index], value[index+1:], nil
		}
	}
	return "", "", fmt.Errorf("source override must be id=path")
}

func report(payload []byte, path string) {
	if len(payload) == 0 {
		fail("empty report")
	}
	if path == "" {
		fmt.Printf("%s", payload)
		return
	}
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		fail("write report: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
