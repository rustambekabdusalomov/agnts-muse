package msp

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// schema is the MSP JSON Schema exported by `muse schema
// generate-json-schema` from Muse Code 1.4.13.
type schema struct {
	Methods       map[string]json.RawMessage `json:"methods"`
	Notifications map[string]json.RawMessage `json:"notifications"`
	Requests      map[string]json.RawMessage `json:"requests"`
	Defs          map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"$defs"`
}

func loadSchema(t *testing.T) schema {
	t.Helper()
	b, err := os.ReadFile("../testdata/msp.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTypesMatchSchema checks every hand-written type against the schema
// definition it mirrors: each JSON field must exist there, and a type the
// client sends must carry every field the schema requires.
func TestTypesMatchSchema(t *testing.T) {
	s := loadSchema(t)
	for _, tc := range []struct {
		v    any
		def  string
		sent bool // the client sends it: required fields must be present
	}{
		{InitializeParams{}, "InitializeParams", true},
		{ClientInfo{}, "ClientInfo", true},
		{InitializeResult{}, "InitializeResult", false},
		{SchemaInfo{}, "SchemaInfo", false},
		{ServerInfo{}, "ServerInfo", false},
		{ModelCost{}, "ModelCost", false},
		{ModelEntry{}, "ModelCatalogEntry", false},
		{ModelListResult{}, "ModelListResult", false},
		{SessionStartParams{}, "SessionStartParams", true},
		{SessionResumeParams{}, "SessionResumeParams", true},
		{Session{}, "Session", false},
		{SessionResult{}, "SessionStartResult", false},
		{ModelSelection{}, "ModelSelection", true},
		{SessionSetModelParams{}, "SessionSetModelParams", true},
		{SessionLeaveParams{}, "SessionLeaveParams", true},
		{InputPart{}, "TurnInputPart", true},
		{TurnStartParams{}, "TurnStartParams", true},
		{TurnStartResult{}, "TurnStartResult", false},
		{TurnSteerParams{}, "TurnSteerParams", true},
		{TurnInterruptParams{}, "TurnInterruptParams", true},
		{CommandAck{}, "TurnInterruptResult", false},
		{RequirementRef{}, "ApprovalRequirementRef", true},
		{ApprovalDecideParams{}, "ApprovalDecideParams", true},
		{TokenCounts{}, "TokenUsage", false},
		{Cost{}, "CumulativeCost", false},
		{CumulativeUsage{}, "CumulativeTokenUsage", false},
		{TokenUsage{}, "SessionTokenUsageParams", false},
		{Quota{}, "QuotaDetail", false},
		{TurnError{}, "TurnError", false},
		{TurnStarted{}, "TurnStartedParams", false},
		{TurnCompleted{}, "TurnCompletedParams", false},
		{Item{}, "Item", false},
		{ItemEvent{}, "ItemCompletedParams", false},
		{ItemDelta{}, "ItemDeltaParams", false},
		{ApprovalChoice{}, "ApprovalChoice", false},
		{ApprovalSubject{}, "ApprovalSubject", false},
		{ApprovalRequested{}, "ApprovalRequestParams", false},
		{ApprovalResolved{}, "ApprovalResolvedParams", false},
		{UsageWindow{}, "SubscriptionUsageWindow", false},
		{SubscriptionUsage{}, "SubscriptionUsage", false},
		{UsageReadResult{}, "UsageReadResult", false},
		{RetryScheduled{}, "TurnRetryScheduledParams", false},
	} {
		typ := reflect.TypeOf(tc.v)
		def, ok := s.Defs[tc.def]
		if !ok {
			t.Errorf("%s: schema has no definition %s", typ.Name(), tc.def)
			continue
		}
		var fields []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			fields = append(fields, name)
			if _, ok := def.Properties[name]; !ok {
				t.Errorf("%s.%s: %s has no property %q", typ.Name(), f.Name, tc.def, name)
			}
		}
		if tc.sent {
			for _, req := range def.Required {
				if !slices.Contains(fields, req) {
					t.Errorf("%s: missing required field %q of %s", typ.Name(), req, tc.def)
				}
			}
		}
	}
}

func TestMethodNamesExist(t *testing.T) {
	s := loadSchema(t)
	for _, m := range []string{MethodInitialize, MethodModelList, MethodSessionStart, MethodSessionResume, MethodSessionSetModel,
		MethodSessionLeave, MethodTurnStart, MethodTurnSteer, MethodTurnInterrupt, MethodTurnCancel, MethodApprovalDecide, MethodUsageRead} {
		if _, ok := s.Methods[m]; !ok {
			t.Errorf("method %q is not in the schema", m)
		}
	}
	for _, n := range []string{NoteTokenUsage, NoteContextUsage, NoteSessionClosed, NoteTurnStarted, NoteTurnCompleted, NoteTurnRetry,
		NoteTurnModelRetry, NoteItemStarted, NoteItemDelta, NoteItemCompleted, NoteApprovalRequested, NoteApprovalResolved,
		NoteUsageChanged, NoteModelRouteUnserved, NotificationInitialized} {
		if _, ok := s.Notifications[n]; !ok && n != NotificationInitialized {
			t.Errorf("notification %q is not in the schema", n)
		}
	}
	for _, r := range []string{RequestApproval, RequestUserInput} {
		if _, ok := s.Requests[r]; !ok {
			t.Errorf("request %q is not in the schema", r)
		}
	}
}

func TestFingerprintMatchesSnapshot(t *testing.T) {
	b, err := os.ReadFile("../testdata/msp.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Fingerprint   string `json:"fingerprint"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Fingerprint != SchemaFingerprint || m.SchemaVersion != SchemaVersion {
		t.Fatalf("snapshot %+v, code expects %s v%d: regenerate types after `muse schema generate-json-schema`", m, SchemaFingerprint, SchemaVersion)
	}
}
