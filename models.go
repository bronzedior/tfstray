package main

import "time"

type Resource struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	ResourceGroup string            `json:"resource_group"`
	Location      string            `json:"location"`
	Tags          map[string]string `json:"tags,omitempty"`
}

type UnmanagedResource struct {
	Resource
	Bucket     string     `json:"bucket"`
	Caller     string     `json:"caller,omitempty"`
	CallerType string     `json:"caller_type,omitempty"`
	EventTime  *time.Time `json:"event_time,omitempty"`
}

type SuppressedResource struct {
	Resource
	Rule    string `json:"rule"`
	builtIn bool
}

type SuppressedCount struct {
	Total   int `json:"total"`
	BuiltIn int `json:"built_in"`
	User    int `json:"user"`
}

type Summary struct {
	Scanned    int             `json:"scanned"`
	InState    int             `json:"in_state"`
	Unmanaged  int             `json:"unmanaged"`
	Coverage   float64         `json:"coverage"`
	Suppressed SuppressedCount `json:"suppressed"`
}

type Output struct {
	SchemaVersion int                  `json:"schema_version"`
	Subscription  string               `json:"subscription"`
	Summary       Summary              `json:"summary"`
	Resources     []UnmanagedResource  `json:"resources"`
	Suppressed    []SuppressedResource `json:"suppressed_resources,omitempty"`
}

const (
	BucketReview         = "REVIEW"
	BucketLikelyOtherIaC = "LIKELY_OTHER_IAC"
	BucketUnknown        = "UNKNOWN"
	BucketNotAttempted   = "NOT_ATTEMPTED"
)

const (
	CallerTypePerson = "person"
	CallerTypeSP     = "service_principal"
)
