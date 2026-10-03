package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const resourceGraphURL = "https://management.azure.com/providers/Microsoft.ResourceGraph/resources?api-version=2021-03-01"

const inventoryQuery = `resources
| union (resourcecontainers | where type =~ 'microsoft.resources/subscriptions/resourcegroups')
| extend resourceGroup = iff(isempty(resourceGroup), name, resourceGroup)
| project id, type, name, resourceGroup, location, tags`

func StageA(ctx context.Context, httpClient *http.Client, endpoint, token, subscriptionID string) ([]Resource, error) {
	var all []Resource
	skipToken := ""

	for {
		options := map[string]any{"resultFormat": "objectArray", "$top": 1000}
		if skipToken != "" {
			options["$skipToken"] = skipToken
		}
		body, err := json.Marshal(map[string]any{
			"subscriptions": []string{subscriptionID},
			"query":         inventoryQuery,
			"options":       options,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal query: %w", err)
		}

		page, next, err := fetchInventoryPage(ctx, httpClient, endpoint, token, body)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)

		if next == "" {
			return all, nil
		}
		skipToken = next
	}
}

func fetchInventoryPage(ctx context.Context, httpClient *http.Client, endpoint, token string, body []byte) ([]Resource, string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build resource graph request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("resource graph request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("resource graph returned %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Data []struct {
			ID            string            `json:"id"`
			Type          string            `json:"type"`
			Name          string            `json:"name"`
			ResourceGroup string            `json:"resourceGroup"`
			Location      string            `json:"location"`
			Tags          map[string]string `json:"tags"`
		} `json:"data"`
		SkipToken       string `json:"$skipToken"`
		ResultTruncated string `json:"resultTruncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", fmt.Errorf("decode resource graph response: %w", err)
	}
	if result.ResultTruncated == "true" {
		return nil, "", fmt.Errorf("resource graph truncated the inventory and offered no page token")
	}

	page := make([]Resource, 0, len(result.Data))
	for _, r := range result.Data {
		page = append(page, Resource{
			ID:            r.ID,
			Type:          strings.ToLower(r.Type),
			Name:          r.Name,
			ResourceGroup: r.ResourceGroup,
			Location:      r.Location,
			Tags:          r.Tags,
		})
	}
	return page, result.SkipToken, nil
}

type tfState struct {
	Version       int             `json:"version"`
	FormatVersion string          `json:"format_version"`
	Resources     []stateResource `json:"resources"`
	Values        *struct {
		RootModule showModule `json:"root_module"`
	} `json:"values"`
	PlannedValues json.RawMessage `json:"planned_values"`
}

type stateResource struct {
	Mode      string `json:"mode"`
	Instances []struct {
		Attributes struct {
			ID string `json:"id"`
		} `json:"attributes"`
	} `json:"instances"`
}

type showModule struct {
	Resources []struct {
		Mode   string `json:"mode"`
		Values struct {
			ID string `json:"id"`
		} `json:"values"`
	} `json:"resources"`
	ChildModules []showModule `json:"child_modules"`
}

func StageB(stateInput string) (map[string]bool, error) {
	var data []byte
	var err error
	if stateInput == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(stateInput)
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	return parseState(data)
}

func parseState(data []byte) (map[string]bool, error) {
	var state tfState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}

	managed := make(map[string]bool)
	switch {
	case state.PlannedValues != nil:
		return nil, fmt.Errorf("this is plan JSON; pipe `terraform show -json` without a plan file to get the current state")
	case state.FormatVersion != "":
		if state.Values != nil {
			collectShowIDs(state.Values.RootModule, managed)
		}
	case state.Version == 4:
		for _, r := range state.Resources {
			if r.Mode != "managed" {
				continue
			}
			for _, inst := range r.Instances {
				addID(managed, inst.Attributes.ID)
			}
		}
	default:
		return nil, fmt.Errorf("unrecognised state format: want a version 4 state file or `terraform show -json` output")
	}
	return managed, nil
}

func collectShowIDs(m showModule, managed map[string]bool) {
	for _, r := range m.Resources {
		if r.Mode == "managed" {
			addID(managed, r.Values.ID)
		}
	}
	for _, child := range m.ChildModules {
		collectShowIDs(child, managed)
	}
}

func addID(managed map[string]bool, id string) {
	if id := normalizeID(id); id != "" {
		managed[id] = true
	}
}

func normalizeID(id string) string {
	id = strings.ToLower(id)
	return strings.TrimSuffix(id, "/")
}

func StageD(inventory []Resource, managed map[string]bool) []Resource {
	var unmanaged []Resource
	for _, res := range inventory {
		normID := normalizeID(res.ID)
		if !managed[normID] {
			unmanaged = append(unmanaged, res)
		}
	}
	return unmanaged
}

var defaultDenyList = []struct {
	pattern string
	field   string
}{
	{"MC_*", "rg"},
	{"NetworkWatcherRG", "rg"},
	{"AzureBackupRG_*", "rg"},
	{"databricks-rg-*", "rg"},
	{"microsoft.compute/virtualmachinescalesets/*", "type"},
	{"microsoft.network/networkwatchers", "type"},
}

func StageS(resources []Resource, ignorePatterns []string, noDefaults bool) ([]Resource, []Resource) {
	patterns := []struct {
		pattern string
		field   string
	}{}

	if !noDefaults {
		for _, d := range defaultDenyList {
			patterns = append(patterns, struct {
				pattern string
				field   string
			}{d.pattern, d.field})
		}
	}

	for _, p := range ignorePatterns {
		patterns = append(patterns, struct {
			pattern string
			field   string
		}{p, ""})
	}

	var kept, suppressed []Resource
	for _, res := range resources {
		if shouldSuppress(res, patterns) {
			suppressed = append(suppressed, res)
		} else {
			kept = append(kept, res)
		}
	}
	return kept, suppressed
}

func shouldSuppress(res Resource, patterns []struct {
	pattern string
	field   string
}) bool {
	for _, p := range patterns {
		pattern := strings.ToLower(p.pattern)
		rgMatch, _ := filepath.Match(pattern, strings.ToLower(res.ResourceGroup))
		typeMatch, _ := filepath.Match(pattern, strings.ToLower(res.Type))

		if p.field == "rg" && rgMatch {
			return true
		}
		if p.field == "type" && typeMatch {
			return true
		}
		if p.field == "" && (rgMatch || typeMatch) {
			return true
		}
	}
	return false
}

func StageC(ctx context.Context, httpClient *http.Client, token, subscriptionID string, resources []Resource) []UnmanagedResource {
	var result []UnmanagedResource
	for _, res := range resources {
		um := UnmanagedResource{
			Resource:   res,
			Bucket:     BucketUnknown,
			CallerType: "",
			Caller:     "",
		}
		result = append(result, um)
	}
	return result
}

func StageT(resources []UnmanagedResource) []UnmanagedResource {
	for i := range resources {
		if resources[i].Caller == "" {
			resources[i].Bucket = BucketUnknown
		} else if isEmail(resources[i].Caller) {
			resources[i].CallerType = CallerTypePerson
			resources[i].Bucket = BucketReview
		} else {
			resources[i].CallerType = CallerTypeSP
			resources[i].Bucket = BucketLikelyOtherIaC
		}
	}
	return resources
}

func isEmail(s string) bool {
	return strings.Contains(s, "@")
}
