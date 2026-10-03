package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

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
  -subscription string    Subscription ID (required)
  -state string          State file or - for stdin (required, repeatable)
  -format string         Output format: text or json (default "text")
  -ignore string         Suppress by resource group/type glob (repeatable)
  -no-default-ignores    Disable built-in suppression patterns
  -show-suppressed       List suppressed rows
  -fail-on string        Exit with 1 if threshold crossed: none|person|any (default "none")
`)
	}

	var (
		subscription   string
		format         string
		noDefaults     bool
		showSuppressed bool
		failOn         string
	)
	var stateFiles, ignorePatterns []string

	fs.StringVar(&subscription, "subscription", "", "Subscription ID")
	fs.StringVar(&format, "format", "text", "Output format")
	fs.BoolVar(&noDefaults, "no-default-ignores", false, "Disable built-in suppression")
	fs.BoolVar(&showSuppressed, "show-suppressed", false, "Show suppressed resources")
	fs.StringVar(&failOn, "fail-on", "none", "Threshold for exit code 1")

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
	if len(stateFiles) == 0 {
		fmt.Fprintf(os.Stderr, "error: at least one -state is required\n")
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

	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "get token failed: %v\n", err)
		return 2
	}

	httpClient := &http.Client{}

	fmt.Fprintf(os.Stderr, "Scanning subscription %s...\n", subscription)
	inventory, err := StageA(ctx, httpClient, resourceGraphURL, token.Token, subscription)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage A failed: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "Found %d resources in subscription\n", len(inventory))

	unmanaged := StageD(inventory, managed)
	fmt.Fprintf(os.Stderr, "Found %d unmanaged resources\n", len(unmanaged))

	kept, suppressed := StageS(unmanaged, ignorePatterns, noDefaults)
	fmt.Fprintf(os.Stderr, "Suppressed %d resources\n", len(suppressed))

	attributed := StageC(ctx, httpClient, token.Token, subscription, kept)

	triaged := StageT(attributed)

	summary := summarize(len(inventory), len(unmanaged), len(suppressed))

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

	if failOn != "none" {
		for _, res := range triaged {
			if failOn == "any" || (failOn == "person" && res.CallerType == CallerTypePerson) {
				return 1
			}
		}
	}

	return 0
}

func summarize(scanned, unmanaged, suppressed int) Summary {
	s := Summary{
		Scanned:    scanned,
		InState:    scanned - unmanaged,
		Unmanaged:  unmanaged,
		Suppressed: suppressed,
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
	fmt.Printf("Unmanaged: %d (suppressed: %d)\n\n", output.Summary.Unmanaged, output.Summary.Suppressed)

	fmt.Println("REVIEW (created by person):")
	count := 0
	for _, res := range output.Resources {
		if res.Bucket == BucketReview {
			fmt.Printf("  %s (%s)\n    creator: %s\n", res.Name, res.Type, res.Caller)
			count++
		}
	}
	if count == 0 {
		fmt.Println("  (none)")
	}

	fmt.Println("\nLIKELY_OTHER_IaC (created by service principal):")
	count = 0
	for _, res := range output.Resources {
		if res.Bucket == BucketLikelyOtherIaC {
			fmt.Printf("  %s (%s)\n    creator: %s\n", res.Name, res.Type, res.Caller)
			count++
		}
	}
	if count == 0 {
		fmt.Println("  (none)")
	}

	fmt.Println("\nUNKNOWN (no event found):")
	count = 0
	for _, res := range output.Resources {
		if res.Bucket == BucketUnknown {
			fmt.Printf("  %s (%s)\n", res.Name, res.Type)
			count++
		}
	}
	if count == 0 {
		fmt.Println("  (none)")
	}

	if len(output.Suppressed) > 0 {
		fmt.Printf("\nSuppressed (%d):\n", len(output.Suppressed))
		for _, res := range output.Suppressed {
			fmt.Printf("  %s\n", res.Name)
		}
	}
}
