package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"/SUBSCRIPTIONS/sub/RESOURCEGROUPS/rg/PROVIDERS/MICROSOFT.COMPUTE/DISKS/disk", "/subscriptions/sub/resourcegroups/rg/providers/microsoft.compute/disks/disk"},
		{"/subscriptions/sub/resourcegroups/rg/providers/microsoft.compute/disks/disk/", "/subscriptions/sub/resourcegroups/rg/providers/microsoft.compute/disks/disk"},
		{"", ""},
	}

	for _, tt := range tests {
		if got := normalizeID(tt.input); got != tt.want {
			t.Errorf("normalizeID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStageD(t *testing.T) {
	inventory := []Resource{
		{ID: "/subs/1/rg/a/providers/microsoft.compute/disks/disk1"},
		{ID: "/subs/1/rg/a/providers/microsoft.compute/disks/disk2"},
		{ID: "/subs/1/rg/a/providers/microsoft.storage/accounts/sa1"},
	}

	managed := map[string]bool{
		"/subs/1/rg/a/providers/microsoft.compute/disks/disk1": true,
	}

	unmanaged := StageD(inventory, managed)
	if len(unmanaged) != 2 {
		t.Errorf("StageD: got %d unmanaged, want 2", len(unmanaged))
	}

	for _, res := range unmanaged {
		if res.ID == "/subs/1/rg/a/providers/microsoft.compute/disks/disk1" {
			t.Errorf("StageD: disk1 should be managed, not unmanaged")
		}
	}
}

func TestSuppression(t *testing.T) {
	resources := []Resource{
		{Name: "disk1", ResourceGroup: "MC_myaks_eastus", Type: "microsoft.compute/disks"},
		{Name: "disk2", ResourceGroup: "rg-app", Type: "microsoft.compute/disks"},
		{Name: "watcher", ResourceGroup: "NetworkWatcherRG", Type: "microsoft.network/networkwatchers"},
		{Name: "sa", ResourceGroup: "rg-sandbox-prod", Type: "microsoft.storage/storageaccounts"},
	}

	kept, suppressed := StageS(resources, mustRules(t, "rg-sandbox-*"))

	if len(suppressed) != 3 {
		t.Errorf("StageS: got %d suppressed, want 3", len(suppressed))
	}
	if len(kept) != 1 {
		t.Errorf("StageS: got %d kept, want 1", len(kept))
	}

	if kept[0].Name != "disk2" {
		t.Errorf("StageS: expected disk2 to be kept, got %s", kept[0].Name)
	}
}

func TestTriage(t *testing.T) {
	resources := []UnmanagedResource{
		{Resource: Resource{Name: "res1"}, Caller: "user@example.com"},
		{Resource: Resource{Name: "res2"}, Caller: "00000000-0000-0000-0000-000000000000"},
		{Resource: Resource{Name: "res3"}, Caller: ""},
		{Resource: Resource{Name: "res4"}, Caller: "Microsoft.Resources"},
		{Resource: Resource{Name: "res5"}, Bucket: BucketNotAttempted},
	}

	triaged := StageT(resources)

	if triaged[0].CallerType != CallerTypePerson || triaged[0].Bucket != BucketReview {
		t.Errorf("StageT: email caller should be REVIEW")
	}
	if triaged[1].CallerType != CallerTypeSP || triaged[1].Bucket != BucketLikelyOtherIaC {
		t.Errorf("StageT: GUID caller should be LIKELY_OTHER_IaC")
	}
	if triaged[2].Bucket != BucketUnknown {
		t.Errorf("StageT: empty caller should be UNKNOWN")
	}
	if triaged[3].CallerType != "" || triaged[3].Bucket != BucketLikelyOtherIaC {
		t.Errorf("StageT: non-email, non-GUID caller should be LIKELY_OTHER_IAC with no caller_type, got %+v", triaged[3])
	}
	if triaged[4].Bucket != BucketNotAttempted {
		t.Errorf("StageT: must keep NOT_ATTEMPTED decided in Stage C, got %s", triaged[4].Bucket)
	}
}

func fakeResourceGraph(t *testing.T, total int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query   string `json:"query"`
			Options struct {
				Top          int    `json:"$top"`
				SkipToken    string `json:"$skipToken"`
				ResultFormat string `json:"resultFormat"`
			} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
			return
		}
		if req.Options.ResultFormat != "objectArray" || req.Options.Top != 1000 {
			t.Errorf("options = %+v, want objectArray and $top 1000", req.Options)
		}
		if !strings.Contains(req.Query, "resourcecontainers") {
			t.Errorf("query does not include resource groups: %s", req.Query)
		}
		start, _ := strconv.Atoi(req.Options.SkipToken)
		end := min(start+1000, total)
		var data []map[string]any
		for i := start; i < end; i++ {
			data = append(data, map[string]any{
				"id":   fmt.Sprintf("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d%d", i),
				"type": "Microsoft.Compute/disks", "name": fmt.Sprintf("d%d", i),
				"resourceGroup": "rg", "location": "eastus", "tags": map[string]string{"env": "dev"},
			})
		}
		resp := map[string]any{"data": data}
		if end < total {
			resp["$skipToken"] = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestStageAPagination(t *testing.T) {
	srv := fakeResourceGraph(t, 1500)
	defer srv.Close()

	got, err := StageA(context.Background(), testClient(srv), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1500 {
		t.Fatalf("got %d resources, want 1500", len(got))
	}
	if got[0].Type != "microsoft.compute/disks" || got[0].Tags["env"] != "dev" || got[1499].Name != "d1499" {
		t.Errorf("bad row decode: %+v / %+v", got[0], got[1499])
	}
}

func TestParseStateRaw(t *testing.T) {
	data, err := os.ReadFile("testdata/example-state.json")
	if err != nil {
		t.Fatal(err)
	}
	managed, err := parseState(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 2 {
		t.Errorf("got %d managed IDs, want 2 (data block excluded): %v", len(managed), managed)
	}
	if !managed["/subscriptions/12345678-1234-1234-1234-123456789012/resourcegroups/rg-managed"] {
		t.Errorf("mis-cased RG ID not normalized: %v", managed)
	}
}

func TestParseStateShowJSON(t *testing.T) {
	data := []byte(`{
	  "format_version": "1.0",
	  "terraform_version": "1.9.0",
	  "values": {"root_module": {
	    "resources": [
	      {"mode": "managed", "values": {"id": "/subscriptions/s/resourceGroups/RG-A"}},
	      {"mode": "data", "values": {"id": "/subscriptions/s/resourceGroups/rg-read-only"}}
	    ],
	    "child_modules": [{"resources": [
	      {"mode": "managed", "values": {"id": "/subscriptions/s/resourceGroups/rg-a/providers/Microsoft.Storage/storageAccounts/sa1/"}}
	    ], "child_modules": [{"resources": [
	      {"mode": "managed", "values": {"id": "/subscriptions/s/resourceGroups/rg-b"}}
	    ]}]}]
	  }}
	}`)
	managed, err := parseState(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/subscriptions/s/resourcegroups/rg-a",
		"/subscriptions/s/resourcegroups/rg-a/providers/microsoft.storage/storageaccounts/sa1",
		"/subscriptions/s/resourcegroups/rg-b",
	}
	if len(managed) != len(want) {
		t.Errorf("got %v, want %v", managed, want)
	}
	for _, id := range want {
		if !managed[id] {
			t.Errorf("missing %s in %v", id, managed)
		}
	}
}

func TestParseStateRejectsUnknownShapes(t *testing.T) {
	for _, in := range []string{`{"foo": 1}`, `{"version": 3, "resources": []}`, `[]`, `not json`} {
		if _, err := parseState([]byte(in)); err == nil {
			t.Errorf("parseState(%s) = nil error, want error", in)
		}
	}
}

func TestSummarize(t *testing.T) {
	suppressed := []SuppressedResource{{builtIn: true}, {builtIn: true}, {}}
	s := summarize(312, 56, suppressed)
	if s.InState != 256 || s.Coverage < 0.8205 || s.Coverage > 0.8206 {
		t.Errorf("summarize(312, 56, ...) = %+v", s)
	}
	if s.Suppressed != (SuppressedCount{Total: 3, BuiltIn: 2, User: 1}) {
		t.Errorf("suppressed split = %+v, want 3 total, 2 built-in, 1 user", s.Suppressed)
	}
	if s := summarize(0, 0, nil); s.Coverage != 0 {
		t.Errorf("empty subscription coverage = %v, want 0", s.Coverage)
	}
}

func TestStageARejectsTruncatedResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"id": "/subscriptions/s/resourceGroups/rg"}], "resultTruncated": "true"}`))
	}))
	defer srv.Close()

	if _, err := StageA(context.Background(), testClient(srv), "s"); err == nil {
		t.Error("truncated inventory accepted; resources beyond the cut would silently go unscanned")
	}
}

func TestParseStateRejectsPlanJSON(t *testing.T) {
	plan := `{"format_version": "1.2", "planned_values": {"root_module": {"resources": [
	  {"mode": "managed", "values": {"id": "/subscriptions/s/resourceGroups/rg-a"}}]}}}`
	if _, err := parseState([]byte(plan)); err == nil {
		t.Error("plan JSON accepted as state")
	}
	if m, err := parseState([]byte(`{"format_version": "1.0"}`)); err != nil || len(m) != 0 {
		t.Errorf("empty show -json state: got %v, %v; want empty, nil", m, err)
	}
}

func TestSuppressionIgnoresCase(t *testing.T) {
	resources := []Resource{
		{Name: "NetworkWatcherRG", ResourceGroup: "networkwatcherrg", Type: "microsoft.resources/subscriptions/resourcegroups"},
		{Name: "aks-nodepool-disk", ResourceGroup: "mc_myaks_eastus", Type: "microsoft.compute/disks"},
		{Name: "sa", ResourceGroup: "rg-sandbox-prod", Type: "microsoft.storage/storageaccounts"},
		{Name: "app", ResourceGroup: "rg-app", Type: "microsoft.web/sites"},
	}

	kept, suppressed := StageS(resources, mustRules(t, "RG-Sandbox-*"))

	if len(suppressed) != 3 || len(kept) != 1 || kept[0].Name != "app" {
		t.Errorf("kept %v, suppressed %v; want only app kept", kept, suppressed)
	}
}

func testClient(srv *httptest.Server) *armClient {
	return &armClient{http: srv.Client(), base: srv.URL, token: func(context.Context) (string, error) { return "t", nil }}
}

func daysAgo(d int) string {
	return time.Now().UTC().Add(-time.Duration(d) * 24 * time.Hour).Format(time.RFC3339)
}

const diskPrefix = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/"

func event(caller, op, status string, days int) map[string]any {
	return map[string]any{
		"caller": caller, "eventTimestamp": daysAgo(days),
		"operationName": map[string]string{"value": op},
		"status":        map[string]string{"value": status},
	}
}

func fakeARM(t *testing.T) (*httptest.Server, func(name string) int) {
	t.Helper()
	const diskWrite = "Microsoft.Compute/disks/write"
	events := map[string][]map[string]any{
		"by-person": {
			event("bob@example.com", "Microsoft.Resources/tags/write", "Succeeded", 9),
			event("00000000-0000-0000-0000-00000000aaaa", "Microsoft.Resources/deployments/write", "Succeeded", 9),
			event("carol@example.com", diskWrite, "Started", 9),
			event("alice@example.com", diskWrite, "Succeeded", 8),
			event("dave@example.com", diskWrite, "Succeeded", 1),
		},
		"by-sp":       {event("11111111-2222-3333-4444-555555555555", diskWrite, "Succeeded", 5)},
		"rg-handmade": {event("you@example.com", "Microsoft.Resources/subscriptions/resourceGroups/write", "Succeeded", 2)},
	}
	var mu sync.Mutex
	queried := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page2 := r.URL.Query().Get("page") == "2"
		next := "http://" + r.Host + r.URL.RequestURI() + "&page=2"
		switch {
		case strings.HasSuffix(r.URL.Path, "/subscriptions/s/resources"):
			if r.URL.Query().Get("$expand") != "createdTime" && !page2 {
				t.Errorf("creation time request without $expand=createdTime: %s", r.URL)
			}
			if !page2 {
				json.NewEncoder(w).Encode(map[string]any{"nextLink": next, "value": []map[string]any{
					{"id": strings.ToUpper(diskPrefix + "by-person"), "createdTime": daysAgo(10)},
				}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
				{"id": diskPrefix + "by-sp", "createdTime": daysAgo(5)},
				{"id": diskPrefix + "no-events", "createdTime": daysAgo(3)},
				{"id": diskPrefix + "old", "createdTime": daysAgo(200)},
			}})
		case strings.HasSuffix(r.URL.Path, "/providers/Microsoft.Insights/eventtypes/management/values"):
			filter := r.URL.Query().Get("$filter")
			_, id, ok := strings.Cut(filter, "resourceUri eq '")
			if !ok || !strings.Contains(filter, "eventTimestamp ge '") {
				t.Errorf("bad activity log filter: %s", filter)
			}
			name := path.Base(strings.TrimSuffix(id, "'"))
			mu.Lock()
			queried[name]++
			n := queried[name]
			mu.Unlock()
			if name == "by-sp" && n == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			evs := events[name]
			if name == "by-person" {
				if !page2 {
					json.NewEncoder(w).Encode(map[string]any{"value": evs[:3], "nextLink": next})
					return
				}
				evs = evs[3:]
			}
			json.NewEncoder(w).Encode(map[string]any{"value": evs})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	return srv, func(name string) int { mu.Lock(); defer mu.Unlock(); return queried[name] }
}

func disk(name string) Resource {
	return Resource{ID: diskPrefix + name, Type: "microsoft.compute/disks", Name: name}
}

func TestStageC(t *testing.T) {
	srv, queried := fakeARM(t)
	defer srv.Close()

	resources := []Resource{
		{ID: "/subscriptions/s/resourceGroups/rg-handmade", Type: "microsoft.resources/subscriptions/resourcegroups", Name: "rg-handmade"},
		disk("by-person"), disk("by-sp"), disk("no-events"), disk("old"),
	}
	got, err := StageC(context.Background(), testClient(srv), "s", resources, 500)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]UnmanagedResource{}
	for _, r := range StageT(got) {
		byName[r.Name] = r
	}

	if r := byName["by-person"]; r.Bucket != BucketReview || r.Caller != "alice@example.com" || r.EventTime == nil {
		t.Errorf("by-person: want REVIEW by alice (tag, deployment and Started events must not win), got %+v", r)
	}
	if r := byName["by-sp"]; r.Bucket != BucketLikelyOtherIaC || r.CallerType != CallerTypeSP {
		t.Errorf("by-sp: got %+v", r)
	}
	if r := byName["rg-handmade"]; r.Bucket != BucketReview || r.Caller != "you@example.com" {
		t.Errorf("rg-handmade (no creation time) should still be attributed, got %+v", r)
	}
	if r := byName["no-events"]; r.Bucket != BucketUnknown || r.Caller != "" {
		t.Errorf("no-events: want UNKNOWN with no caller, got %+v", r)
	}
	if r := byName["old"]; r.Bucket != BucketUnknown || queried("old") != 0 {
		t.Errorf("old: want UNKNOWN without an Activity Log query, got %+v after %d queries", r, queried("old"))
	}
	if queried("by-sp") != 2 {
		t.Errorf("by-sp: want 1 throttled + 1 retried query, got %d", queried("by-sp"))
	}
	var order []string
	for _, r := range got {
		order = append(order, r.Name)
	}
	if want := []string{"no-events", "by-sp", "by-person", "old", "rg-handmade"}; !slices.Equal(order, want) {
		t.Errorf("want newest first and no creation time last: got %v, want %v", order, want)
	}
}

func TestStageCMaxAttribute(t *testing.T) {
	srv, queried := fakeARM(t)
	defer srv.Close()

	got, err := StageC(context.Background(), testClient(srv), "s", []Resource{disk("by-person"), disk("by-sp")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	got = StageT(got)
	if got[0].Name != "by-sp" || got[0].Bucket != BucketLikelyOtherIaC {
		t.Errorf("most recent should be attributed, got %+v", got[0])
	}
	if got[1].Name != "by-person" || got[1].Bucket != BucketNotAttempted || queried("by-person") != 0 {
		t.Errorf("beyond the cap should be NOT_ATTEMPTED and never queried, got %+v", got[1])
	}
}

func TestStageCFailsAfterRetries(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resources") {
			w.Write([]byte(`{"value": []}`))
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := StageC(context.Background(), testClient(srv), "s", []Resource{disk("x")}, 500); err == nil {
		t.Error("an attribution query that keeps failing must fail the run, not become UNKNOWN")
	}
	if calls < 2 {
		t.Errorf("want retries before giving up, got %d calls", calls)
	}
}

func TestParseFailOn(t *testing.T) {
	rows := []UnmanagedResource{{Bucket: BucketReview}, {Bucket: BucketReview}, {Bucket: BucketUnknown}}
	for rule, want := range map[string]bool{
		"none": false, "any": true, "person": true, "person>1": true, "person>2": false,
	} {
		crossed, err := parseFailOn(rule)
		if err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
		if got := crossed(rows); got != want {
			t.Errorf("%s on 2 REVIEW + 1 UNKNOWN = %v, want %v", rule, got, want)
		}
	}
	if crossed, _ := parseFailOn("person"); crossed([]UnmanagedResource{{Bucket: BucketUnknown}}) {
		t.Error("person must count only REVIEW rows")
	}
	if crossed, _ := parseFailOn("any"); crossed(nil) {
		t.Error("any must not fire on an empty result")
	}
	for _, bad := range []string{"", "all", "person>", "person>-1", "person>x", "any>1"} {
		if _, err := parseFailOn(bad); err == nil {
			t.Errorf("parseFailOn(%q) accepted", bad)
		}
	}
}

func TestExpandStateInputs(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.tfstate", "b.tfstate", "a.tfstate.backup", ".terraform/c.tfstate"} {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("{}"), 0o644)
	}

	got, err := expandStateInputs([]string{dir, "-", "prod.tfstate"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.tfstate"), filepath.Join(dir, "b.tfstate"), "-", "prod.tfstate"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (non-recursive, *.tfstate only)", got, want)
	}

	if _, err := expandStateInputs([]string{"-", "-"}); err == nil {
		t.Error("- twice accepted; stdin holds one document")
	}
	if _, err := expandStateInputs([]string{filepath.Join(dir, ".terraform")}); err != nil {
		t.Errorf("a directory with one state file should be accepted: %v", err)
	}
	empty := t.TempDir()
	if _, err := expandStateInputs([]string{empty}); err == nil {
		t.Error("directory with no *.tfstate accepted; that would flag everything as unmanaged")
	}
}

func mustRules(t *testing.T, ignore ...string) []suppressRule {
	t.Helper()
	rules, err := suppressionRules(ignore, filepath.Join(t.TempDir(), ".tfstrayignore"), false)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestSuppressionRules(t *testing.T) {
	ignoreFile := filepath.Join(t.TempDir(), ".tfstrayignore")
	os.WriteFile(ignoreFile, []byte("# team suppressions\n\n  Microsoft.Insights/*  \n/subscriptions/s/resourcegroups/rg-app/providers/microsoft.web/sites/legacy\n"), 0o644)

	rules, err := suppressionRules([]string{"rg-sandbox-*"}, ignoreFile, false)
	if err != nil {
		t.Fatal(err)
	}
	resources := []Resource{
		{Name: "aks", ResourceGroup: "mc_aks_eastus", Type: "microsoft.compute/disks"},
		{Name: "sb", ResourceGroup: "rg-sandbox-1", Type: "microsoft.storage/storageaccounts"},
		{Name: "ai", ResourceGroup: "rg-app", Type: "microsoft.insights/components"},
		{Name: "legacy", ResourceGroup: "rg-app", Type: "microsoft.web/sites",
			ID: "/subscriptions/s/resourceGroups/rg-app/providers/Microsoft.Web/sites/legacy"},
		{Name: "keep", ResourceGroup: "rg-app", Type: "microsoft.web/sites",
			ID: "/subscriptions/s/resourceGroups/rg-app/providers/Microsoft.Web/sites/keep"},
	}
	kept, suppressed := StageS(resources, rules)

	if len(kept) != 1 || kept[0].Name != "keep" {
		t.Errorf("kept %v, want only keep", kept)
	}
	want := map[string]SuppressedResource{
		"aks":    {Rule: "MC_*", builtIn: true},
		"sb":     {Rule: "rg-sandbox-*"},
		"ai":     {Rule: "Microsoft.Insights/*"},
		"legacy": {Rule: "/subscriptions/s/resourcegroups/rg-app/providers/microsoft.web/sites/legacy"},
	}
	for _, r := range suppressed {
		if w := want[r.Name]; r.Rule != w.Rule || r.builtIn != w.builtIn {
			t.Errorf("%s: rule %q builtIn %v, want %q %v", r.Name, r.Rule, r.builtIn, w.Rule, w.builtIn)
		}
	}

	if rules, _ := suppressionRules(nil, ignoreFile, true); len(rules) != 2 {
		t.Errorf("--no-default-ignores must keep only user rules, got %d", len(rules))
	}
	for _, bad := range [][]string{{"rg-["}, {"ok", "[a-"}} {
		if _, err := suppressionRules(bad, "missing", false); err == nil {
			t.Errorf("bad pattern %q accepted", bad)
		}
	}
	os.WriteFile(ignoreFile, []byte("rg-[\n"), 0o644)
	if _, err := suppressionRules(nil, ignoreFile, false); err == nil || !strings.Contains(err.Error(), ignoreFile+":1") {
		t.Errorf("bad pattern in .tfstrayignore: want an error naming the file and line, got %v", err)
	}
}

func TestPrintText(t *testing.T) {
	out := Output{
		Subscription: "s",
		Summary: summarize(312, 56, []SuppressedResource{
			{builtIn: true}, {builtIn: true}, {builtIn: true}, {builtIn: true}, {builtIn: true}, {builtIn: true}, {builtIn: true}, {}, {},
		}),
		Resources: []UnmanagedResource{
			{Resource: Resource{Name: "disk-test-old", Type: "microsoft.compute/disks", ResourceGroup: "rg-app"}, Bucket: BucketReview, Caller: "alice@contoso.com"},
			{Resource: Resource{Name: "rg-legacy-billing", Type: "microsoft.resources/subscriptions/resourcegroups", ResourceGroup: "rg-legacy-billing"}, Bucket: BucketReview, Caller: "bob@contoso.com"},
			{Resource: Resource{Name: "st0accountlogs", Type: "microsoft.storage/storageaccounts", ResourceGroup: "rg-logs"}, Bucket: BucketLikelyOtherIaC, Caller: "11111111-2222-3333-4444-555555555555"},
			{Resource: Resource{Name: "vnet-old", Type: "microsoft.network/virtualnetworks", ResourceGroup: "rg-net"}, Bucket: BucketUnknown},
		},
		Suppressed: []SuppressedResource{
			{Resource: Resource{Name: "aks-pool-disk", Type: "microsoft.compute/disks", ResourceGroup: "mc_aks_eastus"}, Rule: "MC_*"},
		},
	}
	var b strings.Builder
	printText(&b, out)

	want := `Scanned 312 | in state: 256 | unmanaged: 56 | coverage: 82%
suppressed: 9 (built-in: 7, user rules: 2)

REVIEW (2) - created by a person
  disk-test-old      compute/disks             rg-app  alice@contoso.com
  rg-legacy-billing  resources/resourcegroups  -       bob@contoso.com

LIKELY_OTHER_IAC (1) - created by a service principal or the platform
  st0accountlogs  storage/storageaccounts  rg-logs  11111111-2222-3333-4444-555555555555

UNKNOWN (1) - no creation event in the last 90 days
  vnet-old  network/virtualnetworks  rg-net  -

SUPPRESSED (1)
  aks-pool-disk  compute/disks  mc_aks_eastus  rule: MC_*
`
	if b.String() != want {
		t.Errorf("text output mismatch\ngot:\n%s\nwant:\n%s", b.String(), want)
	}

	var empty strings.Builder
	printText(&empty, Output{Summary: summarize(2, 2, []SuppressedResource{{builtIn: true}, {builtIn: true}}), Resources: []UnmanagedResource{}})
	if want := "Scanned 2 | in state: 0 | unmanaged: 2 | coverage: 0%\nsuppressed: 2 (built-in: 2, user rules: 0)\n\nNothing left to review after suppression.\n"; empty.String() != want {
		t.Errorf("empty output:\ngot:\n%s\nwant:\n%s", empty.String(), want)
	}
}

func TestVersionString(t *testing.T) {
	if v := versionString(); !strings.HasPrefix(v, "tfstray ") || strings.Contains(v, "commit ,") {
		t.Errorf("versionString() = %q", v)
	}
}
