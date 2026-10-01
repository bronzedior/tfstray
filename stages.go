package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func StageA(ctx context.Context, httpClient *http.Client, token, subscriptionID string) ([]Resource, error) {
	var all []Resource
	skipToken := ""

	for {
		query := map[string]interface{}{
			"subscriptions": []string{subscriptionID},
			"query":         "resources | project id, type, name, resourceGroup, location, tags",
		}
		if skipToken != "" {
			query["options"] = map[string]interface{}{
				"$skipToken": skipToken,
			}
		}

		body, err := json.Marshal(query)
		if err != nil {
			return nil, fmt.Errorf("marshal query: %w", err)
		}

		req, _ := http.NewRequestWithContext(ctx, "POST",
			"https://management.azure.com/providers/Microsoft.ResourceGraph/resources?api-version=2021-06-01-preview",
			strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("resource graph request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			data, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("resource graph returned %d: %s", resp.StatusCode, string(data))
		}

		var result struct {
			Data struct {
				Rows [][]interface{} `json:"rows"`
			} `json:"data"`
			SkipToken string `json:"$skipToken"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}

		for _, row := range result.Data.Rows {
			if len(row) < 5 {
				continue
			}

			res := Resource{
				ID:            fmt.Sprintf("%v", row[0]),
				Type:          strings.ToLower(fmt.Sprintf("%v", row[1])),
				Name:          fmt.Sprintf("%v", row[2]),
				ResourceGroup: fmt.Sprintf("%v", row[3]),
				Location:      fmt.Sprintf("%v", row[4]),
			}

			if len(row) > 5 && row[5] != nil {
				if tagMap, ok := row[5].(map[string]interface{}); ok {
					res.Tags = make(map[string]string)
					for k, v := range tagMap {
						res.Tags[k] = fmt.Sprintf("%v", v)
					}
				}
			}

			all = append(all, res)
		}

		if result.SkipToken == "" {
			break
		}
		skipToken = result.SkipToken
	}

	return all, nil
}

type tfState struct {
	Resources []struct {
		Type      string `json:"type"`
		Mode      string `json:"mode"`
		Instances []struct {
			Attributes struct {
				ID string `json:"id"`
			} `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

func StageB(stateInput string) (map[string]bool, error) {
	managed := make(map[string]bool)

	if stateInput == "-" {
		var state tfState
		if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
			return nil, fmt.Errorf("stdin state parse failed: %w", err)
		}
		collectManagedIDs(state, managed)
		return managed, nil
	}

	data, err := os.ReadFile(stateInput)
	if err != nil {
		return nil, fmt.Errorf("read state file failed: %w", err)
	}

	var state tfState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state failed: %w", err)
	}

	collectManagedIDs(state, managed)
	return managed, nil
}

func collectManagedIDs(state tfState, managed map[string]bool) {
	for _, resource := range state.Resources {
		if resource.Mode != "managed" {
			continue
		}
		for _, instance := range resource.Instances {
			id := normalizeID(instance.Attributes.ID)
			if id != "" {
				managed[id] = true
			}
		}
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
		rgMatch, _ := filepath.Match(p.pattern, res.ResourceGroup)
		typeMatch, _ := filepath.Match(strings.ToLower(p.pattern), res.Type)

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
