package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const schemaVersion = 1

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "scan":
		os.Exit(cmdScan(args))
	case "version":
		fmt.Println("tfstray version 0.1.0")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `tfstray - find unmanaged Azure resources

Usage:
  tfstray <command> [flags]

Commands:
  scan      List unmanaged resources
  version   Print version

Examples:
  tfstray scan --subscription 1234-abcd --state prod.tfstate
  terraform show -json | tfstray scan --subscription 1234-abcd --state -
`)
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `tfstray scan - list unmanaged resources

Flags:
  -subscription string   Subscription ID (required)
  -state string          State file, directory of *.tfstate, or - for stdin (required, repeatable)
  -format string         Output format: text or json (default "text")
  -ignore string         Suppress by resource group, type, or full ID glob (repeatable; also read from .tfstrayignore)
  -no-default-ignores    Disable built-in suppression patterns
  -show-suppressed       List suppressed rows
  -max-attribute int     Attribute at most N unmanaged resources, newest first (default 500)
  -fail-on string        Exit 1 when crossed: none, any, person, person>N (default "none")
`)
	}

	var (
		subscription   string
		format         string
		noDefaults     bool
		showSuppressed bool
		failOn         string
		maxAttribute   int
	)
	var stateFiles, ignorePatterns []string

	fs.StringVar(&subscription, "subscription", "", "Subscription ID")
	fs.StringVar(&format, "format", "text", "Output format")
	fs.BoolVar(&noDefaults, "no-default-ignores", false, "Disable built-in suppression")
	fs.BoolVar(&showSuppressed, "show-suppressed", false, "Show suppressed resources")
	fs.StringVar(&failOn, "fail-on", "none", "Threshold for exit code 1")
	fs.IntVar(&maxAttribute, "max-attribute", 500, "Attribute at most N unmanaged resources")

	fs.Func("state", "State file or - for stdin (repeatable)", func(s string) error {
		stateFiles = append(stateFiles, s)
		return nil
	})

	fs.Func("ignore", "Suppression pattern (repeatable)", func(s string) error {
		ignorePatterns = append(ignorePatterns, s)
		return nil
	})

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if subscription == "" {
		fmt.Fprintf(os.Stderr, "error: -subscription is required\n")
		return 2
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(os.Stderr, "error: -format must be text or json, got %q\n", format)
		return 2
	}
	failCrossed, err := parseFailOn(failOn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	rules, err := suppressionRules(ignorePatterns, ".tfstrayignore", noDefaults)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if maxAttribute < 0 {
		fmt.Fprintf(os.Stderr, "error: -max-attribute must be 0 or more\n")
		return 2
	}
	if len(stateFiles) == 0 {
		fmt.Fprintf(os.Stderr, "error: at least one -state is required\n")
		return 2
	}

	stateFiles, err = expandStateInputs(stateFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	managed := make(map[string]bool)
	for _, stateFile := range stateFiles {
		m, err := StageB(stateFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stage B failed on %s: %v\n", stateFile, err)
			return 2
		}
		for id := range m {
			managed[id] = true
		}
	}
	fmt.Fprintf(os.Stderr, "Loaded %d managed resources from state\n", len(managed))

	ctx := context.Background()

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		return 2
	}

	client := &armClient{
		http: &http.Client{},
		base: "https://management.azure.com",
		token: func(ctx context.Context) (string, error) {
			t, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
			return t.Token, err
		},
	}

	fmt.Fprintf(os.Stderr, "Scanning subscription %s...\n", subscription)
	inventory, err := StageA(ctx, client, subscription)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage A failed: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "Found %d resources in subscription\n", len(inventory))

	unmanaged := StageD(inventory, managed)
	fmt.Fprintf(os.Stderr, "Found %d unmanaged resources\n", len(unmanaged))

	kept, suppressed := StageS(unmanaged, rules)
	fmt.Fprintf(os.Stderr, "Suppressed %d resources\n", len(suppressed))

	fmt.Fprintf(os.Stderr, "Attributing %d resources...\n", min(len(kept), maxAttribute))
	attributed, err := StageC(ctx, client, subscription, kept, maxAttribute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage C failed: %v\n", err)
		return 2
	}

	triaged := StageT(attributed)

	summary := summarize(len(inventory), len(unmanaged), suppressed)

	output := Output{
		SchemaVersion: schemaVersion,
		Subscription:  subscription,
		Summary:       summary,
		Resources:     triaged,
	}
	if showSuppressed {
		output.Suppressed = suppressed
	}

	if format == "json" {
		data, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(data))
	} else {
		printText(output)
	}

	if failCrossed(triaged) {
		return 1
	}

	return 0
}

func parseFailOn(rule string) (func([]UnmanagedResource) bool, error) {
	personOnly, max := true, 0
	switch rule {
	case "none":
		return func([]UnmanagedResource) bool { return false }, nil
	case "any":
		personOnly = false
	case "person":
	default:
		n, ok := strings.CutPrefix(rule, "person>")
		v, err := strconv.Atoi(n)
		if !ok || err != nil || v < 0 {
			return nil, fmt.Errorf("-fail-on must be none, any, person, or person>N, got %q", rule)
		}
		max = v
	}
	return func(rows []UnmanagedResource) bool {
		count := 0
		for _, r := range rows {
			if !personOnly || r.Bucket == BucketReview {
				count++
			}
		}
		return count > max
	}, nil
}

func summarize(scanned, unmanaged int, suppressed []SuppressedResource) Summary {
	s := Summary{
		Scanned:    scanned,
		InState:    scanned - unmanaged,
		Unmanaged:  unmanaged,
		Suppressed: SuppressedCount{Total: len(suppressed)},
	}
	for _, r := range suppressed {
		if r.builtIn {
			s.Suppressed.BuiltIn++
		} else {
			s.Suppressed.User++
		}
	}
	if scanned > 0 {
		s.Coverage = float64(s.InState) / float64(scanned)
	}
	return s
}

func printText(output Output) {
	fmt.Printf("Subscription: %s\n", output.Subscription)
	fmt.Printf("Coverage: %.1f%% (%d in state, %d total)\n",
		output.Summary.Coverage*100, output.Summary.InState, output.Summary.Scanned)
	sup := output.Summary.Suppressed
	fmt.Printf("Unmanaged: %d (suppressed: %d (built-in: %d, user rules: %d))\n\n",
		output.Summary.Unmanaged, sup.Total, sup.BuiltIn, sup.User)

	sections := []struct{ bucket, title string }{
		{BucketReview, "REVIEW (created by a person)"},
		{BucketLikelyOtherIaC, "LIKELY_OTHER_IAC (created by a service principal or the platform)"},
		{BucketUnknown, "UNKNOWN (no creation event in the last 90 days)"},
		{BucketNotAttempted, "NOT_ATTEMPTED (beyond -max-attribute)"},
	}
	for _, sec := range sections {
		fmt.Printf("%s:\n", sec.title)
		count := 0
		for _, res := range output.Resources {
			if res.Bucket != sec.bucket {
				continue
			}
			fmt.Printf("  %s (%s)\n", res.Name, res.Type)
			if res.Caller != "" {
				fmt.Printf("    creator: %s\n", res.Caller)
			}
			count++
		}
		if count == 0 {
			fmt.Println("  (none)")
		}
		fmt.Println()
	}

	if len(output.Suppressed) > 0 {
		fmt.Printf("Suppressed (%d):\n", len(output.Suppressed))
		for _, res := range output.Suppressed {
			fmt.Printf("  %s (%s)\n    rule: %s\n", res.Name, res.Type, res.Rule)
		}
	}
}
