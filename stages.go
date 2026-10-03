package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const inventoryQuery = `resources
| union (resourcecontainers | where type =~ 'microsoft.resources/subscriptions/resourcegroups')
| extend resourceGroup = iff(isempty(resourceGroup), name, resourceGroup)
| project id, type, name, resourceGroup, location, tags`

type armClient struct {
	http  *http.Client
	base  string
	token func(context.Context) (string, error)
}

const maxAttempts = 6

func (c *armClient) do(ctx context.Context, method, target string, body, out any) error {
	if strings.HasPrefix(target, "/") {
		target = c.base + target
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	for attempt := 1; ; attempt++ {
		token, err := c.token(ctx)
		if err != nil {
			return fmt.Errorf("get token: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < maxAttempts {
			wait := time.Second << (attempt - 1)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(s) * time.Second
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned %d after %d attempts: %s", method, resp.StatusCode, attempt, data)
		}
		return json.Unmarshal(data, out)
	}
}

func StageA(ctx context.Context, c *armClient, subscriptionID string) ([]Resource, error) {
	var all []Resource
	skipToken := ""

	for {
		options := map[string]any{"resultFormat": "objectArray", "$top": 1000}
		if skipToken != "" {
			options["$skipToken"] = skipToken
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
		err := c.do(ctx, "POST", "/providers/Microsoft.ResourceGraph/resources?api-version=2021-03-01", map[string]any{
			"subscriptions": []string{subscriptionID},
			"query":         inventoryQuery,
			"options":       options,
		}, &result)
		if err != nil {
			return nil, fmt.Errorf("resource graph: %w", err)
		}
		if result.ResultTruncated == "true" {
			return nil, fmt.Errorf("resource graph truncated the inventory and offered no page token")
		}

		for _, r := range result.Data {
			all = append(all, Resource{
				ID:            r.ID,
				Type:          strings.ToLower(r.Type),
				Name:          r.Name,
				ResourceGroup: r.ResourceGroup,
				Location:      r.Location,
				Tags:          r.Tags,
			})
		}

		if result.SkipToken == "" {
			return all, nil
		}
		skipToken = result.SkipToken
	}
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

func expandStateInputs(inputs []string) ([]string, error) {
	var files []string
	for _, in := range inputs {
		if in == "-" {
			if slices.Contains(files, "-") {
				return nil, fmt.Errorf("- may appear at most once; stdin holds one document")
			}
			files = append(files, in)
			continue
		}
		info, err := os.Stat(in)
		if err != nil || !info.IsDir() {
			files = append(files, in)
			continue
		}
		entries, err := os.ReadDir(in)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tfstate") {
				files = append(files, filepath.Join(in, e.Name()))
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: no *.tfstate files in directory", in)
		}
	}
	return files, nil
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

// suppressRule matches one field ("rg" or "type") for a built-in rule. A user
// rule has no field and matches resource group, type, or full ID.
type suppressRule struct {
	pattern string
	field   string
}

var defaultDenyList = []suppressRule{
	{"MC_*", "rg"},
	{"NetworkWatcherRG", "rg"},
	{"AzureBackupRG_*", "rg"},
	{"databricks-rg-*", "rg"},
	{"microsoft.network/networkwatchers", "type"},
}

// suppressionRules returns the built-in rules (unless disabled), then --ignore
// patterns, then .tfstrayignore lines. A missing ignore file is not an error;
// a malformed pattern is.
func suppressionRules(ignore []string, ignoreFile string, noDefaults bool) ([]suppressRule, error) {
	var rules []suppressRule
	if !noDefaults {
		rules = append(rules, defaultDenyList...)
	}

	add := func(p, source string) error {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%s: bad ignore pattern %q: %w", source, p, err)
		}
		rules = append(rules, suppressRule{pattern: p})
		return nil
	}

	for _, p := range ignore {
		if err := add(p, "--ignore"); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(ignoreFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			if err := add(line, fmt.Sprintf("%s:%d", ignoreFile, i+1)); err != nil {
				return nil, err
			}
		}
	}
	return rules, nil
}

func StageS(resources []Resource, rules []suppressRule) ([]Resource, []SuppressedResource) {
	var kept []Resource
	var suppressed []SuppressedResource
	for _, res := range resources {
		if r, ok := matchRule(res, rules); ok {
			suppressed = append(suppressed, SuppressedResource{Resource: res, Rule: r.pattern, builtIn: r.field != ""})
		} else {
			kept = append(kept, res)
		}
	}
	return kept, suppressed
}

// matchRule returns the first matching rule. Matching is case-insensitive, and
// path.Match keeps * from crossing /.
func matchRule(res Resource, rules []suppressRule) (suppressRule, bool) {
	for _, r := range rules {
		pattern := strings.ToLower(r.pattern)
		match := func(v string) bool {
			ok, _ := path.Match(pattern, strings.ToLower(v))
			return ok
		}
		switch r.field {
		case "rg":
			if match(res.ResourceGroup) {
				return r, true
			}
		case "type":
			if match(res.Type) {
				return r, true
			}
		default:
			if match(res.ResourceGroup) || match(res.Type) || match(res.ID) {
				return r, true
			}
		}
	}
	return suppressRule{}, false
}

const attributionWorkers = 8

const attributionWindow = 90*24*time.Hour - time.Hour

func StageC(ctx context.Context, c *armClient, subscriptionID string, resources []Resource, maxAttribute int) ([]UnmanagedResource, error) {
	result := make([]UnmanagedResource, len(resources))
	if len(resources) == 0 {
		return result, nil
	}

	created, err := fetchCreatedTimes(ctx, c, subscriptionID)
	if err != nil {
		return nil, err
	}
	resources = slices.Clone(resources)
	slices.SortStableFunc(resources, func(a, b Resource) int {
		return created[normalizeID(b.ID)].Compare(created[normalizeID(a.ID)])
	})

	now := time.Now()
	since := now.Add(-attributionWindow)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, attributionWorkers)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i, res := range resources {
		result[i] = UnmanagedResource{Resource: res}
		createdAt := created[normalizeID(res.ID)]
		switch {
		case i >= maxAttribute:
			result[i].Bucket = BucketNotAttempted
		case !createdAt.IsZero() && createdAt.Before(since):
			result[i].Bucket = BucketUnknown
		default:
			wg.Add(1)
			go func(u *UnmanagedResource) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := findCreator(ctx, c, subscriptionID, u, since, now); err != nil {
					once.Do(func() { firstErr = err; cancel() })
				}
			}(&result[i])
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

func fetchCreatedTimes(ctx context.Context, c *armClient, subscriptionID string) (map[string]time.Time, error) {
	created := make(map[string]time.Time)
	next := "/subscriptions/" + subscriptionID + "/resources?$expand=createdTime&api-version=2021-04-01"
	for next != "" {
		var page struct {
			Value []struct {
				ID          string    `json:"id"`
				CreatedTime time.Time `json:"createdTime"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := c.do(ctx, "GET", next, nil, &page); err != nil {
			return nil, fmt.Errorf("creation times: %w", err)
		}
		for _, r := range page.Value {
			if !r.CreatedTime.IsZero() {
				created[normalizeID(r.ID)] = r.CreatedTime
			}
		}
		next = page.NextLink
	}
	return created, nil
}

func findCreator(ctx context.Context, c *armClient, subscriptionID string, u *UnmanagedResource, since, now time.Time) error {
	q := url.Values{}
	q.Set("api-version", "2015-04-01")
	q.Set("$filter", fmt.Sprintf("eventTimestamp ge '%s' and eventTimestamp le '%s' and resourceUri eq '%s'",
		since.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), u.ID))
	q.Set("$select", "caller,eventTimestamp,operationName,status")
	next := "/subscriptions/" + subscriptionID + "/providers/Microsoft.Insights/eventtypes/management/values?" + q.Encode()
	createOp := u.Type + "/write"

	for next != "" {
		var page struct {
			Value []struct {
				Caller         string    `json:"caller"`
				EventTimestamp time.Time `json:"eventTimestamp"`
				OperationName  struct {
					Value string `json:"value"`
				} `json:"operationName"`
				Status struct {
					Value string `json:"value"`
				} `json:"status"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := c.do(ctx, "GET", next, nil, &page); err != nil {
			return fmt.Errorf("activity log for %s: %w", u.ID, err)
		}
		for _, e := range page.Value {
			if e.Status.Value != "Succeeded" || !strings.EqualFold(e.OperationName.Value, createOp) {
				continue
			}
			if u.EventTime == nil || e.EventTimestamp.Before(*u.EventTime) {
				t := e.EventTimestamp
				u.Caller, u.EventTime = e.Caller, &t
			}
		}
		next = page.NextLink
	}
	return nil
}

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

func StageT(resources []UnmanagedResource) []UnmanagedResource {
	for i := range resources {
		r := &resources[i]
		switch {
		case r.Bucket != "":
		case r.Caller == "":
			r.Bucket = BucketUnknown
		case strings.Contains(r.Caller, "@"):
			r.CallerType, r.Bucket = CallerTypePerson, BucketReview
		case guidPattern.MatchString(r.Caller):
			r.CallerType, r.Bucket = CallerTypeSP, BucketLikelyOtherIaC
		default:
			r.Bucket = BucketLikelyOtherIaC
		}
	}
	return resources
}
